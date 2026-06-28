package dcg

// Generic protobuf -> JSON decoding for exploration. Without the compiled
// descriptors we can't recover field names, so fields are keyed by number
// ("f1", "f2", …). Length-delimited values are decoded heuristically: a value
// that parses cleanly as a sub-message is shown as a nested object; an obvious
// UTF-8 string is shown as text; anything else is shown as hex.

import (
	"encoding/hex"
	"unicode/utf8"
)

// DecodeGeneric turns an arbitrary protobuf message into a JSON-friendly map.
// Repeated fields become arrays. depth bounds recursion into nested messages.
func DecodeGeneric(b []byte) map[string]any {
	return decodeGeneric(b, 8)
}

func decodeGeneric(b []byte, depth int) map[string]any {
	out := map[string]any{}
	r := newPBReader(b)
	for {
		f, ok, err := r.next()
		if err != nil || !ok {
			break
		}
		key := "f" + itoa(f.Num)
		var val any
		switch f.Wire {
		case wireVarint:
			val = f.V
		case wireI64:
			val = f.V
		case wireI32:
			val = uint32(f.V)
		case wireBytes:
			val = decodeBytesValue(f.Bytes, depth)
		default:
			continue
		}
		appendField(out, key, val)
	}
	return out
}

// decodeBytesValue renders a length-delimited value as nested message, string,
// or hex depending on what it most plausibly is.
func decodeBytesValue(b []byte, depth int) any {
	if len(b) == 0 {
		return ""
	}
	if depth > 0 && looksLikeMessage(b) {
		return decodeGeneric(b, depth-1)
	}
	if s, ok := printableString(b); ok {
		return s
	}
	const maxHex = 64
	if len(b) > maxHex {
		return map[string]any{"hex": hex.EncodeToString(b[:maxHex]) + "…", "len": len(b)}
	}
	return map[string]any{"hex": hex.EncodeToString(b), "len": len(b)}
}

// appendField stores val under key, promoting to a slice for repeated fields.
func appendField(m map[string]any, key string, val any) {
	if prev, exists := m[key]; exists {
		if arr, ok := prev.([]any); ok {
			m[key] = append(arr, val)
		} else {
			m[key] = []any{prev, val}
		}
		return
	}
	m[key] = val
}

// looksLikeMessage reports whether b parses cleanly as a non-trivial protobuf
// message (every field consumed, at least one field, sane field numbers).
func looksLikeMessage(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	r := newPBReader(b)
	fields := 0
	for {
		f, ok, err := r.next()
		if err != nil {
			return false
		}
		if !ok {
			break
		}
		if f.Num == 0 || f.Num > 536870911 {
			return false
		}
		fields++
	}
	return fields > 0 && r.done()
}

// printableString returns b as a string if it is valid UTF-8 made of printable
// runes (so we don't render binary blobs as garbage text).
func printableString(b []byte) (string, bool) {
	if !utf8.Valid(b) {
		return "", false
	}
	for _, r := range string(b) {
		if r != '\t' && r != '\n' && r != '\r' && r < 0x20 {
			return "", false
		}
		if r == utf8.RuneError {
			return "", false
		}
	}
	return string(b), true
}
