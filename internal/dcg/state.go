package dcg

// Decoding of the board-state payload carried inside CMsgGameBoardGameState.
//
// The payload is an uncompressed Source 2 schema-serialized snapshot. Game
// objects are emitted as a symbol-keyed stream; each object is introduced by
// the object-id symbol followed by a length-delimited protobuf "card" record:
//
//	[symObjectID][varint object_id][symCard][varint len][card protobuf]
//
// This exact substructure appears in both full snapshots (is_diff=false) and
// deltas (is_diff=true, where only changed objects are present), so a single
// scan recovers objects from either. The card protobuf's field layout matches
// the documented card-state fields (owner, template id, attributes, effects,
// flags). Per-object attributes (card field 3) are themselves a symbol-keyed
// map of attribute-hash -> value; we surface them best-effort (the attribute
// name hashes are not resolvable without the game's attribute table).

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
)

// Serialization symbols discovered in the snapshot stream (5 bytes each).
var (
	symObjectID = []byte{0x32, 0xd5, 0x48, 0x7c, 0x1f} // precedes a varint object id
	symCard     = []byte{0x3e, 0x3b, 0xae, 0x62, 0x1d} // precedes a length-delimited card
)

// Card field numbers within the embedded protobuf.
const (
	cardFieldOwner      = 1
	cardFieldTemplateID = 2
	cardFieldAttributes = 3 // symbol-keyed attribute map
	cardFieldEffects    = 4
)

// Object is a decoded board object (a card/hero/creep/tower/improvement, etc.).
type Object struct {
	ObjectID      uint64  `json:"objectId"`
	OwnerID       *uint32 `json:"ownerId,omitempty"`
	TemplateCardID uint32 `json:"templateCardId"`
	Attributes    []Attr  `json:"attributes,omitempty"`
	Flags         map[int]uint64 `json:"flags,omitempty"`
	RawCardHex    string  `json:"rawCardHex,omitempty"`
}

// Attr is one attribute: a symbol hash (hex) and its scalar value. The hash is
// the game's attribute-name hash; resolving it to a name needs the attribute
// table, which isn't bundled.
type Attr struct {
	Hash  string `json:"hash"`
	Value uint64 `json:"value"`
}

// ExtractObjects pulls every object update out of a board-state payload (full
// or diff). includeRaw embeds the raw card protobuf hex per object.
func ExtractObjects(payload []byte, includeRaw bool) []Object {
	var objs []Object
	i := 0
	for {
		j := indexFrom(payload, symObjectID, i)
		if j < 0 {
			break
		}
		o := j + len(symObjectID)
		id, n := uvarint(payload, o)
		if n == 0 {
			i = j + 1
			continue
		}
		o = n
		// The object id is only an object header when immediately followed by
		// the card symbol; the same id symbol also appears inside attributes.
		if !bytes.Equal(at(payload, o, len(symCard)), symCard) {
			i = j + 1
			continue
		}
		o += len(symCard)
		ln, n2 := uvarint(payload, o)
		if n2 == 0 || n2+int(ln) > len(payload) {
			i = j + 1
			continue
		}
		card := payload[n2 : n2+int(ln)]
		obj, ok := decodeCard(id, card, includeRaw)
		if ok {
			objs = append(objs, obj)
		}
		i = n2 + int(ln)
	}
	return objs
}

// maxTemplateCardID bounds a plausible card template id; values above this (or
// equal to the object id) indicate the "card" protobuf was a coincidental match
// rather than a real card record.
const maxTemplateCardID = 1 << 20

func decodeCard(id uint64, card []byte, includeRaw bool) (Object, bool) {
	m, _ := fieldMap(card)
	tid, ok := first(m, cardFieldTemplateID)
	if !ok {
		return Object{}, false // not a card record (e.g. minimal control object)
	}
	if tid.V == id || tid.V > maxTemplateCardID {
		return Object{}, false // coincidental match, not a real card
	}
	obj := Object{ObjectID: id, TemplateCardID: uint32(tid.V)}
	if owner, ok := first(m, cardFieldOwner); ok {
		ov := uint32(owner.V)
		obj.OwnerID = &ov
	}
	if attrs, ok := first(m, cardFieldAttributes); ok {
		obj.Attributes = decodeAttributes(attrs.Bytes)
	}
	for _, f := range []int{5, 6, 7, 8, 9} {
		if v, ok := first(m, f); ok && v.V != 0 {
			if obj.Flags == nil {
				obj.Flags = map[int]uint64{}
			}
			obj.Flags[f] = v.V
		}
	}
	if includeRaw {
		obj.RawCardHex = hex.EncodeToString(card)
	}
	return obj, true
}

// decodeAttributes walks the symbol-keyed attribute map: repeated
// [5-byte symbol hash][varint value]. The object-id symbol (which restates the
// object id) is skipped. Decoding stops at the first sign of misalignment — a
// symbol with an embedded zero run or an implausibly large value — because the
// exact variable-length symbol/value grammar (especially in deltas) is not yet
// fully reversed, and emitting drifted garbage is worse than stopping early.
func decodeAttributes(b []byte) []Attr {
	var attrs []Attr
	o := 0
	for o+5 <= len(b) {
		sym := b[o : o+5]
		if driftedSymbol(sym) {
			break
		}
		v, n := uvarint(b, o+5)
		if n == 0 || v > 1<<48 {
			break
		}
		if !bytesEqual(sym, symObjectID) { // object id is surfaced separately
			attrs = append(attrs, Attr{Hash: hex.EncodeToString(sym), Value: v})
		}
		o = n
	}
	return attrs
}

// driftedSymbol heuristically detects a misaligned read: real attribute symbol
// hashes don't begin with a zero byte or contain a two-byte zero run.
func driftedSymbol(sym []byte) bool {
	if sym[0] == 0 {
		return true
	}
	for i := 0; i+1 < len(sym); i++ {
		if sym[i] == 0 && sym[i+1] == 0 {
			return true
		}
	}
	return false
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

// --- small helpers ---

func indexFrom(b, sep []byte, from int) int {
	if from < 0 {
		from = 0
	}
	if from >= len(b) {
		return -1
	}
	j := bytes.Index(b[from:], sep)
	if j < 0 {
		return -1
	}
	return from + j
}

func at(b []byte, off, n int) []byte {
	if off < 0 || off+n > len(b) {
		return nil
	}
	return b[off : off+n]
}

// uvarint reads a base-128 varint at off, returning the value and the offset
// just past it (0 length on failure).
func uvarint(b []byte, off int) (uint64, int) {
	var x uint64
	var s uint
	for o := off; o < len(b); o++ {
		c := b[o]
		x |= uint64(c&0x7f) << s
		if c&0x80 == 0 {
			return x, o + 1
		}
		s += 7
		if s > 63 {
			return 0, 0
		}
	}
	return 0, 0
}

var _ = binary.LittleEndian // keep binary imported for potential fixed reads
