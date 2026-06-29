package dcg

// Trace-event decoding. A board-state payload is the "Trace Events" buffer: a
// stream of records, each
//
//	type=<id>  objectId=<id>  traceMsg=<serialized CTraceMsg_*>
//
// The `traceMsg` field (token 0x62ae3b3e = MakeStringToken("traceMsg")) holds a
// length-delimited protobuf — one of the dcg_gamemessages CTraceMsg_* events.
// There is no explicit type tag inside traceMsg; instead the record's `type`
// field is a stable per-trace-type id (an even enum), and the message itself is
// identified by its protobuf field signature. We classify by signature (which
// is robust across replays/versions and, unlike the `type` enum, also works in
// delta states where the type id isn't repeated) and propagate a confident
// classification to every record sharing the same `type` id.

import "sort"

// traceSig is a CTraceMsg field signature used for classification.
type traceSig struct {
	name        string
	fields      []int
	bytesFields []int
}

// traceTokenMsg is MakeStringToken("traceMsg"); the field holding the CTraceMsg.
var traceTokenMsg = MakeStringToken("traceMsg")

// traceTokenType is MakeStringToken("type"); the per-record trace-type id.
var traceTokenType = MakeStringToken("type")

// Trace is one decoded trace event.
type Trace struct {
	TypeID   uint64         `json:"typeId"`
	Type     string         `json:"type"`               // CTraceMsg name, or "" if unclassified
	ObjectID uint64         `json:"objectId,omitempty"` // subject object, if any
	Fields   map[string]any `json:"fields,omitempty"`   // decoded traceMsg, named when classified
}

// knownTraceTypeIDs2 seeds the trace-type-id -> name table for Artifact 2.0
// with classifications confirmed empirically but ambiguous by field shape
// alone. The `type` id is the game's internal trace enum, stable across 2.0
// replays. (16: field1==field5 object id, field2 lane; 108/110: paired blocks
// whose field1 is an EScriptBlockType.)
var knownTraceTypeIDs2 = map[uint64]string{
	16:  "UnitChangedCombatTarget",
	108: "ScriptBlockBegin",
	110: "ScriptBlockEnd",
}

// ExtractTraces pulls every trace record out of a board-state payload (full or
// diff). It scans for the traceMsg token, reads the length-delimited CTraceMsg,
// and pairs it with the nearest preceding objectId and type id. Each trace is
// classified by field signature; records ambiguous by shape are resolved via
// their stable type id (seeded table + propagation from confidently-typed
// records), and the fields are then decoded under the resolved type.
func ExtractTraces(payload []byte) []Trace {
	type raw struct {
		tr  Trace
		msg []byte
	}
	var rows []raw
	typeByID := map[uint64]string{}
	for k, v := range knownTraceTypeIDs2 {
		typeByID[k] = v
	}

	i := 0
	for {
		j := indexFrom(payload, tokenLE(traceTokenMsg), i)
		if j < 0 {
			break
		}
		o := j + 4 // token (4) + valtype (1, expect 0x1d) + varint len + bytes
		if o >= len(payload) || payload[o] != valtypeBytes {
			i = j + 1
			continue
		}
		o++
		ln, no := uvarint(payload, o)
		if no == 0 || no+int(ln) > len(payload) {
			i = j + 1
			continue
		}
		msg := payload[no : no+int(ln)]
		// In delta states the traceMsg is delta-encoded, not a clean
		// length-delimited message; reading it as bytes over-reads. Only accept
		// a candidate that parses as a well-formed protobuf so we never emit
		// garbage. (Delta traces need the not-yet-implemented delta decoder.)
		// Unparseable tokens are surfaced as NotParsed rather than dropped.
		if !messageWellFormed(msg) {
			rows = append(rows, raw{Trace{
				Type:     "NotParsed",
				ObjectID: nearestObjectID(payload, j),
				TypeID:   nearestTypeID(payload, j),
			}, nil})
			i = j + 1
			continue
		}
		tr := Trace{
			ObjectID: nearestObjectID(payload, j),
			TypeID:   nearestTypeID(payload, j),
		}
		if name := classifyShapeOnly(msg); name != "" {
			tr.Type = name
			if tr.TypeID != 0 {
				typeByID[tr.TypeID] = name
			}
		} else if name := classifyByValue(msg); name != "" {
			// Value-pattern fallback for delta traces (which carry no type id):
			// not propagated to the type-id table since it's heuristic.
			tr.Type = name
		}
		rows = append(rows, raw{tr, msg})
		i = no + int(ln)
	}
	// Resolve type (shape, else type-id table) and decode fields under it.
	traces := make([]Trace, 0, len(rows))
	for _, r := range rows {
		if r.msg == nil { // NotParsed (delta-encoded) — surfaced as-is
			traces = append(traces, r.tr)
			continue
		}
		name := r.tr.Type
		if name == "" {
			name = typeByID[r.tr.TypeID]
		}
		r.tr.Type = name
		r.tr.Fields = decodeTraceFields(r.msg, name)
		traces = append(traces, r.tr)
	}
	return traces
}

// IsParsed reports whether a trace held a well-formed message (i.e. is not a
// delta-encoded token we couldn't decode).
func (t Trace) IsParsed() bool { return t.Type != "NotParsed" }

// DisplayType returns the trace type for output, mapping a parseable but
// unclassified trace to "UnknownTrace".
func (t Trace) DisplayType() string {
	if t.Type == "" {
		return "UnknownTrace"
	}
	return t.Type
}

// classifyShapeOnly returns the CTraceMsg name from field signature, or "".
func classifyShapeOnly(msg []byte) string {
	present, wire := messageSignature(msg)
	return matchSignature(present, wire)
}

// negOne is how protobuf encodes a signed -1 (sign-extended to 64 bits).
const negOne = ^uint64(0)

// classifyByValue classifies a delta trace whose field set is shape-ambiguous
// using distinctive scalar value patterns. Only patterns confident enough to
// avoid mislabeling are encoded here.
func classifyByValue(msg []byte) string {
	present, wire := messageSignature(msg)
	for _, w := range wire {
		if w == wireBytes {
			return "" // these patterns are scalar-only
		}
	}
	vals := firstScalarValues(msg)
	switch {
	// UnitChangedCombatTarget: {new_target, lane, old_arrow, arrow, old_target}
	// with new_target == old_target (the two combat-target object ids match).
	case exactly(present, 1, 2, 3, 4, 5) && vals[1] == vals[5] && vals[1] > 1000:
		return "UnitChangedCombatTarget"
	// UnitChangedCombatPosition: {new_pos, old_pos(int32), owner, lane} where a
	// unit entering combat has old_combat_position == -1.
	case exactly(present, 1, 2, 3, 4) && vals[2] == negOne:
		return "UnitChangedCombatPosition"
	}
	return ""
}

// firstScalarValues maps each scalar field number to its first value.
func firstScalarValues(msg []byte) map[int]uint64 {
	out := map[int]uint64{}
	r := newPBReader(msg)
	for {
		f, ok, err := r.next()
		if err != nil || !ok {
			break
		}
		if f.Wire == wireBytes {
			continue
		}
		if _, seen := out[f.Num]; !seen {
			out[f.Num] = f.V
		}
	}
	return out
}

// exactly reports whether present is exactly the given field set.
func exactly(present map[int]bool, fields ...int) bool {
	if len(present) != len(fields) {
		return false
	}
	for _, f := range fields {
		if !present[f] {
			return false
		}
	}
	return true
}

// nearestObjectID finds the objectId value immediately preceding the traceMsg
// token at pos (the record's `objectId  ...  traceMsg` layout).
func nearestObjectID(p []byte, pos int) uint64 {
	tok := tokenLE(MakeStringToken("objectId"))
	// search backwards a short window for the objectId token
	start := pos - 24
	if start < 0 {
		start = 0
	}
	last := -1
	for o := start; o < pos; o++ {
		if o+5 <= len(p) && string(p[o:o+4]) == string(tok) {
			last = o
		}
	}
	if last < 0 {
		return 0
	}
	v, _ := uvarint(p, last+5) // +4 token +1 valtype
	return v
}

// nearestTypeID finds the `type` id value preceding the traceMsg token (only
// present in full states; 0 if absent, e.g. in deltas).
func nearestTypeID(p []byte, pos int) uint64 {
	tok := tokenLE(traceTokenType)
	start := pos - 32
	if start < 0 {
		start = 0
	}
	last := -1
	for o := start; o < pos; o++ {
		if o+5 <= len(p) && string(p[o:o+4]) == string(tok) {
			last = o
		}
	}
	if last < 0 {
		return 0
	}
	v, _ := uvarint(p, last+5)
	return v
}

// tokenLE returns a token as its 4 little-endian wire bytes.
func tokenLE(tok uint32) []byte {
	return []byte{byte(tok), byte(tok >> 8), byte(tok >> 16), byte(tok >> 24)}
}

// messageWellFormed reports whether msg parses as a complete protobuf with no
// trailing bytes and no field number appearing under conflicting wire types
// (the tell-tale of an over-read into the next record).
func messageWellFormed(msg []byte) bool {
	if len(msg) == 0 {
		return false
	}
	seen := map[int]int{}
	r := newPBReader(msg)
	for {
		f, ok, err := r.next()
		if err != nil {
			return false
		}
		if !ok {
			break
		}
		if w, dup := seen[f.Num]; dup && w != f.Wire {
			return false
		}
		seen[f.Num] = f.Wire
	}
	return r.done()
}

// messageSignature returns the set of present field numbers and, for each, its
// wire type (used to tell bytes fields from scalars).
func messageSignature(msg []byte) (map[int]bool, map[int]int) {
	present := map[int]bool{}
	wire := map[int]int{}
	r := newPBReader(msg)
	for {
		f, ok, err := r.next()
		if err != nil || !ok {
			break
		}
		present[f.Num] = true
		wire[f.Num] = f.Wire
	}
	return present, wire
}

// decodeTraceFields decodes the traceMsg, mapping field numbers to proto names
// when the CTraceMsg type is known; otherwise it returns generic fN keys.
func decodeTraceFields(msg []byte, name string) map[string]any {
	names := traceFieldNames[name]
	out := map[string]any{}
	r := newPBReader(msg)
	for {
		f, ok, err := r.next()
		if err != nil || !ok {
			break
		}
		key := "f" + itoa(f.Num)
		if names != nil {
			if n, ok := names[f.Num]; ok {
				key = n
			}
		}
		switch f.Wire {
		case wireVarint, wireI32, wireI64:
			switch {
			case key == "script_type":
				appendField(out, key, scriptBlockName(f.V))
			case key == "template_card_id":
				appendField(out, key, f.V)
				if name := CardName(uint32(f.V)); name != "" {
					out["card_name"] = name
				}
			default:
				appendField(out, key, f.V)
			}
		case wireBytes:
			if isAttributeField(key) {
				appendField(out, key, attributesToJSON(decodeAttributes(f.Bytes)))
			} else {
				appendField(out, key, decodeBytesValue(f.Bytes, 4))
			}
		}
	}
	return out
}

// isAttributeField reports whether a (named) trace field carries a symbol-keyed
// attribute map (the card instance attributes), which we decode into named
// attributes rather than showing as raw bytes.
func isAttributeField(key string) bool {
	return key == "card_instance_attributes"
}

// attributesToJSON renders decoded attributes as a name->value (or #hash->value)
// map, surfacing zoneId (location), ownerId, etc.
func attributesToJSON(attrs []Attr) map[string]any {
	m := map[string]any{}
	for _, a := range attrs {
		if a.Name != "" {
			m[a.Name] = a.Value
		} else {
			m[a.Hash] = a.Value
		}
	}
	return m
}

// matchSignature classifies a message by its fields. For each candidate
// CTraceMsg signature we reject a mismatch (a bytes field the signature doesn't
// declare, a field within the signature's range the signature doesn't declare,
// or a declared bytes field that's present with the wrong wire type), then score
// by how many of the signature's fields are present. The highest-scoring
// signature wins; ties and weak matches are reported as ambiguous (""). This
// tolerates optional absent fields and trailing version-added fields while
// keeping bytes-field placement (highly distinctive) authoritative.
func matchSignature(present map[int]bool, wire map[int]int) string {
	if len(present) < 2 {
		return ""
	}
	msgBytes := map[int]bool{}
	for f, w := range wire {
		if w == wireBytes {
			msgBytes[f] = true
		}
	}
	best := ""
	bestScore := -1 << 30
	bestOverlap := 0
	tie := false
	for _, sig := range traceSignatures {
		sigSet := intSet(sig.fields)
		bytesSet := intSet(sig.bytesFields)
		if foreignBytes(msgBytes, bytesSet) || unexpectedInRange(present, sigSet, maxInt(sig.fields)) || badBytesType(sig.bytesFields, present, wire) {
			continue
		}
		ov := overlap(present, sigSet)
		// Reward present signature fields, penalize signature fields absent from
		// the message, so the closest-fitting (not merely largest) signature wins.
		score := 2*ov - len(sig.fields)
		switch {
		case score > bestScore:
			best, bestScore, bestOverlap, tie = sig.name, score, ov, false
		case score == bestScore:
			tie = true
		}
	}
	if tie || bestOverlap < 2 {
		return ""
	}
	return best
}

func intSet(xs []int) map[int]bool {
	m := make(map[int]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func maxInt(xs []int) int {
	m := 0
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

// foreignBytes reports whether the message has a bytes field the signature does
// not declare as bytes.
func foreignBytes(msgBytes, sigBytes map[int]bool) bool {
	for f := range msgBytes {
		if !sigBytes[f] {
			return true
		}
	}
	return false
}

// unexpectedInRange reports whether any present field within the signature's
// field-number range is not declared by the signature.
func unexpectedInRange(present, sigSet map[int]bool, max int) bool {
	for f := range present {
		if f <= max && !sigSet[f] {
			return true
		}
	}
	return false
}

// badBytesType reports whether a declared bytes field is present but not bytes.
func badBytesType(bytesFields []int, present map[int]bool, wire map[int]int) bool {
	for _, f := range bytesFields {
		if present[f] && wire[f] != wireBytes {
			return true
		}
	}
	return false
}

func overlap(present, sigSet map[int]bool) int {
	n := 0
	for f := range present {
		if sigSet[f] {
			n++
		}
	}
	return n
}

// SortTracesByObject is a stable helper for deterministic output.
func SortTracesByObject(ts []Trace) {
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].ObjectID < ts[j].ObjectID })
}
