package dcg

// Decoding of the Artifact 1.0 board-state payload, which differs from 2.0.
//
// 2.0 inlines every field token on every record. 1.0 uses a byte-aligned
// field-path entity serialization: one baseline object is spelled out in full
// (the 2.0 inline murmur-token format), and every other object is a field-path
// delta against it. Objects are 0x89-delimited field records, each introduced
// by the objectId field — op 0x10 followed by a 5-byte varint object id (the
// ids land in the 0x1000_0000..0x8000_0000 range). Card objects carry a full
// card protobuf inline; container objects carry length-prefixed zone/info
// strings ("Deck", "Hand", "Card Game Data", …).
//
// The complete field-path op table (needed to reconstruct every object's
// attributes from the baseline, and to decode the per-action delta event
// stream) is still being reverse-engineered; this recovers what is reliable:
// the object ids, the inline card protobufs, and the embedded zone names.

import "encoding/binary"

// fpObjectIDOp is the field-path op byte that precedes a 5-byte-varint object id.
const fpObjectIDOp = 0x10

// ClassicObject is a 1.0 board object recovered from the field-path stream.
type ClassicObject struct {
	ObjectID uint64 `json:"objectId"`
	// Card is the inline card protobuf decoded for this object, when present
	// (card objects). Container objects leave it nil.
	Card *Object `json:"card,omitempty"`
}

// objectIDRange bounds plausible 1.0 object ids (5-byte varints).
const (
	objIDMin = 0x10000000
	objIDMax = 0x80000000
)

// DecodeClassicState pulls the recoverable structure out of a 1.0 board-state
// payload: the object ids (in stream order), any inline card protobufs, and the
// embedded zone/info strings.
func DecodeClassicState(payload []byte) (objects []ClassicObject, zones []string) {
	// Object ids: op 0x10 followed by a 5-byte varint in the id range.
	type idpos struct {
		off int
		id  uint64
	}
	var ids []idpos
	for i := 1; i+5 <= len(payload); i++ {
		if payload[i-1] != fpObjectIDOp {
			continue
		}
		v, n := uvarint(payload, i)
		if n-i == 5 && v >= objIDMin && v < objIDMax {
			ids = append(ids, idpos{i - 1, v})
			i = n - 1
		}
	}
	// Inline card protobufs. In 1.0 these aren't introduced by the card symbol;
	// they appear directly as a CardCreated-shaped protobuf
	// (08 <owner> 10 <template> 1a <attrs…>) whose attributes carry the ownerId
	// or zoneId token — a reliable signature.
	cardAt := map[int]*Object{}
	for i := 0; i+5 < len(payload); i++ {
		if payload[i] != 0x08 || payload[i+1] >= 8 || payload[i+2] != 0x10 {
			continue
		}
		tmpl, o := uvarint(payload, i+3)
		if o == 0 || tmpl == 0 || tmpl > maxTemplateCardID || o >= len(payload) || payload[o] != 0x1a {
			continue
		}
		ln, no := uvarint(payload, o+1)
		if no == 0 || no+int(ln) > len(payload) {
			continue
		}
		attrs := payload[no : no+int(ln)]
		if !cardAttrs(attrs) {
			continue
		}
		if obj, ok := decodeCard(0, payload[i:no+int(ln)], false); ok {
			cardAt[i] = &obj
		}
		i = no + int(ln) - 1
	}
	// Pair each object id with the nearest following inline card (within its
	// record, before the next object id).
	for k, ip := range ids {
		end := len(payload)
		if k+1 < len(ids) {
			end = ids[k+1].off
		}
		co := ClassicObject{ObjectID: ip.id}
		for cardOff, card := range cardAt {
			if cardOff >= ip.off && cardOff < end {
				c := *card
				c.ObjectID = ip.id
				co.Card = &c
				break
			}
		}
		objects = append(objects, co)
	}
	zones = extractZoneStrings(payload)
	return objects, zones
}

// extractZoneStrings recovers the length-prefixed names embedded in container
// objects ([u16 little-endian length][string]).
func extractZoneStrings(payload []byte) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i+2 < len(payload); i++ {
		n := int(binary.LittleEndian.Uint16(payload[i:]))
		if n < 3 || n > 28 || i+2+n > len(payload) { // zone names are short words
			continue
		}
		s := payload[i+2 : i+2+n]
		if !printableASCII(s) || hasBase64Special(s) {
			continue
		}
		str := string(s)
		if !seen[str] {
			seen[str] = true
			out = append(out, str)
		}
		i += 2 + n - 1
	}
	return out
}

// cardAttrs reports whether a candidate attribute blob carries the ownerId or
// zoneId token, confirming it is a real card-instance attribute map.
func cardAttrs(b []byte) bool {
	return bytesContains(b, tokenLE(MakeStringToken("ownerId"))) ||
		bytesContains(b, tokenLE(MakeStringToken("zoneId")))
}

func bytesContains(b, sub []byte) bool { return indexFrom(b, sub, 0) >= 0 }

// hasBase64Special reports whether b contains '+' or '/', which mark deck codes
// (and other base64) rather than zone names.
func hasBase64Special(b []byte) bool {
	for _, c := range b {
		if c == '+' || c == '/' {
			return true
		}
	}
	return false
}

func printableASCII(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	letters := 0
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
		if c >= 'A' && c <= 'z' {
			letters++
		}
	}
	return letters*2 >= len(b) // mostly letters, to avoid binary coincidences
}
