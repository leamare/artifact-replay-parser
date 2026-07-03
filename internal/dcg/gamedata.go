package dcg

// Decoding of the "game data" entity tree. In 2.0 this is the network-group-1
// board state (the group-2 states carry the "Trace Events" entity instead); in
// 1.0 the same entity tree is interleaved in the single stream. It holds the
// match-level objects — "Card Game Data" (match/game mode, gauntlet id) and the
// per-player "Private Player Info" (decklists) — plus lanes/towers.
//
// Deck codes live under the murmur token for "decklist":
//
//	[decklist token][valtype 0x01][u16 length][base64 code]
//
// followed by a second deck field (token 0x85b82cd5, proto name unknown) with
// the same layout. The `decklist` value is the player's *current* deck (after
// hero deployment it no longer contains the heroes); the second field carries
// the full starting code. The delta serializer elides the constant "ADCE"
// prefix when a code repeats an earlier value, so 1.0 opponent codes can
// surface as a bare tail ("ZK04…=="); RepairDeckCode reconstructs those.

import "strings"

var (
	tokenDecklist = MakeStringToken("decklist")
	// tokenDeckFull is the second per-player deck field (full starting deck);
	// its source name is unknown, the hash is taken from observation.
	tokenDeckFull uint32 = 0x85b82cd5

	tokenMatchMode  = MakeStringToken("matchMode")
	tokenGameMode   = MakeStringToken("gameMode")
	tokenGauntletID = MakeStringToken("gauntletId")
)

// GameData is the match-level info recovered from the game-data entity.
type GameData struct {
	MatchMode  *uint64 `json:"matchMode,omitempty"`
	GameMode   *uint64 `json:"gameMode,omitempty"`
	GauntletID *uint64 `json:"gauntletId,omitempty"`
}

// ExtractGameData reads the match-level scalar fields from a game-data payload.
func ExtractGameData(payload []byte) GameData {
	var gd GameData
	read := func(tok uint32) *uint64 {
		j := indexFrom(payload, tokenLE(tok), 0)
		if j < 0 || j+5 >= len(payload) {
			return nil
		}
		// [token][valtype][varint]
		v, n := uvarint(payload, j+5)
		if n == 0 {
			return nil
		}
		return &v
	}
	gd.MatchMode = read(tokenMatchMode)
	gd.GameMode = read(tokenGameMode)
	gd.GauntletID = read(tokenGauntletID)
	return gd
}

// ExtractDeckCodes pulls the deck codes from a payload: current holds the
// values of the `decklist` fields (the player's live deck — post-flop it lacks
// the heroes), full holds the companion full-starting-deck fields.
func ExtractDeckCodes(payload []byte) (current, full []string) {
	read := func(tok uint32) []string {
		var out []string
		i := 0
		for {
			j := indexFrom(payload, tokenLE(tok), i)
			if j < 0 {
				break
			}
			i = j + 4
			// [token][valtype 0x01][u16 len][string]
			o := j + 5
			if o+2 > len(payload) {
				continue
			}
			n := int(leU16(payload[o:]))
			if n < 8 || n > 512 || o+2+n > len(payload) {
				continue
			}
			s := payload[o+2 : o+2+n]
			if code, ok := deckCodeString(s); ok {
				out = append(out, code)
			}
		}
		return out
	}
	return read(tokenDecklist), read(tokenDeckFull)
}

// deckCodeString validates a candidate deck-code value, repairing an elided
// prefix when needed.
func deckCodeString(b []byte) (string, bool) {
	for _, c := range b {
		if !isBase64Char(c) {
			return "", false
		}
	}
	return RepairDeckCode(string(b)), true
}

// ScavengePersonas finds player persona names embedded anywhere in the stream,
// including inside 1.0 delta regions where the venchat message isn't cleanly
// framed. It matches the venchat field pair `persona_name(4)` + `user_id(5)`:
// [0x22][len][printable name][0x28][varint user id].
func ScavengePersonas(buf []byte) map[uint32]string {
	out := map[uint32]string{}
	for i := 0; i+4 < len(buf); i++ {
		if buf[i] != 0x22 {
			continue
		}
		n := int(buf[i+1])
		if n < 2 || n > 32 || i+2+n+1 >= len(buf) {
			continue
		}
		name := buf[i+2 : i+2+n]
		if !printableASCII(name) {
			continue
		}
		o := i + 2 + n
		if buf[o] != 0x28 {
			continue
		}
		uid, no := uvarint(buf, o+1)
		if no == 0 || uid == 0 || uid > 1<<32-1 {
			continue
		}
		if _, exists := out[uint32(uid)]; !exists {
			out[uint32(uid)] = string(name)
		}
		i = no - 1
	}
	return out
}

// venchatTimestamp interprets a venchat field-6 value as a unix timestamp when
// it is in a plausible range (the 1.0 client stamps chat lines with wall time).
func venchatTimestamp(v uint64) uint32 {
	const lo, hi = 1_500_000_000, 1_700_000_000 // 2017..2023
	if v >= lo && v <= hi {
		return uint32(v)
	}
	return 0
}

// RepairDeckCode reconstructs a deck code whose constant "ADCE" prefix was
// elided by the delta serializer. A stored tail keeps the original base64
// phase (verified against a file that stores the same code in full), so we
// strip leading residue chars until prepending "ADCE" restores a length that
// is a multiple of 4.
func RepairDeckCode(code string) string {
	if strings.HasPrefix(code, "ADC") || strings.HasPrefix(code, "RTFACT") {
		return code
	}
	for strip := 0; strip <= 3 && strip < len(code); strip++ {
		// The base64 body follows the 3-char "ADC" prefix, so a valid code has
		// (len - 3) % 4 == 0 (verified against a full stored copy of a code
		// that another file stores prefix-elided).
		if candidate := "ADCE" + code[strip:]; (len(candidate)-3)%4 == 0 {
			return candidate
		}
	}
	return code // unrepairable; return as-is
}
