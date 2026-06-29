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
	ObjectID       uint64         `json:"objectId"`
	OwnerID        *uint32        `json:"ownerId,omitempty"`
	TemplateCardID uint32         `json:"templateCardId"`
	Attributes     []Attr         `json:"attributes,omitempty"`
	Flags          map[int]uint64 `json:"flags,omitempty"`
	RawCardHex     string         `json:"rawCardHex,omitempty"`
}

// Attr is one decoded attribute. Name is the resolved field name (via the
// murmur2 token table) when known; Hash is the hex token when not. Value is the
// primary scalar value.
type Attr struct {
	Name  string `json:"name,omitempty"`
	Hash  string `json:"hash,omitempty"`
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

// objectIdToken is the field token whose value restates the object id; it is
// dropped from the attribute list since the object id is surfaced separately.
var objectIdToken = MakeStringToken("objectId")

// decodeAttributes walks the symbol-keyed attribute map. Each field is
// [4-byte name token][1-byte valtype < 0x20][value], where the value is a
// varint (or, for valtype 0x1d, a length-delimited blob). Some fields carry
// extra trailing varints (e.g. a position pair); we consume them up to the next
// field start. Zero bytes between fields are padding. Decoding stops at the
// first byte that can't begin a field, so the reliable leading fields
// (objectId, ownerId, zoneId, …) are always recovered even if a later
// multi-value field is ambiguous.
func decodeAttributes(b []byte) []Attr {
	var attrs []Attr
	o := 0
	for o < len(b) {
		if !isFieldStart(b, o) {
			if b[o] == 0 { // padding / terminator
				o++
				continue
			}
			break
		}
		tok := leU32(b[o:])
		vt := b[o+4]
		o += 5
		var val uint64
		if vt == valtypeBytes {
			n, no := uvarint(b, o)
			if no == 0 || no+int(n) > len(b) {
				break
			}
			val = n // surface the blob length
			o = no + int(n)
		} else {
			v, no := uvarint(b, o)
			if no == 0 {
				break
			}
			val = v
			o = no
			// Consume any extra value varints of a multi-value field.
			for o < len(b) && !isFieldStart(b, o) && b[o] != 0 {
				_, no2 := uvarint(b, o)
				if no2 == 0 || no2 == o {
					break
				}
				o = no2
			}
		}
		if tok == objectIdToken {
			continue
		}
		a := Attr{Value: val}
		if name := TokenName(tok); name != "" {
			a.Name = name
		} else {
			a.Hash = hexToken(tok)
		}
		attrs = append(attrs, a)
	}
	return attrs
}

// valtypeBytes is the valtype code for a length-delimited (sub-message) value.
const valtypeBytes = 0x1d

// isFieldStart reports whether a field header begins at b[o]: a non-zero 4-byte
// token followed by a valtype byte in the valid range (< 0x20).
func isFieldStart(b []byte, o int) bool {
	if o+5 > len(b) {
		return false
	}
	if b[o+4] >= 0x20 {
		return false
	}
	return b[o]|b[o+1]|b[o+2]|b[o+3] != 0
}

func hexToken(tok uint32) string {
	const hexd = "0123456789abcdef"
	var out [9]byte
	out[0] = '#'
	for i := 0; i < 4; i++ {
		bb := byte(tok >> (8 * i)) // little-endian, as seen on the wire
		out[1+i*2] = hexd[bb>>4]
		out[2+i*2] = hexd[bb&0xf]
	}
	return string(out[:])
}

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
