package dcg

// Exploratory dumps: a demo-info summary and a per-packet JSONL stream. These
// are the primary tools for reverse-engineering message contents — every
// record's payload is decoded generically so unknown fields are still visible.

import (
	"encoding/json"
	"io"
	"sort"
	"time"
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
// For 1.0, traces come from the whole-file scan (delta gaps included) and are
// interleaved with the packets by file offset.
func WritePacketsJSONL(w io.Writer, r *Reader) error {
	enc := json.NewEncoder(w)
	if r.Version == VersionClassic {
		return writeClassicPacketsJSONL(enc, r)
	}
	for i, rec := range r.Records {
		if rec.Type == MsgBoardGameState {
			bs, _ := DecodeBoardState(rec.Payload)
			for _, tr := range TracesFor(r.Version, bs.Payload) {
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

// writeClassicPacketsJSONL interleaves 1.0 records and whole-file traces by
// their byte offsets, so the JSONL preserves game order.
func writeClassicPacketsJSONL(enc *json.Encoder, r *Reader) error {
	traces := ExtractClassicFileTraces(r.buf)
	ti := 0
	emitTrace := func(pt PosTrace) error {
		return enc.Encode(TraceLine{
			Message:  "trace",
			Type:     pt.Trace.DisplayType(),
			ObjectID: pt.Trace.ObjectID,
			TypeID:   pt.Trace.TypeID,
			Data:     pt.Trace.Fields,
		})
	}
	for i, rec := range r.Records {
		for ti < len(traces) && traces[ti].Offset < rec.Offset {
			if err := emitTrace(traces[ti]); err != nil {
				return err
			}
			ti++
		}
		if rec.Type == MsgBoardGameState {
			continue // its traces are covered by the whole-file scan
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
	for ; ti < len(traces); ti++ {
		if err := emitTrace(traces[ti]); err != nil {
			return err
		}
	}
	return nil
}

// DemoInfo is the high-level summary (the CDemoFileInfo / CGameInfo analogue).
// Note: these replays are GC message logs, not Source 2 DEM containers — there
// is no CDemoFileHeader, and no match date, server name, network protocol or
// end time anywhere in the file (verified by token and timestamp scans). The
// closest header data is the container version word and (1.0) checksum.
type DemoInfo struct {
	File             string         `json:"file"`
	Version          string         `json:"version"`
	ContainerVersion uint32         `json:"containerVersion"`
	CRC              uint32         `json:"crc,omitempty"` // 1.0 header checksum
	MatchID          uint32         `json:"matchId"`
	GameMode         *uint32        `json:"gameMode,omitempty"`
	PlayerCount      *uint32        `json:"playerCount,omitempty"`
	IsSpectator      *bool          `json:"isSpectator,omitempty"`
	MatchTime        uint32         `json:"matchTime,omitempty"` // unix, from chat timestamps
	MatchDate        string         `json:"matchDate,omitempty"` // ISO form of MatchTime
	Players          []PlayerInfo   `json:"players,omitempty"`
	Decks            []DeckInfo     `json:"decks,omitempty"`            // reconstructed card lists
	DeckCodes        []string       `json:"deckCodes,omitempty"`        // full deck codes (repaired)
	PartialDeckCodes []string       `json:"partialDeckCodes,omitempty"` // current-deck codes (no heroes post-flop)
	GameData         *GameData      `json:"gameData,omitempty"`         // match/game mode, gauntlet
	RecordCounts     map[string]int `json:"recordCounts"`
	RecordTotal      int            `json:"recordTotal"`
	Note             string         `json:"note,omitempty"`
}

// PlayerInfo is a participant: the Handoff roster joined with the persona name
// seen in chat/venchatoquist records. Bot marks AI participants (their roster
// user id is a placeholder, not a real account id).
type PlayerInfo struct {
	UserID      uint32 `json:"userId"`
	PlayerIndex uint32 `json:"playerIndex"`
	Persona     string `json:"persona,omitempty"`
	Bot         bool   `json:"bot,omitempty"`
}

// botUserIDMax bounds placeholder user ids assigned to AI players.
const botUserIDMax = 1000

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
		File:             file,
		Version:          r.Version.String(),
		ContainerVersion: r.ContainerVersion,
		CRC:              r.CRC,
		MatchID:          r.MatchID,
		RecordCounts:     map[string]int{},
		RecordTotal:      len(r.Records),
		Note:             r.Note,
	}
	personas := ScavengePersonas(r.buf)
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
			if ts := venchatTimestamp(uint64(c.LineID)); ts != 0 && (di.MatchTime == 0 || ts < di.MatchTime) {
				di.MatchTime = ts
			}
		case MsgInitialGameState:
			di.Decks = append(di.Decks, decodeDecks(rec.Payload)...)
		}
	}
	// Chat lines carry wall-clock stamps in the 1.0 delta stream too; scan for
	// the earliest one as the match time.
	if ts := scanChatTimestamps(r.buf); ts != 0 && (di.MatchTime == 0 || ts < di.MatchTime) {
		di.MatchTime = ts
	}
	if di.MatchTime != 0 {
		di.MatchDate = time.Unix(int64(di.MatchTime), 0).UTC().Format("2006-01-02 15:04:05 UTC")
	}
	// Decks: the reconstructed card lists (reliable) are the primary deck data.
	// Deck codes come from the decklist fields of the game-data entity (token
	// scan across board states), with the raw byte scavenge as a fallback for
	// codes the entity parse misses.
	if len(di.Decks) == 0 {
		di.Decks = reconstructDecks(r)
	}
	seenCode := map[string]bool{}
	add := func(dst *[]string, c string) {
		if c != "" && !seenCode[c] {
			seenCode[c] = true
			*dst = append(*dst, c)
		}
	}
	for _, rec := range r.Records {
		if rec.Type != MsgBoardGameState {
			continue
		}
		bs, _ := DecodeBoardState(rec.Payload)
		current, full := ExtractDeckCodes(bs.Payload)
		for _, c := range full {
			add(&di.DeckCodes, c)
		}
		for _, c := range current {
			add(&di.PartialDeckCodes, c)
		}
		if di.GameData == nil {
			if gd := ExtractGameData(bs.Payload); gd.MatchMode != nil || gd.GameMode != nil || gd.GauntletID != nil {
				di.GameData = &gd
			}
		}
	}
	if r.Version == VersionClassic {
		// The 1.0 delta stream holds codes outside decodable board states; the
		// full/current distinction isn't recoverable there, so keep them all as
		// deck codes.
		for _, c := range scavengeDeckCodes(r.buf) {
			add(&di.DeckCodes, RepairDeckCode(c.DeckData))
		}
	}
	seen := map[uint32]bool{}
	for uid, idx := range roster {
		di.Players = append(di.Players, PlayerInfo{
			UserID: uid, PlayerIndex: idx, Persona: personas[uid],
			Bot: uid < botUserIDMax,
		})
		seen[uid] = true
	}
	for uid, name := range personas { // personas with no roster entry (1.0 has no roster)
		if !seen[uid] {
			di.Players = append(di.Players, PlayerInfo{UserID: uid, Persona: name})
		}
	}
	sort.Slice(di.Players, func(i, j int) bool { return di.Players[i].PlayerIndex < di.Players[j].PlayerIndex })
	return di
}

// scanChatTimestamps finds the earliest chat wall-clock stamp in the raw
// stream: venchat field 6 (tag 0x30) followed by a 5-byte varint unix time.
func scanChatTimestamps(buf []byte) uint32 {
	var best uint32
	for i := 0; i+6 < len(buf); i++ {
		if buf[i] != 0x30 {
			continue
		}
		v, n := uvarint(buf, i+1)
		if n-i-1 != 5 {
			continue
		}
		if ts := venchatTimestamp(v); ts != 0 && (best == 0 || ts < best) {
			best = ts
		}
	}
	return best
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
		// Foundry). The delta serializer can elide the constant prefix, leaving
		// a '=='-padded tail; accept those too (RepairDeckCode reconstructs).
		run := buf[i:j]
		if j-i >= minDeckLen && (hasDeckPrefix(run) || (run[len(run)-1] == '=' && j-i >= 50)) {
			code := string(run)
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
	var all []Trace
	if r.Version == VersionClassic {
		for _, pt := range ExtractClassicFileTraces(r.buf) {
			all = append(all, pt.Trace)
		}
	} else {
		for _, rec := range r.Records {
			if rec.Type != MsgBoardGameState {
				continue
			}
			bs, _ := DecodeBoardState(rec.Payload)
			all = append(all, ExtractTraces(bs.Payload)...)
		}
	}
	{
		for _, tr := range all {
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
