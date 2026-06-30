package dcg

// Game is the higher-level, turn-grouped view built from a container's record
// stream. Turns are delimited by replay action markers: a "Pregame" turn holds
// everything before the initial flop, then "Initial Flop", then one turn per
// combat round (delimited by LaneStart), mirroring the in-game flow
// spellcasting -> combat -> after-combat -> shopping.
//
// The output centers on three things: $.handoff (the players, with personas),
// $.objects (an index of every object id -> card + owner), and per-turn traces
// (the decoded CTraceMsg_* event log). Board states come in two network-group
// perspectives, but only one carries the trace-event stream, so no de-duping is
// needed.

import "sort"

// Game is the assembled replay.
type Game struct {
	File             string                `json:"file"`
	Version          string                `json:"version"`
	MatchID          uint32                `json:"matchId"`
	ContainerDecoded bool                  `json:"containerDecoded"`
	Note             string                `json:"note,omitempty"`
	Handoff          *HandoffInfo          `json:"handoff,omitempty"`
	Decks            []DeckInfo            `json:"decks,omitempty"`     // reconstructed card lists
	DeckCodes        []string              `json:"deckCodes,omitempty"` // raw base64 deck codes
	Objects          map[string]*ObjectRef `json:"objects,omitempty"`
	// Zones lists the board zone/container names recovered from a 1.0 entity
	// tree (Deck, Hand, Shop, Graveyard, …).
	Zones        []string       `json:"zones,omitempty"`
	RecordCounts map[string]int `json:"recordCounts"`
	Turns        []*Turn        `json:"turns"`
}

// HandoffInfo summarizes the match and its players (roster joined with personas).
type HandoffInfo struct {
	PlayerCount uint32       `json:"playerCount"`
	GameMode    uint32       `json:"gameMode"`
	IsSpectator bool         `json:"isSpectator"`
	IsDraft     bool         `json:"isDraft"`
	Players     []PlayerInfo `json:"players,omitempty"`
}

// ObjectRef is an entry in the object index.
type ObjectRef struct {
	CardID   uint32  `json:"cardId"`
	CardName string  `json:"cardName,omitempty"`
	Owner    *uint32 `json:"owner,omitempty"`
}

// Turn groups the events that happened between two turn boundaries.
type Turn struct {
	Index       int      `json:"index"`
	Label       string   `json:"label"`
	Markers     []string `json:"markers,omitempty"`
	BoardStates int      `json:"boardStateCount"`
	Traces      []Trace  `json:"traces,omitempty"`
}

// BuildOptions tunes Game assembly.
type BuildOptions struct {
	IncludeTraces bool // include the per-turn decoded trace log
	// Deprecated aliases kept for CLI compatibility.
	IncludePayloads bool
	IncludeChat     bool
	IncludeObjects  bool
	IncludeBoard    bool
}

// BuildGame turns a parsed container into the turn-grouped Game model.
func BuildGame(file string, r *Reader, opt BuildOptions) *Game {
	includeTraces := opt.IncludeTraces || opt.IncludeBoard || opt.IncludeObjects
	g := &Game{
		File:             file,
		Version:          r.Version.String(),
		MatchID:          r.MatchID,
		ContainerDecoded: r.ContainerDecoded,
		Note:             r.Note,
		RecordCounts:     map[string]int{},
		Objects:          map[string]*ObjectRef{},
	}

	objects := map[uint64]*ObjectRef{}
	var zoneSet []string
	personas := map[uint32]string{}
	var handoff *Handoff

	round := 0
	cur := &Turn{Index: 0, Label: "Pregame"}
	turns := []*Turn{cur}
	newTurn := func(label string) {
		t := &Turn{Index: len(turns), Label: label}
		turns = append(turns, t)
		cur = t
	}

	for _, rec := range r.Records {
		g.RecordCounts[MsgName(rec.Type)]++
		switch rec.Type {
		case MsgReplayActionMark:
			act := DecodeActionMarker(rec.Payload)
			switch act {
			case 13: // InitialFlopStart
				newTurn("Initial Flop")
			case 8: // LaneStart -> new combat round
				round++
				newTurn(roundLabel(round))
			}
			cur.Markers = append(cur.Markers, ActionName(act))
		case MsgBoardGameState:
			bs, _ := DecodeBoardState(rec.Payload)
			if r.Version == VersionClassic {
				// 1.0 board states are a field-path entity tree; recover the
				// object ids, inline cards, and zone names.
				cobjs, zones := DecodeClassicState(bs.Payload)
				for _, co := range cobjs {
					ref := &ObjectRef{}
					if co.Card != nil {
						ref.CardID = co.Card.TemplateCardID
						ref.CardName = CardName(co.Card.TemplateCardID)
						if co.Card.OwnerID != nil {
							o := *co.Card.OwnerID
							ref.Owner = &o
						}
					}
					if _, ok := objects[co.ObjectID]; !ok {
						objects[co.ObjectID] = ref
					}
				}
				zoneSet = appendUnique(zoneSet, zones)
			}
			for _, ob := range ExtractObjects(bs.Payload, false) {
				ref := &ObjectRef{CardID: ob.TemplateCardID, CardName: CardName(ob.TemplateCardID)}
				if ob.OwnerID != nil {
					o := *ob.OwnerID
					ref.Owner = &o
				}
				objects[ob.ObjectID] = ref
			}
			cur.BoardStates++
			if includeTraces {
				for _, tr := range ExtractTraces(bs.Payload) {
					if !tr.IsParsed() {
						continue
					}
					tr.Type = tr.DisplayType()
					cur.Traces = append(cur.Traces, tr)
				}
			}
		case MsgHandoff:
			h := DecodeHandoff(rec.Payload)
			handoff = &h
		case MsgVenchatoquist:
			if c := DecodeVenchat(rec.Payload); c.UserID != 0 && c.PersonaName != "" {
				personas[c.UserID] = c.PersonaName
			}
		}
	}

	g.Handoff = buildHandoffInfo(handoff, personas)
	g.Decks = reconstructDecks(r)
	for _, c := range scavengeDeckCodes(r.buf) {
		g.DeckCodes = append(g.DeckCodes, c.DeckData)
	}
	for id, ref := range objects {
		g.Objects[utoa(id)] = ref
	}
	g.Zones = zoneSet
	g.Turns = turns
	return g
}

// appendUnique appends the items of add not already in base.
func appendUnique(base, add []string) []string {
	for _, s := range add {
		found := false
		for _, b := range base {
			if b == s {
				found = true
				break
			}
		}
		if !found {
			base = append(base, s)
		}
	}
	return base
}

// buildHandoffInfo joins the handoff roster with persona names from chat.
func buildHandoffInfo(h *Handoff, personas map[uint32]string) *HandoffInfo {
	if h == nil {
		return nil
	}
	info := &HandoffInfo{
		PlayerCount: h.PlayerCount,
		GameMode:    h.GameMode,
		IsSpectator: h.IsSpectator,
	}
	for _, p := range h.Players {
		info.Players = append(info.Players, PlayerInfo{
			UserID: p.UserID, PlayerIndex: p.PlayerIndex, Persona: personas[p.UserID],
		})
	}
	sort.Slice(info.Players, func(i, j int) bool {
		return info.Players[i].PlayerIndex < info.Players[j].PlayerIndex
	})
	return info
}

func roundLabel(n int) string { return "Round " + itoa(n) }

func groupKey(g uint32) string {
	switch g {
	case 1:
		return "player1"
	case 2:
		return "player2"
	default:
		return "group" + itoa(int(g))
	}
}

func utoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
