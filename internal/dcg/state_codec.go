package dcg

// state_codec.go decodes the Artifact 1.0 board-state serialization, reverse-
// engineered from the client reader `FUN_00d165e0` in libserver/libclient.
//
// The board-state message is protobuf-wrapped (`08 .. 10 .. 1a <len> <payload>`);
// the payload is a typed key→value tree:
//
//	value := [u32 key = MakeStringToken(name)] [u8 type] [data]
//
// with these value types (jump table, 40 entries, bound <= 0x27):
//
//	most (2, 5, 0xa, 0x11, 0x1f, …)  varint (7-bit groups, 0x80 continuation)
//	0x1d                             varint length + that many bytes (a blob —
//	                                 the CTraceMsg protobuf)
//	0x10                             fixed u32 (4 bytes little-endian)
//	1                                string: u16 length (< 0x400) + bytes
//	6                                nested value (recurse once)
//	7                                array (aligned; element encoding varies)
//
// The top level is the "Trace Events" entity map: name, objectId, a record
// count, record 0's fields inline, then a blob field holding the remaining
// records (which findCardProtos scans). This decoder extracts the self-
// describing header + record-0 fields; it resynchronises past the array/blob
// forms it doesn't model rather than aborting.

import "encoding/binary"

// CodecField is one decoded key→value entry.
type CodecField struct {
	Key   uint32
	Name  string // resolved token name, or "" if unknown
	Type  byte
	Int   uint64 // for varint / u32 types
	Bytes []byte // for blob (0x1d) / string (1) types
}

// codecValType classifies a value type byte.
func codecIsVarint(t byte) bool {
	switch t {
	case 1, 6, 7, 0x10, 0x1d:
		return false
	}
	return t < 0x28
}

// DecodeStatePayload finds the protobuf-wrapped payload in a 1.0 board-state
// message and decodes its top-level codec map. It returns the decoded fields
// (resynchronising past unmodelled array/nested forms).
func DecodeStatePayload(msg []byte) []CodecField {
	payload := statePayload(msg)
	if payload == nil {
		return nil
	}
	return decodeCodecMap(payload)
}

// findCodecTraces decodes the board-state entity maps in a 1.0 file and returns
// the traceMsg protobufs they contain as typed traces. A record's `type` field
// (the trace type id) precedes its `traceMsg` blob in the map, so each blob is
// typed exactly rather than by proto-signature guessing.
func findCodecTraces(buf []byte) []PosTrace {
	var out []PosTrace
	tmToken := traceTokenMsg
	typeToken := traceTokenType
	// Decode each protobuf-wrapped board-state payload found in the stream.
	for i := 0; i+2 < len(buf); i++ {
		if buf[i] != 0x1a {
			continue
		}
		ln, o := uvarint(buf, i+1)
		if o == 0 || ln < 32 || ln > 1<<20 || o+int(ln) > len(buf) {
			continue
		}
		fields := decodeCodecMap(buf[o : o+int(ln)])
		var curType uint64
		haveType := false
		for _, f := range fields {
			switch f.Key {
			case typeToken:
				curType, haveType = f.Int, true
			case tmToken:
				// Only emit records typed by a preceding `type` field — those
				// are the clean top-level records the codec reaches. Untyped
				// blobs overlap the proto-signature scan (form 2) and would add
				// shape-only UnknownTraces.
				if f.Type == 0x1d && len(f.Bytes) > 0 && haveType {
					tr := Trace{TypeID: curType, Fields: map[string]any{}}
					if name := lookupClassicTypeID(curType); name != "" {
						tr.Type = name
					}
					if prefix, ok := parseTracePrefix(f.Bytes); ok {
						tr.Fields = decodeTraceFields(prefix, tr.Type)
					}
					out = append(out, PosTrace{Offset: o, Trace: tr})
				}
				haveType = false
			}
		}
		i = o + int(ln) - 1
	}
	return out
}

// statePayload extracts the payload bytes from the protobuf envelope
// (`… 1a <varint len> <payload>`), or returns the input if it already looks
// like a raw codec stream.
func statePayload(msg []byte) []byte {
	for i := 0; i+1 < len(msg) && i < 32; i++ {
		if msg[i] != 0x1a { // field 3, wire type 2
			continue
		}
		ln, o := uvarint(msg, i+1)
		if o == 0 || ln < 16 || o+int(ln) > len(msg) {
			continue
		}
		return msg[o : o+int(ln)]
	}
	return msg
}

// decodeCodecMap decodes a run of value nodes, resynchronising by advancing one
// byte whenever a node doesn't parse cleanly (the array/blob record forms are
// not modelled). It stops at a zero key or end of buffer.
func decodeCodecMap(d []byte) []CodecField {
	var out []CodecField
	o := 0
	skips := 0
	for o+5 <= len(d) && len(out) < 4096 {
		f, no, ok := decodeCodecValue(d, o)
		if !ok {
			o++ // resync
			skips++
			if skips > 4096 {
				break
			}
			continue
		}
		if f.Key == 0 {
			break
		}
		out = append(out, f)
		o = no
	}
	return out
}

// decodeCodecValue reads one [u32 key][type][data] node. ok is false when the
// node is malformed (caller resyncs).
func decodeCodecValue(d []byte, o int) (f CodecField, next int, ok bool) {
	if o+5 > len(d) {
		return f, o, false
	}
	key := binary.LittleEndian.Uint32(d[o:])
	if key == 0 {
		return CodecField{}, o + 4, true
	}
	// A plausible field key is a resolvable token or a hash-like value (the
	// header/record-0 fields all resolve or look like murmur hashes).
	name := TokenName(key)
	if name == "" && key < 0x01000000 {
		return f, o, false
	}
	typ := d[o+4]
	o2 := o + 5
	f = CodecField{Key: key, Name: name, Type: typ}
	switch {
	case typ == 1: // string: u16 len + bytes
		if o2+2 > len(d) {
			return f, o, false
		}
		n := int(binary.LittleEndian.Uint16(d[o2:]))
		o2 += 2
		if n > 0x400 || o2+n > len(d) {
			return f, o, false
		}
		f.Bytes = d[o2 : o2+n]
		return f, o2 + n, true
	case typ == 0x1d: // blob: varint len + bytes
		n, no := uvarint(d, o2)
		if no == 0 || n > 0x400 || no+int(n) > len(d) {
			return f, o, false
		}
		f.Bytes = d[no : no+int(n)]
		return f, no + int(n), true
	case typ == 0x10: // fixed u32
		if o2+4 > len(d) {
			return f, o, false
		}
		f.Int = uint64(binary.LittleEndian.Uint32(d[o2:]))
		return f, o2 + 4, true
	case codecIsVarint(typ): // varint scalar
		v, no := uvarint(d, o2)
		if no == 0 {
			return f, o, false
		}
		f.Int = v
		return f, no, true
	default: // type 6 (nested) / 7 (array) — not modelled; let caller resync
		return f, o, false
	}
}
