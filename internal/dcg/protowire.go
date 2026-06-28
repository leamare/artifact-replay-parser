package dcg

// Minimal protobuf wire-format reader. The replay messages we decode are tiny
// (a handful of scalar fields each), so a full generated-code dependency is
// unnecessary. This reader exposes just enough to walk fields and pull scalars.

import "errors"

var errTruncated = errors.New("protowire: truncated buffer")

// Wire types.
const (
	wireVarint = 0
	wireI64    = 1
	wireBytes  = 2
	wireI32    = 5
)

// Field is one decoded protobuf field.
type Field struct {
	Num  int
	Wire int
	// Exactly one of these is meaningful depending on Wire:
	V     uint64 // varint, i32 or i64 raw value
	Bytes []byte // length-delimited payload (sub-slice of the input)
}

// pbReader walks the fields of a protobuf message.
type pbReader struct {
	buf []byte
	pos int
}

func newPBReader(b []byte) *pbReader { return &pbReader{buf: b} }

func (r *pbReader) done() bool { return r.pos >= len(r.buf) }

func (r *pbReader) varint() (uint64, error) {
	var x uint64
	var s uint
	for {
		if r.pos >= len(r.buf) {
			return 0, errTruncated
		}
		b := r.buf[r.pos]
		r.pos++
		x |= uint64(b&0x7f) << s
		if b&0x80 == 0 {
			return x, nil
		}
		s += 7
		if s > 63 {
			return 0, errors.New("protowire: varint overflow")
		}
	}
}

// next reads the next field; returns false when the buffer is exhausted.
func (r *pbReader) next() (Field, bool, error) {
	if r.done() {
		return Field{}, false, nil
	}
	tag, err := r.varint()
	if err != nil {
		return Field{}, false, err
	}
	f := Field{Num: int(tag >> 3), Wire: int(tag & 7)}
	if f.Num == 0 {
		return Field{}, false, errors.New("protowire: zero field number")
	}
	switch f.Wire {
	case wireVarint:
		f.V, err = r.varint()
	case wireI64:
		if r.pos+8 > len(r.buf) {
			return Field{}, false, errTruncated
		}
		f.V = leU64(r.buf[r.pos:])
		r.pos += 8
	case wireI32:
		if r.pos+4 > len(r.buf) {
			return Field{}, false, errTruncated
		}
		f.V = uint64(leU32(r.buf[r.pos:]))
		r.pos += 4
	case wireBytes:
		var n uint64
		n, err = r.varint()
		if err == nil {
			if r.pos+int(n) > len(r.buf) {
				return Field{}, false, errTruncated
			}
			f.Bytes = r.buf[r.pos : r.pos+int(n)]
			r.pos += int(n)
		}
	default:
		return Field{}, false, errors.New("protowire: unsupported wire type")
	}
	if err != nil {
		return Field{}, false, err
	}
	return f, true, nil
}

// fieldMap decodes every field into a map keyed by field number. For repeated
// fields only the values are appended. Returns partial results on error.
func fieldMap(b []byte) (map[int][]Field, error) {
	m := map[int][]Field{}
	r := newPBReader(b)
	for {
		f, ok, err := r.next()
		if err != nil {
			return m, err
		}
		if !ok {
			return m, nil
		}
		m[f.Num] = append(m[f.Num], f)
	}
}

func leU16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func leU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func leU64(b []byte) uint64 {
	return uint64(leU32(b)) | uint64(leU32(b[4:]))<<32
}
