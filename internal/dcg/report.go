package dcg

// Trace-type coverage report. It cross-references the trace-type ids actually
// observed in replays against the CTraceMsg_* messages in the proto, producing
// three groups so the still-unmapped type ids can be identified later (e.g. by
// recording a console obtrace alongside a reference replay):
//
//	known            type id -> CTraceMsg name (decoded)
//	unknown          type id seen but not named, with shape/context to identify it
//	tracesWithoutId  CTraceMsg names the proto declares but no type id mapped to
//
// It also emits a ready-to-edit metadata table (typeId -> name) for -tracetypes.

import (
	"sort"
)

// TraceReport is the JSON-serialisable coverage report.
type TraceReport struct {
	Files           []string                    `json:"files"`
	TotalTraces     int                         `json:"totalTraces"`
	Known           map[string]string           `json:"known"`           // typeId -> name
	Unknown         map[string]*UnknownTypeInfo `json:"unknown"`         // typeId -> details
	NotParsed       int                         `json:"notParsed"`       // tokens we couldn't parse at all
	TracesWithoutID []string                    `json:"tracesWithoutId"` // proto CTraceMsg with no observed id
}

// UnknownTypeInfo describes an observed-but-unnamed trace type id.
type UnknownTypeInfo struct {
	Count   int            `json:"count"`
	Shapes  []string       `json:"shapes"`            // distinct field-number signatures, e.g. "1,2,3"
	Blocks  []string       `json:"blocks"`            // distinct enclosing EScriptBlockType names
	Example map[string]any `json:"example,omitempty"` // a sample decoded field map
}

// BuildTraceReport aggregates a coverage report over one or more parsed
// replays. file labels each reader for the report.
func BuildTraceReport(readers map[string]*Reader) *TraceReport {
	rep := &TraceReport{
		Known:   map[string]string{},
		Unknown: map[string]*UnknownTypeInfo{},
	}
	// Per-type aggregation.
	type agg struct {
		name    string
		count   int
		shapes  map[string]bool
		blocks  map[string]bool
		example map[string]any
	}
	byID := map[uint64]*agg{}
	get := func(id uint64) *agg {
		a := byID[id]
		if a == nil {
			a = &agg{shapes: map[string]bool{}, blocks: map[string]bool{}}
			byID[id] = a
		}
		return a
	}

	for name, r := range readers {
		rep.Files = append(rep.Files, name)
		block := "" // current enclosing script block
		var stack []string
		for _, rec := range r.Records {
			if rec.Type != MsgBoardGameState {
				continue
			}
			bs, _ := DecodeBoardState(rec.Payload)
			for _, tr := range ExtractTraces(bs.Payload) {
				rep.TotalTraces++
				switch tr.Type {
				case "NotParsed":
					rep.NotParsed++
					continue
				case "ScriptBlockBegin":
					stack = append(stack, blockTitle(tr))
					block = stack[len(stack)-1]
					continue
				case "ScriptBlockEnd":
					if len(stack) > 0 {
						stack = stack[:len(stack)-1]
					}
					block = ""
					if len(stack) > 0 {
						block = stack[len(stack)-1]
					}
					continue
				}
				if tr.TypeID == 0 {
					continue // delta trace with no id; not useful for the id report
				}
				a := get(tr.TypeID)
				a.count++
				a.shapes[shapeKey(tr.Fields)] = true
				if block != "" {
					a.blocks[block] = true
				}
				if tr.Type != "" && tr.Type != "UnknownTrace" {
					a.name = tr.Type
				} else if a.example == nil && len(tr.Fields) > 0 {
					a.example = tr.Fields
				}
			}
		}
	}

	mappedNames := map[string]bool{}
	for id, a := range byID {
		if a.name != "" {
			rep.Known[utoa(id)] = a.name
			mappedNames[a.name] = true
		} else {
			rep.Unknown[utoa(id)] = &UnknownTypeInfo{
				Count:   a.count,
				Shapes:  sortedKeys(a.shapes),
				Blocks:  sortedKeys(a.blocks),
				Example: a.example,
			}
		}
	}
	// Proto traces with no observed id.
	for _, sig := range traceSignatures {
		if !mappedNames[sig.name] {
			rep.TracesWithoutID = append(rep.TracesWithoutID, sig.name)
		}
	}
	sort.Strings(rep.TracesWithoutID)
	sort.Strings(rep.Files)
	return rep
}

// TraceTypeTemplate returns a -tracetypes metadata table: every known id mapped,
// every observed-unknown id with an empty value to fill in.
func (rep *TraceReport) TraceTypeTemplate() map[string]string {
	out := map[string]string{}
	for id, name := range rep.Known {
		out[id] = name
	}
	for id := range rep.Unknown {
		out[id] = ""
	}
	return out
}

func shapeKey(fields map[string]any) string {
	// Use field count + names is overkill; the decoded fields are already named
	// when classified. For unknowns the keys are f1,f2,... — join them sorted.
	if len(fields) == 0 {
		return "(empty)"
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return joinComma(keys)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinComma(xs []string) string {
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += ","
		}
		s += x
	}
	return s
}
