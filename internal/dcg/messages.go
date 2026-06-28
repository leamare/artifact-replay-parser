package dcg

// Typed decoders for the EDCGGameMessages we care about, plus the enum names
// used throughout output. Field numbers come from protos/artifact/*.proto.

// EDCGGameMessages ids.
const (
	MsgBoardGameCommand  = 1
	MsgBoardGameState    = 2
	MsgBoardGameButton   = 3
	MsgRequestHandoff    = 4
	MsgHandoff           = 5
	MsgReplayActionMark  = 6
	MsgClientChat        = 7
	MsgServerChat        = 8
	MsgPauseTimer        = 9
	MsgConnectionStatus  = 10
	MsgGiveOpponentTime  = 11
	MsgInitialGameState  = 12
	MsgGameCompleted     = 13
	MsgBoardGameStateAck = 14
	MsgGoodbye           = 15
	MsgTutorialEvent     = 1000
	MsgVenchatoquist     = 1001
)

// MsgName returns the EDCGGameMessages name for a record type.
func MsgName(t int) string {
	switch t {
	case MsgBoardGameCommand:
		return "BoardGameCommand"
	case MsgBoardGameState:
		return "BoardGameState"
	case MsgBoardGameButton:
		return "BoardGameButton"
	case MsgRequestHandoff:
		return "PlayerRequestHandoff"
	case MsgHandoff:
		return "Handoff"
	case MsgReplayActionMark:
		return "ReplayActionMarker"
	case MsgClientChat:
		return "ClientToServerChatMsg"
	case MsgServerChat:
		return "ServerToClientChatMsg"
	case MsgPauseTimer:
		return "PauseTimer"
	case MsgConnectionStatus:
		return "ConnectionStatus"
	case MsgGiveOpponentTime:
		return "GiveOpponentTime"
	case MsgInitialGameState:
		return "InitialGameState"
	case MsgGameCompleted:
		return "GameCompleted"
	case MsgBoardGameStateAck:
		return "BoardGameStateAck"
	case MsgGoodbye:
		return "Goodbye"
	case MsgTutorialEvent:
		return "TutorialEvent"
	case MsgVenchatoquist:
		return "VenchatoquistEvent"
	default:
		return "Unknown"
	}
}

// ActionType is CMsgGameReplayActionMarker.EActionType.
type ActionType int

// ActionName maps the marker action enum (1.0 + 2.0 supersets).
func ActionName(a int) string {
	switch a {
	case 1:
		return "TurnStart"
	case 2:
		return "TurnEnd"
	case 3:
		return "ShoppingStart"
	case 4:
		return "ShoppingEnd"
	case 5:
		return "HeroPlacementStart"
	case 6:
		return "HeroPlacementEnd"
	case 7:
		return "MatchComplete"
	case 8:
		return "LaneStart"
	case 9:
		return "LaneEnd"
	case 10:
		return "LaneCombat"
	case 11:
		return "PlayerAction"
	case 12:
		return "TowerDestroyed" // 2.0
	case 13:
		return "InitialFlopStart" // 2.0
	case 14:
		return "InitialFlopEnd" // 2.0
	default:
		return "Action?"
	}
}

// BoardState is the decoded envelope of a CMsgGameBoardGameState. The Payload
// is the opaque schema-serialized board snapshot (full when !IsDiff).
type BoardState struct {
	IsDiff       bool
	IsLast       bool
	NetworkGroup uint32
	UnpackedSize uint32
	ServerTimeMs uint32
	Payload      []byte
}

// DecodeBoardState parses the envelope fields of a type-2 record payload.
func DecodeBoardState(payload []byte) (BoardState, error) {
	var bs BoardState
	m, err := fieldMap(payload)
	if v, ok := first(m, 1); ok {
		bs.IsDiff = v.V != 0
	}
	if v, ok := first(m, 2); ok {
		bs.IsLast = v.V != 0
	}
	if v, ok := first(m, 3); ok {
		bs.Payload = v.Bytes
	}
	if v, ok := first(m, 4); ok {
		bs.UnpackedSize = uint32(v.V)
	}
	if v, ok := first(m, 5); ok {
		bs.NetworkGroup = uint32(v.V)
	}
	if v, ok := first(m, 6); ok {
		bs.ServerTimeMs = uint32(v.V)
	}
	return bs, err
}

// DecodeActionMarker returns the action enum from a type-6 record.
func DecodeActionMarker(payload []byte) int {
	m, _ := fieldMap(payload)
	if v, ok := first(m, 1); ok {
		return int(v.V)
	}
	return 0
}

// Handoff is the decoded CMsgGameHandoff. The live 2.0 message carries fields
// beyond the documented proto, notably a player roster in field 5.
type Handoff struct {
	PlayerCount uint32
	IsSpectator bool
	GameMode    uint32
	Players     []HandoffPlayer
}

// HandoffPlayer is one roster entry (Handoff field 5: {user_id, player_index}).
type HandoffPlayer struct {
	UserID      uint32 `json:"userId"`
	PlayerIndex uint32 `json:"playerIndex"`
}

// DecodeHandoff parses a type-5 record.
func DecodeHandoff(payload []byte) Handoff {
	var h Handoff
	m, _ := fieldMap(payload)
	if v, ok := first(m, 1); ok {
		h.PlayerCount = uint32(v.V)
	}
	if v, ok := first(m, 3); ok {
		h.IsSpectator = v.V != 0
	}
	if v, ok := first(m, 4); ok {
		h.GameMode = uint32(v.V)
	}
	for _, f := range m[5] { // repeated roster entry
		pm, _ := fieldMap(f.Bytes)
		var p HandoffPlayer
		if u, ok := first(pm, 1); ok {
			p.UserID = uint32(u.V)
		}
		if idx, ok := first(pm, 2); ok {
			p.PlayerIndex = uint32(idx.V)
		}
		h.Players = append(h.Players, p)
	}
	return h
}

// Venchat is the decoded CMsgVenchatoquistEvent (chat / gesture line).
type Venchat struct {
	Text        string
	ObjectID    uint32
	ChatSource  uint32
	PersonaName string
	UserID      uint32
	LineID      uint32
}

// DecodeVenchat parses a venchatoquist record. 2.0 places the chat text in
// field 1; 1.0 uses field 3 (and has no user_id). We read whichever is a
// printable string.
func DecodeVenchat(payload []byte) Venchat {
	var c Venchat
	m, _ := fieldMap(payload)
	if v, ok := first(m, 1); ok && v.Wire == wireBytes {
		c.Text = string(v.Bytes)
	}
	if v, ok := first(m, 2); ok {
		c.ObjectID = uint32(v.V)
	}
	if v, ok := first(m, 3); ok {
		if v.Wire == wireBytes { // 1.0: field 3 carries the text
			if s, ok := printableString(v.Bytes); ok {
				c.Text = s
			}
		} else {
			c.ChatSource = uint32(v.V)
		}
	}
	if v, ok := first(m, 4); ok && v.Wire == wireBytes {
		c.PersonaName = string(v.Bytes)
	}
	if v, ok := first(m, 5); ok {
		c.UserID = uint32(v.V)
	}
	if v, ok := first(m, 6); ok {
		c.LineID = uint32(v.V)
	}
	return c
}

// ConnectionStatusPlayers counts player entries in a type-10 record.
func ConnectionStatusPlayers(payload []byte) int {
	m, _ := fieldMap(payload)
	return len(m[1])
}

func first(m map[int][]Field, num int) (Field, bool) {
	if fs := m[num]; len(fs) > 0 {
		return fs[0], true
	}
	return Field{}, false
}
