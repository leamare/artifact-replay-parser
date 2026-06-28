package dcg

// Container parsing for Artifact replay files.
//
// Two on-disk formats are supported:
//
//	2.0 Foundry (.dem): header = u32 version(=7), u32 matchId, then 4 reserved
//	    bytes. Records follow at offset 12, each framed as
//	    [byte 0xb6][u32 a][u16 type][u32 size][payload[size]]
//	    where `type` is an EDCGGameMessages id. This frame tiles the whole file.
//
//	1.0 Classic (.dcg): header = u32 crc, u32 version(=7), u32 matchId (the
//	    leading crc is the only header difference). The record stream after the
//	    header uses a different, bit-packed Source2 encoding that is not yet
//	    decoded — see Reader.ContainerDecoded.
//
// Both formats embed the same opaque schema-serialized board-state payload.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Version identifies the replay generation.
type Version int

const (
	VersionUnknown Version = iota
	VersionClassic         // 1.0, .dcg
	VersionFoundry         // 2.0, .dem
)

func (v Version) String() string {
	switch v {
	case VersionClassic:
		return "1.0 (Classic)"
	case VersionFoundry:
		return "2.0 (Foundry)"
	default:
		return "unknown"
	}
}

// Record is one container frame: a single EDCGGameMessages-typed message.
type Record struct {
	Offset    int    // byte offset of the frame in the file
	A         uint32 // frame's leading u32 (per-channel sequence/ack, unconfirmed)
	Type      int    // EDCGGameMessages id
	Payload   []byte // message bytes (sub-slice of the file buffer)
	Scavenged bool   // 1.0: recovered by content scan, type is inferred
}

// Reader holds a parsed replay container.
type Reader struct {
	Version          Version
	MatchID          uint32
	CRC              uint32 // 1.0 only
	Records          []Record
	ContainerDecoded bool   // false for 1.0 until the bit-stream is reversed
	Note             string // human-readable status (e.g. why records are empty)

	buf []byte
}

const frameMarker = 0xb6

// knownMsgType reports whether t is a documented EDCGGameMessages id. Used to
// validate frames during scanning.
func knownMsgType(t int) bool {
	switch t {
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 1000, 1001:
		return true
	}
	return false
}

// Parse decodes a replay file. name is used only as a format hint (extension).
func Parse(name string, buf []byte) (*Reader, error) {
	if len(buf) < 12 {
		return nil, errors.New("file too short to be a replay")
	}
	r := &Reader{buf: buf}

	v0 := binary.LittleEndian.Uint32(buf[0:])
	v1 := binary.LittleEndian.Uint32(buf[4:])
	switch {
	case v0 == 7:
		// 2.0: version, matchId, reserved...
		r.Version = VersionFoundry
		r.MatchID = v1
	case v1 == 7:
		// 1.0: crc, version, matchId
		r.Version = VersionClassic
		r.CRC = v0
		r.MatchID = binary.LittleEndian.Uint32(buf[8:])
	default:
		// Fall back to extension if the version word isn't where we expect.
		if strings.HasSuffix(strings.ToLower(name), ".dcg") {
			r.Version = VersionClassic
			r.CRC = v0
			r.MatchID = binary.LittleEndian.Uint32(buf[8:])
		} else {
			r.Version = VersionFoundry
			r.MatchID = v1
		}
	}

	if r.Version == VersionFoundry {
		if err := r.scanFoundry(); err != nil {
			return r, err
		}
		r.ContainerDecoded = true
	} else {
		// 1.0 (.dcg): only the periodic full/keyframe board states and some
		// markers are 0xb6-framed and recoverable. The bulk of the game (the
		// per-action deltas) uses a separate compact serialization that is not
		// yet decoded, so coverage is partial.
		r.scanClassic()
	}
	return r, nil
}

// readFrame decodes a 2.0 frame header at off, returning the record and the
// offset just past its payload. ok=false if off is not a plausible frame.
func (r *Reader) readFrame(off int) (rec Record, next int, ok bool) {
	b := r.buf
	if off < 0 || off+11 > len(b) || b[off] != frameMarker {
		return Record{}, 0, false
	}
	a := binary.LittleEndian.Uint32(b[off+1:])
	typ := int(binary.LittleEndian.Uint16(b[off+5:]))
	size := int(binary.LittleEndian.Uint32(b[off+7:]))
	ps := off + 11
	if !knownMsgType(typ) || size < 0 || ps+size > len(b) {
		return Record{}, 0, false
	}
	return Record{Offset: off, A: a, Type: typ, Payload: b[ps : ps+size]}, ps + size, true
}

// scanFoundry walks the 2.0 record stream. The frames are contiguous, but we
// validate each with one-frame lookahead and resync on the rare chance of an
// unexpected gap, so a single malformed record can't desync the whole parse.
func (r *Reader) scanFoundry() error {
	b := r.buf
	// First frame: skip the 8-byte header + up to a few reserved bytes.
	off := -1
	for s := 8; s <= 16 && s+11 <= len(b); s++ {
		if _, _, ok := r.readFrame(s); ok {
			off = s
			break
		}
	}
	if off < 0 {
		return fmt.Errorf("no valid record frame found in first 16 bytes")
	}

	for off < len(b) {
		rec, next, ok := r.readFrame(off)
		if ok && (next == len(b) || r.looksLikeFrame(next)) {
			r.Records = append(r.Records, rec)
			off = next
			continue
		}
		// Resync: hunt forward for the next lookahead-valid frame.
		p := off + 1
		for p+11 <= len(b) {
			if b[p] == frameMarker {
				if _, n2, ok2 := r.readFrame(p); ok2 && (n2 == len(b) || r.looksLikeFrame(n2)) {
					break
				}
			}
			p++
		}
		if p+11 > len(b) {
			break // no more frames
		}
		off = p
	}
	if len(r.Records) == 0 {
		return errors.New("2.0 container: no records decoded")
	}
	return nil
}

func (r *Reader) looksLikeFrame(off int) bool {
	_, _, ok := r.readFrame(off)
	return ok
}

// validClassicPayload checks that a candidate 1.0 record payload actually
// decodes as its claimed message type, so we don't anchor on coincidental 0xb6
// bytes (which are common inside the compact delta regions).
func validClassicPayload(typ int, p []byte) bool {
	switch typ {
	case MsgBoardGameState:
		// CMsgGameBoardGameState envelope: is_diff(1) is_last(2) payload(3).
		return len(p) >= 6 && p[0] == 0x08 && p[2] == 0x10 && p[4] == 0x1a
	case MsgReplayActionMark:
		return len(p) >= 2 && p[0] == 0x08 && p[1] >= 1 && p[1] <= 14
	case MsgHandoff:
		return len(p) >= 2 && p[0] == 0x08
	case MsgConnectionStatus, MsgVenchatoquist:
		return true
	}
	return false
}

// readClassicFrame tries both 1.0 frame widths at off: the common 11-byte
// header [b6][u32 a][u16 type][u32 size] and the 10-byte variant seen on the
// opening keyframe [b6][u24 a][u16 type][u32 size]. Returns a validated record.
func (r *Reader) readClassicFrame(off int) (rec Record, next int, ok bool) {
	b := r.buf
	if off < 0 || b[off] != frameMarker {
		return Record{}, 0, false
	}
	for _, hdr := range []int{11, 10} {
		if off+hdr > len(b) {
			continue
		}
		typ := int(binary.LittleEndian.Uint16(b[off+hdr-6:]))
		size := int(binary.LittleEndian.Uint32(b[off+hdr-4:]))
		ps := off + hdr
		if !knownMsgType(typ) || size < 0 || ps+size > len(b) {
			continue
		}
		if !validClassicPayload(typ, b[ps:ps+size]) {
			continue
		}
		a := uint32(0)
		if hdr == 11 {
			a = binary.LittleEndian.Uint32(b[off+1:])
		}
		return Record{Offset: off, A: a, Type: typ, Payload: b[ps : ps+size]}, ps + size, true
	}
	return Record{}, 0, false
}

// scanClassic recovers the 0xb6-framed records from a 1.0 file by scanning for
// content-validated frames. It records coverage so callers know how much of the
// stream (the compact delta regions) remains undecoded.
func (r *Reader) scanClassic() {
	b := r.buf
	var covered int
	off := 12
	for off+11 <= len(b) {
		if b[off] != frameMarker {
			off++
			continue
		}
		rec, next, ok := r.readClassicFrame(off)
		if !ok {
			off++
			continue
		}
		r.Records = append(r.Records, rec)
		covered += next - off
		off = next
	}
	framed := len(r.Records)
	// The non-b6 region is a stream of length-delimited protobuf GC messages.
	// Scavenge the ones we can confidently classify (chat/venchatoquist) so chat
	// and personas surface; keep them in file order.
	r.scavengeClassic()
	sort.Slice(r.Records, func(i, j int) bool { return r.Records[i].Offset < r.Records[j].Offset })

	r.ContainerDecoded = len(r.Records) > 0
	pct := 0
	if len(b) > 12 {
		pct = covered * 100 / (len(b) - 12)
	}
	r.Note = fmt.Sprintf("1.0 (.dcg): %d 0xb6-framed records (~%d%% of the stream, "+
		"incl. keyframe board states) + %d scavenged length-delimited GC messages; "+
		"the bulk per-action delta stream is still being reverse-engineered",
		framed, pct, len(r.Records)-framed)
}

// scavengeClassic recovers length-delimited protobuf GC messages from the 1.0
// stream that we can classify with confidence. Currently this targets
// venchatoquist (chat/persona) records, anchored on their persona string, since
// those are unambiguous and carry player identity + chat.
func (r *Reader) scavengeClassic() {
	b := r.buf
	covered := make(map[int]bool)
	for _, rec := range r.Records {
		covered[rec.Offset] = true
	}
	i := 12
	for i < len(b) {
		ln, o := uvarint(b, i)
		if o != 0 && ln >= 4 && ln <= 4096 && o+int(ln) <= len(b) {
			msg := b[o : o+int(ln)]
			if looksLikeMessage(msg) && isVenchatShape(msg) {
				r.Records = append(r.Records, Record{Offset: i, Type: MsgVenchatoquist, Payload: msg, Scavenged: true})
				i = o + int(ln)
				continue
			}
		}
		i++
	}
}

// isVenchatShape reports whether a message looks like a 1.0 venchatoquist event:
// a persona string in field 4 plus at least one other field. (1.0 places the
// chat text in field 3, object id in field 2, line id in field 6.)
func isVenchatShape(msg []byte) bool {
	m, err := fieldMap(msg)
	if err != nil {
		return false
	}
	persona, ok := first(m, 4)
	if !ok || persona.Wire != wireBytes {
		return false
	}
	if s, ok := printableString(persona.Bytes); !ok || len(s) < 2 || len(s) > 64 {
		return false
	}
	return len(m) >= 2
}
