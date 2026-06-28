package dcg

// Game is the higher-level, turn-grouped view built from a container's record
// stream. Turns are delimited by replay action markers: a "Pregame" turn holds
// everything before the initial flop, then "Initial Flop", then one turn per
// combat round (delimited by LaneStart), mirroring the in-game flow
// spellcasting -> combat -> after-combat -> shopping.

import (
	"encoding/base64"
	"sort"
)

// Game is the assembled replay.
type Game struct {
	File             string         `json:"file"`
	Version          string         `json:"version"`
	MatchID          uint32         `json:"matchId"`
	ContainerDecoded bool           `json:"containerDecoded"`
	Note             string         `json:"note,omitempty"`
	Handoff          *Handoff       `json:"handoff,omitempty"`
	RecordCounts     map[string]int `json:"recordCounts"`
	Turns            []*Turn        `json:"turns"`
}

// Turn groups the events that happened between two turn boundaries.
type Turn struct {
	Index       int     `json:"index"`
	Label       string  `json:"label"`
	Events      []Event `json:"events"`
	BoardStates int     `json:"boardStateCount"`
	// ObjectUpdates is the number of object (card/hero/creep/tower/…) updates
	// decoded from this turn's board-state payloads.
	ObjectUpdates int `json:"objectUpdates"`
	// FinalBoard is the running board snapshot at the end of this turn: every
	// object seen so far with its latest decoded card state, sorted by id.
	FinalBoard []Object `json:"finalBoard,omitempty"`
	// FinalStateMeta is the latest board-state envelope per network group as of
	// the end of this turn (carried forward).
	FinalStateMeta map[string]*StateInfo `json:"finalStateMeta,omitempty"`
}

// Event is one thing that happened, in stream order.
type Event struct {
	Kind   string     `json:"kind"`             // marker | boardState | chat | connection | message
	Action string     `json:"action,omitempty"` // for markers
	State  *StateInfo `json:"state,omitempty"`  // for boardState
	Chat   *ChatInfo  `json:"chat,omitempty"`   // for chat
	Detail string     `json:"detail,omitempty"` // for other messages
	Offset int        `json:"offset"`
}

// StateInfo is the decoded envelope of a board-state snapshot plus the object
// updates recovered from its payload.
type StateInfo struct {
	NetworkGroup uint32   `json:"networkGroup"`
	IsDiff       bool     `json:"isDiff"`
	IsLast       bool     `json:"isLast"`
	UnpackedSize uint32   `json:"unpackedSize"`
	PayloadLen   int      `json:"payloadLen"`
	ObjectCount  int      `json:"objectCount"`
	Objects      []Object `json:"objects,omitempty"`
	ServerTimeMs uint32   `json:"serverTimeMs,omitempty"`
	PayloadB64   string   `json:"payloadBase64,omitempty"`
}

// ChatInfo is a decoded venchatoquist line.
type ChatInfo struct {
	Text        string `json:"text,omitempty"`
	ObjectID    uint32 `json:"objectId,omitempty"`
	ChatSource  uint32 `json:"chatSource,omitempty"`
	PersonaName string `json:"persona,omitempty"`
	LineID      uint32 `json:"lineId,omitempty"`
}

// BuildOptions tunes Game assembly.
type BuildOptions struct {
	IncludePayloads bool // embed base64 of raw state payloads
	IncludeChat     bool // include venchatoquist events (very chatty)
	IncludeObjects  bool // include per-state object updates in events
	IncludeBoard    bool // include the running board snapshot per turn
}

// BuildGame turns a parsed container into the turn-grouped Game model.
func BuildGame(file string, r *Reader, opt BuildOptions) *Game {
	g := &Game{
		File:             file,
		Version:          r.Version.String(),
		MatchID:          r.MatchID,
		ContainerDecoded: r.ContainerDecoded,
		Note:             r.Note,
		RecordCounts:     map[string]int{},
	}

	// Carry-forward snapshot of the latest envelope per network group, and the
	// running board: object id -> latest decoded card state.
	running := map[string]*StateInfo{}
	board := map[uint64]Object{}
	round := 0
	cur := &Turn{Index: 0, Label: "Pregame"}
	turns := []*Turn{cur}

	snapshotBoard := func() []Object {
		if !opt.IncludeBoard {
			return nil
		}
		return sortedBoard(board)
	}
	newTurn := func(label string) {
		// Snapshot running state into the turn we're closing.
		cur.FinalStateMeta = cloneStates(running)
		cur.FinalBoard = snapshotBoard()
		t := &Turn{Index: len(turns), Label: label}
		turns = append(turns, t)
		cur = t
	}

	for _, rec := range r.Records {
		g.RecordCounts[MsgName(rec.Type)]++
		switch rec.Type {
		case MsgReplayActionMark:
			act := DecodeActionMarker(rec.Payload)
			name := ActionName(act)
			switch act {
			case 13: // InitialFlopStart
				newTurn("Initial Flop")
			case 8: // LaneStart -> new combat round
				round++
				newTurn(roundLabel(round))
			}
			cur.Events = append(cur.Events, Event{Kind: "marker", Action: name, Offset: rec.Offset})
		case MsgBoardGameState:
			bs, _ := DecodeBoardState(rec.Payload)
			objs := ExtractObjects(bs.Payload, opt.IncludePayloads)
			si := &StateInfo{
				NetworkGroup: bs.NetworkGroup,
				IsDiff:       bs.IsDiff,
				IsLast:       bs.IsLast,
				UnpackedSize: bs.UnpackedSize,
				PayloadLen:   len(bs.Payload),
				ObjectCount:  len(objs),
				ServerTimeMs: bs.ServerTimeMs,
			}
			if opt.IncludeObjects {
				si.Objects = objs
			}
			if opt.IncludePayloads {
				si.PayloadB64 = base64.StdEncoding.EncodeToString(bs.Payload)
			}
			// Apply object updates to the running board (group 1 carries the
			// full perspective; both groups update the same object ids).
			for _, ob := range objs {
				board[ob.ObjectID] = ob
			}
			running[groupKey(bs.NetworkGroup)] = si
			cur.BoardStates++
			cur.ObjectUpdates += len(objs)
			cur.Events = append(cur.Events, Event{Kind: "boardState", State: si, Offset: rec.Offset})
		case MsgHandoff:
			h := DecodeHandoff(rec.Payload)
			g.Handoff = &h
			cur.Events = append(cur.Events, Event{Kind: "message", Detail: "Handoff", Offset: rec.Offset})
		case MsgConnectionStatus:
			cur.Events = append(cur.Events, Event{Kind: "connection", Offset: rec.Offset})
		case MsgVenchatoquist:
			if opt.IncludeChat {
				c := DecodeVenchat(rec.Payload)
				cur.Events = append(cur.Events, Event{Kind: "chat", Chat: &ChatInfo{
					Text: c.Text, ObjectID: c.ObjectID, ChatSource: c.ChatSource,
					PersonaName: c.PersonaName, LineID: c.LineID,
				}, Offset: rec.Offset})
			}
		default:
			cur.Events = append(cur.Events, Event{Kind: "message", Detail: MsgName(rec.Type), Offset: rec.Offset})
		}
	}
	cur.FinalStateMeta = cloneStates(running)
	cur.FinalBoard = snapshotBoard()
	g.Turns = turns
	return g
}

// sortedBoard returns the board objects sorted by object id for stable output.
func sortedBoard(board map[uint64]Object) []Object {
	out := make([]Object, 0, len(board))
	for _, o := range board {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out
}

func roundLabel(n int) string {
	return "Round " + itoa(n)
}

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

func cloneStates(m map[string]*StateInfo) map[string]*StateInfo {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*StateInfo, len(m))
	for k, v := range m {
		cp := *v
		cp.PayloadB64 = "" // don't duplicate large payloads into the snapshot
		cp.Objects = nil   // objects live on the per-state event, not the meta
		out[k] = &cp
	}
	return out
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
