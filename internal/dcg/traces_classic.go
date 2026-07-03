package dcg

// Pattern-based trace extraction for Artifact 1.0 payloads.
//
// The 1.0 stream appends trace records in the same shape as 2.0 —
//
//	[objectId][traceMsg][valtype 0x1d][len][CTraceMsg protobuf]
//
// — but the murmur tokens are only inline on first use; later records refer to
// them with short back-reference ops (grammar not fully reversed yet). Two
// record forms are reliably recognizable without the full op table:
//
//  1. inline-token records: the 5-byte traceMsg token appears, followed by
//     [0x1d][len][proto] (same as 2.0; the shared extractor handles these);
//  2. card-payload records: a CardCreated/CardRevealed-shaped protobuf
//     (08 <owner> 10 <template> 1a <attrs> …) with the subject object id as a
//     5-byte varint shortly before it (ops 0x1b / 0x10 / token).
//
// This recovers the most informative traces (card creations/reveals with
// template ids and attributes) from every 1.0 board state; the remaining
// scalar traces need the op-table decoder.

// TracesFor extracts the traces of a board-state payload using the extractor
// appropriate for the replay version.
func TracesFor(v Version, payload []byte) []Trace {
	if v == VersionClassic {
		return ExtractClassicTraces(payload)
	}
	return ExtractTraces(payload)
}

// classicTypeIDShift is where the 1.0 and 2.0 trace-type enums diverge: 2.0
// inserted GoldSpent at 66, shifting every later id by 2. (Verified: 1.0's
// first trace has type id 106 where 2.0's ScriptBlockBegin is 108.)
const classicTypeIDShift = 66

// lookupClassicTypeID resolves a 1.0 trace-type id through the 2.0 name table.
func lookupClassicTypeID(id uint64) string {
	if id == 0 {
		return ""
	}
	if id >= classicTypeIDShift {
		id += 2
	}
	return lookupTypeID(id)
}

// PosTrace is a trace paired with its byte offset in the file, for ordering
// against the (sparse) recovered 1.0 records.
type PosTrace struct {
	Offset int
	Trace  Trace
}

// ExtractClassicFileTraces scans an entire 1.0 file — the recovered records
// and the not-yet-decoded delta gaps alike — for trace records. The delta
// stream carries the same record content as the framed board states, so
// scanning the whole buffer recovers the card events wherever they sit.
func ExtractClassicFileTraces(buf []byte) []PosTrace {
	var out []PosTrace
	type span struct{ a, b int }
	var spans []span

	// Form 1: inline traceMsg-token records anywhere in the file.
	i := 0
	for {
		j := indexFrom(buf, tokenLE(traceTokenMsg), i)
		if j < 0 {
			break
		}
		o := j + 4
		if o >= len(buf) || buf[o] != valtypeBytes {
			i = j + 1
			continue
		}
		ln, no := uvarint(buf, o+1)
		if no == 0 || no+int(ln) > len(buf) {
			i = j + 1
			continue
		}
		prefix, ok := parseTracePrefix(buf[no : no+int(ln)])
		if !ok {
			i = j + 1
			continue
		}
		tr := Trace{ObjectID: precedingObjectID(buf, j), TypeID: nearestTypeID(buf, j)}
		if name := lookupClassicTypeID(tr.TypeID); name != "" {
			tr.Type = name
		} else if name := classifyShapeOnly(prefix); name != "" {
			tr.Type = name
		}
		// A card-shaped body (owner, template, attrs) is a CardCreated; the
		// CardTemplateOperation signature matches the same field set spuriously.
		if len(prefix) > 3 && prefix[0] == 0x08 && prefix[1] < 8 && prefix[2] == 0x10 &&
			(tr.Type == "" || tr.Type == "CardTemplateOperation") {
			tr.Type = "CardCreated"
		}
		tr.Fields = decodeTraceFields(prefix, tr.Type)
		out = append(out, PosTrace{Offset: j, Trace: tr})
		spans = append(spans, span{j, no + len(prefix)})
		i = no + int(ln)
	}
	// Form 2: card-shaped protos outside the form-1 spans.
	for _, cp := range findCardProtosLoose(buf) {
		inSpan := false
		for _, s := range spans {
			if cp.off >= s.a && cp.off < s.b {
				inSpan = true
				break
			}
		}
		if inSpan {
			continue
		}
		tr := Trace{
			Type:     "CardCreated",
			ObjectID: cp.objectID,
			Fields:   decodeTraceFields(cp.proto, "CardCreated"),
		}
		out = append(out, PosTrace{Offset: cp.off, Trace: tr})
	}
	sortPosTraces(out)
	return out
}

// findCardProtosLoose is findCardProtos with validation suited to the delta
// stream, where the attribute map's tokens may be back-referenced (so the
// ownerId/zoneId token check can't be required). Tighter bounds on owner and
// template id compensate.
func findCardProtosLoose(p []byte) []cardProto {
	var out []cardProto
	for i := 0; i+10 < len(p); i++ {
		if p[i] != 0x08 || p[i+1] >= 4 || p[i+2] != 0x10 {
			continue
		}
		tmpl, o := uvarint(p, i+3)
		if o == 0 || tmpl < 900 || tmpl > 40000 || o >= len(p) || p[o] != 0x1a {
			continue
		}
		ln, no := uvarint(p, o+1)
		if no == 0 || ln < 16 || ln > 150 || no+int(ln) > len(p) {
			continue
		}
		end := no + int(ln)
		if prefix, ok := parseTracePrefix(p[i:min(end+64, len(p))]); ok && len(prefix) > end-i {
			end = i + len(prefix)
		}
		out = append(out, cardProto{off: i, objectID: precedingObjectID(p, i), proto: p[i:end]})
		i = end - 1
	}
	return out
}

func sortPosTraces(ts []PosTrace) {
	for i := 1; i < len(ts); i++ { // insertion sort; lists are small and near-sorted
		for j := i; j > 0 && ts[j].Offset < ts[j-1].Offset; j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

// ExtractClassicTraces returns the traces recoverable from a 1.0 payload.
func ExtractClassicTraces(payload []byte) []Trace {
	traces := ExtractTraces(payload) // inline-token records (form 1)
	covered := make(map[uint64]bool) // object ids already seen this payload
	for _, t := range traces {
		if t.ObjectID != 0 {
			covered[t.ObjectID] = true
		}
	}
	// Form 2: card protos not already recovered via an inline token. The find
	// pattern (owner, template id, attrs carrying ownerId/zoneId) is the
	// CardCreated layout by construction, so don't shape-classify (short
	// signatures like CardTemplateOperation would win spuriously).
	for _, cp := range findCardProtos(payload) {
		if cp.objectID != 0 && covered[cp.objectID] {
			continue
		}
		name := "CardCreated"
		tr := Trace{
			Type:     name,
			ObjectID: cp.objectID,
			Fields:   decodeTraceFields(cp.proto, name),
		}
		traces = append(traces, tr)
		if cp.objectID != 0 {
			covered[cp.objectID] = true
		}
	}
	return traces
}

// cardProto is a located card protobuf with its subject object id.
type cardProto struct {
	off      int
	objectID uint64
	proto    []byte
}

// findCardProtos scans for card-shaped protobufs:
// 08 <owner<8> 10 <template varint> 1a <len> <attrs with ownerId/zoneId token>.
func findCardProtos(p []byte) []cardProto {
	var out []cardProto
	for i := 0; i+8 < len(p); i++ {
		if p[i] != 0x08 || p[i+1] >= 8 || p[i+2] != 0x10 {
			continue
		}
		tmpl, o := uvarint(p, i+3)
		if o == 0 || tmpl == 0 || tmpl > maxTemplateCardID || o >= len(p) || p[o] != 0x1a {
			continue
		}
		ln, no := uvarint(p, o+1)
		if no == 0 || ln < 4 || ln > 400 || no+int(ln) > len(p) {
			continue
		}
		if !cardAttrs(p[no : no+int(ln)]) {
			continue
		}
		// The full record may extend past the attrs (calculated effects, flags);
		// take the protobuf prefix from the start.
		end := no + int(ln)
		for end < len(p) { // consume trailing scalar fields (f4 bytes, f5..f9 varints)
			prefix, ok := parseTracePrefix(p[i:min(end+64, len(p))])
			if ok && len(prefix) > end-i {
				end = i + len(prefix)
			}
			break
		}
		out = append(out, cardProto{off: i, objectID: precedingObjectID(p, i), proto: p[i:end]})
		i = end - 1
	}
	return out
}

// precedingObjectID looks back a short window for a 5-byte-varint object id
// (in the observed 1.0 id range), the record's subject.
func precedingObjectID(p []byte, pos int) uint64 {
	start := pos - 28
	if start < 0 {
		start = 0
	}
	var best uint64
	for o := start; o < pos-4; o++ {
		v, n := uvarint(p, o)
		if n-o == 5 && v >= objIDMin && v < objIDMax {
			best = v // keep the closest one before the proto
		}
	}
	return best
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
