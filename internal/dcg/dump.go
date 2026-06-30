package dcg

// Exploratory dumps: a demo-info summary and a per-packet JSONL stream. These
// are the primary tools for reverse-engineering message contents — every
// record's payload is decoded generically so unknown fields are still visible.

import (
	"encoding/json"
	"io"
	"sort"
)

// Packet is one non-board-state record rendered for the JSONL dump.
type Packet struct {
	Message string         `json:"message"` // "packet"
	Type    string         `json:"type"`    // EDCGGameMessages name
	MsgID   int            `json:"msgId"`
	Index   int            `json:"index"`
	Offset  int            `json:"offset"`
	Data    map[string]any `json:"data"`
}

// TraceLine is one trace event extracted from a board-state payload. Board
// states are exploded into their constituent traces here (the board-state
// record itself is not emitted), since the traces are the meaningful content.
type TraceLine struct {
	Message  string         `json:"message"` // "trace"
	Type     string         `json:"type"`    // CTraceMsg name, "UnknownTrace", or "NotParsed"
	Group    uint32         `json:"group,omitempty"`
	IsDiff   bool           `json:"isDiff,omitempty"`
	ObjectID uint64         `json:"objectId,omitempty"`
	TypeID   uint64         `json:"typeId,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

// WritePacketsJSONL writes one JSON object per line: a "packet" per
// non-board-state record, and a "trace" per trace inside each board state.
func WritePacketsJSONL(w io.Writer, r *Reader) error {
	enc := json.NewEncoder(w)
	for i, rec := range r.Records {
		if rec.Type == MsgBoardGameState {
			bs, _ := DecodeBoardState(rec.Payload)
			for _, tr := range ExtractTraces(bs.Payload) {
				line := TraceLine{
					Message:  "trace",
					Type:     tr.DisplayType(),
					Group:    bs.NetworkGroup,
					IsDiff:   bs.IsDiff,
					ObjectID: tr.ObjectID,
					TypeID:   tr.TypeID,
					Data:     tr.Fields,
				}
				if err := enc.Encode(line); err != nil {
					return err
				}
			}
			continue
		}
		p := Packet{
			Message: "packet",
			Type:    MsgName(rec.Type),
			MsgID:   rec.Type,
			Index:   i,
			Offset:  rec.Offset,
			Data:    TypedPacketData(rec),
		}
		if rec.Scavenged {
			p.Data["_scavenged"] = true
		}
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	return nil
}

// DemoInfo is the high-level summary (the CDemoFileInfo / CGameInfo analogue).
type DemoInfo struct {
	File         string         `json:"file"`
	Version      string         `json:"version"`
	MatchID      uint32         `json:"matchId"`
	GameMode     *uint32        `json:"gameMode,omitempty"`
	PlayerCount  *uint32        `json:"playerCount,omitempty"`
	IsSpectator  *bool          `json:"isSpectator,omitempty"`
	Players      []PlayerInfo   `json:"players,omitempty"`
	Decks        []DeckInfo     `json:"decks,omitempty"`     // reconstructed card lists
	DeckCodes    []string       `json:"deckCodes,omitempty"` // raw base64 deck codes
	RecordCounts map[string]int `json:"recordCounts"`
	RecordTotal  int            `json:"recordTotal"`
	Note         string         `json:"note,omitempty"`
}

// PlayerInfo is a participant: the Handoff roster joined with the persona name
// seen in chat/venchatoquist records.
type PlayerInfo struct {
	UserID      uint32 `json:"userId"`
	PlayerIndex uint32 `json:"playerIndex"`
	Persona     string `json:"persona,omitempty"`
}

// DeckInfo is a player's deck, recovered as a base64 deck code when present and
// reconstructed as a card list from the card-create events.
type DeckInfo struct {
	OwnerID  uint32     `json:"ownerId"`
	DeckData string     `json:"deckData,omitempty"`
	Cards    []DeckCard `json:"cards,omitempty"`
}

// DeckCard is one card in a reconstructed deck.
type DeckCard struct {
	CardID   uint32 `json:"cardId"`
	CardName string `json:"cardName,omitempty"`
	Count    int    `json:"count"`
}

// BuildDemoInfo assembles the summary from a parsed container.
func BuildDemoInfo(file string, r *Reader) *DemoInfo {
	di := &DemoInfo{
		File:         file,
		Version:      r.Version.String(),
		MatchID:      r.MatchID,
		RecordCounts: map[string]int{},
		RecordTotal:  len(r.Records),
		Note:         r.Note,
	}
	personas := map[uint32]string{}
	roster := map[uint32]uint32{} // user_id -> player_index
	for _, rec := range r.Records {
		di.RecordCounts[MsgName(rec.Type)]++
		switch rec.Type {
		case MsgHandoff:
			h := DecodeHandoff(rec.Payload)
			pc, gm, sp := h.PlayerCount, h.GameMode, h.IsSpectator
			di.PlayerCount, di.GameMode, di.IsSpectator = &pc, &gm, &sp
			for _, p := range h.Players {
				roster[p.UserID] = p.PlayerIndex
			}
		case MsgVenchatoquist:
			c := DecodeVenchat(rec.Payload)
			if c.UserID != 0 && c.PersonaName != "" {
				personas[c.UserID] = c.PersonaName
			}
		case MsgInitialGameState:
			di.Decks = append(di.Decks, decodeDecks(rec.Payload)...)
		}
	}
	// Decks: the reconstructed card lists (reliable) are the primary deck data;
	// the raw base64 deck codes are surfaced separately as supplementary.
	if len(di.Decks) == 0 {
		di.Decks = reconstructDecks(r)
	}
	for _, c := range scavengeDeckCodes(r.buf) {
		di.DeckCodes = append(di.DeckCodes, c.DeckData)
	}
	seen := map[uint32]bool{}
	for uid, idx := range roster {
		di.Players = append(di.Players, PlayerInfo{UserID: uid, PlayerIndex: idx, Persona: personas[uid]})
		seen[uid] = true
	}
	for uid, name := range personas { // personas with no roster entry (spectators)
		if !seen[uid] {
			di.Players = append(di.Players, PlayerInfo{UserID: uid, Persona: name})
		}
	}
	sort.Slice(di.Players, func(i, j int) bool { return di.Players[i].PlayerIndex < di.Players[j].PlayerIndex })
	return di
}

// scavengeDeckCodes recovers Artifact deck codes from a 1.0 file. They are long
// base64 strings (the only such runs in the stream), so we extract every
// maximal base64 run of at least minDeckLen characters without decoding the
// surrounding entity serialization.
func scavengeDeckCodes(buf []byte) []DeckInfo {
	const minDeckLen = 40
	var decks []DeckInfo
	seen := map[string]bool{}
	i := 0
	for i < len(buf) {
		if !isBase64Char(buf[i]) {
			i++
			continue
		}
		j := i
		for j < len(buf) && isBase64Char(buf[j]) {
			j++
		}
		// Deck codes start with a version prefix ("ADC" in 1.0, "RTFACT" in 2.0
		// Foundry); this filters out base64-looking fragments the serialization
		// splits out.
		if j-i >= minDeckLen && hasDeckPrefix(buf[i:j]) {
			code := string(buf[i:j])
			if !seen[code] {
				seen[code] = true
				decks = append(decks, DeckInfo{OwnerID: uint32(len(decks)), DeckData: code})
			}
		}
		i = j
	}
	return decks
}

// hasDeckPrefix reports whether a base64 run begins with a known deck-code
// version prefix.
func hasDeckPrefix(b []byte) bool {
	for _, p := range []string{"ADC", "RTFACT"} {
		if len(b) >= len(p) && string(b[:len(p)]) == p {
			return true
		}
	}
	return false
}

func isBase64Char(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
		c == '+', c == '/', c == '=', c == '-', c == '_': // deck codes use base64url
		return true
	}
	return false
}

// minDeckCardID is the lowest template id treated as a real (deckable) card,
// excluding control objects, basic creeps and spawned tokens.
const minDeckCardID = 10000

// reconstructDecks rebuilds each owner's deck from CardCreated/CardRevealed
// traces: distinct card instances (by object id) grouped by owner, counted per
// template. Only real cards (>= minDeckCardID) are included.
func reconstructDecks(r *Reader) []DeckInfo {
	// owner -> template -> set of object ids (distinct instances)
	byOwner := map[uint32]map[uint32]map[uint64]bool{}
	for _, rec := range r.Records {
		if rec.Type != MsgBoardGameState {
			continue
		}
		bs, _ := DecodeBoardState(rec.Payload)
		for _, tr := range ExtractTraces(bs.Payload) {
			if tr.Type != "CardCreated" && tr.Type != "CardRevealed" {
				continue
			}
			tmpl, ok := toU64(tr.Fields["template_card_id"])
			if !ok || tmpl < minDeckCardID {
				continue
			}
			owner := uint32(0)
			if o, ok := toU64(tr.Fields["owner_id"]); ok {
				owner = uint32(o)
			}
			if byOwner[owner] == nil {
				byOwner[owner] = map[uint32]map[uint64]bool{}
			}
			if byOwner[owner][uint32(tmpl)] == nil {
				byOwner[owner][uint32(tmpl)] = map[uint64]bool{}
			}
			byOwner[owner][uint32(tmpl)][tr.ObjectID] = true
		}
	}
	var decks []DeckInfo
	for owner, templates := range byOwner {
		d := DeckInfo{OwnerID: owner}
		for tmpl, insts := range templates {
			d.Cards = append(d.Cards, DeckCard{CardID: tmpl, CardName: CardName(tmpl), Count: len(insts)})
		}
		sort.Slice(d.Cards, func(i, j int) bool { return d.Cards[i].CardID < d.Cards[j].CardID })
		decks = append(decks, d)
	}
	sort.Slice(decks, func(i, j int) bool { return decks[i].OwnerID < decks[j].OwnerID })
	return decks
}

// decodeDecks parses CMsgGameServerToClientInitialGameState.decklist entries.
func decodeDecks(payload []byte) []DeckInfo {
	var decks []DeckInfo
	m, _ := fieldMap(payload)
	for _, f := range m[1] { // repeated decklist
		dm, _ := fieldMap(f.Bytes)
		var d DeckInfo
		if owner, ok := first(dm, 1); ok {
			d.OwnerID = uint32(owner.V)
		}
		if data, ok := first(dm, 2); ok {
			if s, ok := printableString(data.Bytes); ok {
				d.DeckData = s
			}
		}
		decks = append(decks, d)
	}
	return decks
}
