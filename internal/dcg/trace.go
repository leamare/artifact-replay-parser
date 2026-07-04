package dcg

// WriteTrace renders a replay's board-state trace stream in the indented,
// script-block style of the game's debug obtrace.txt. The block titles are the
// EScriptBlockType values carried by ScriptBlockBegin/End traces (PREGAME,
// INITIAL_FLOP, CARD_PLAY_SCRIPT, COMBAT, …); the leaf lines are the decoded
// CTraceMsg events, annotated with the card name when the subject object's card
// is known.
//
// Notes: the replay carries no wall-clock timestamps (unlike a live client
// obtrace), so block headers have none. Delta-encoded traces that we can't yet
// decode (NotParsed) are omitted here; they're visible in the packets dump.

import (
	"fmt"
	"sort"
	"strings"
)

// WriteTrace returns the obtrace-style text for a parsed container.
func WriteTrace(r *Reader) string {
	WarmTraceTable(r)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s  match %d  (%d records)\n", r.Version, r.MatchID, len(r.Records))
	if !r.ContainerDecoded {
		fmt.Fprintf(&b, "# %s\n", r.Note)
		return b.String()
	}

	objCard := map[uint64]uint32{} // objectId -> template card id, for naming
	depth := 0
	if r.Version == VersionClassic {
		// 1.0: render the whole-file trace stream (delta gaps included).
		// ScriptBlockEnd records are delta-compressed beyond recognition, so
		// blocks render as flat sections: each Begin closes the previous one.
		var traces []Trace
		for _, pt := range ExtractClassicFileTraces(r.buf) {
			tr := pt.Trace
			if tr.Type == "ScriptBlockBegin" && depth > 0 {
				traces = append(traces, Trace{Type: "ScriptBlockEnd"})
				depth--
			}
			traces = append(traces, tr)
			if tr.Type == "ScriptBlockBegin" {
				depth++
			}
		}
		depth = renderTraceStream(&b, traces, 0, objCard)
	} else {
		for _, rec := range r.Records {
			if rec.Type != MsgBoardGameState {
				continue
			}
			bs, _ := DecodeBoardState(rec.Payload)
			depth = renderTraceStream(&b, ExtractTraces(bs.Payload), depth, objCard)
		}
	}
	for depth > 0 { // close any blocks left open at end of stream
		depth--
		fmt.Fprintf(&b, "%s]\n", indentTrace(depth))
	}
	return b.String()
}

// renderTraceStream prints traces, opening/closing blocks on ScriptBlockBegin/
// End. depth is threaded across board states. It updates objCard from card
// creation/reveal traces so later lines can name objects.
func renderTraceStream(b *strings.Builder, traces []Trace, depth int, objCard map[uint64]uint32) int {
	for _, t := range traces {
		switch t.Type {
		case "NotParsed":
			continue
		case "ScriptBlockBegin":
			fmt.Fprintf(b, "%s%s [\n", indentTrace(depth), blockTitle(t))
			depth++
		case "ScriptBlockEnd":
			if depth > 0 {
				depth--
			}
			fmt.Fprintf(b, "%s]\n", indentTrace(depth))
		default:
			cid := cardIDOf(t)
			if cid != 0 && t.ObjectID != 0 {
				objCard[t.ObjectID] = cid
			}
			ref := objRef(t.ObjectID, objCard)
			if ref == "" && cid != 0 { // no object id recovered; name the card itself
				if name := CardName(cid); name != "" {
					ref = fmt.Sprintf(" -'%s'", name)
				} else {
					ref = fmt.Sprintf(" -card%d", cid)
				}
			}
			fmt.Fprintf(b, "%s%s%s%s\n", indentTrace(depth), t.DisplayType(),
				ref, traceFieldStr(t.Fields))
		}
	}
	return depth
}

func indentTrace(d int) string { return strings.Repeat(" .  ", d+1) }

// blockTitle is the script-block name (EScriptBlockType) for a block trace.
func blockTitle(t Trace) string {
	if v, ok := t.Fields["script_type"]; ok {
		return fmt.Sprintf("%v", v)
	}
	return "SCRIPT_BLOCK"
}

// cardIDOf returns the template card id a trace establishes for its object.
func cardIDOf(t Trace) uint32 {
	if v, ok := t.Fields["template_card_id"]; ok {
		if n, ok := toU64(v); ok {
			return uint32(n)
		}
	}
	return 0
}

// objRef renders an object reference, with the card name when known:
// -'Melee Creep'(obj:123) or (obj:123).
func objRef(id uint64, objCard map[uint64]uint32) string {
	if id == 0 {
		return ""
	}
	if cid := objCard[id]; cid != 0 {
		if name := CardName(cid); name != "" {
			return fmt.Sprintf(" -'%s'(obj:%d)", name, id)
		}
		return fmt.Sprintf(" -card%d(obj:%d)", cid, id)
	}
	return fmt.Sprintf("(obj:%d)", id)
}

// traceFieldStr renders a compact " k=v" list of a trace's scalar fields,
// skipping the object/template fields already shown in the object reference.
func traceFieldStr(fields map[string]any) string {
	if len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "template_card_id" || k == "card_name" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		switch v := fields[k].(type) {
		case uint64, int, int64, string:
			fmt.Fprintf(&sb, " %s=%v", k, v)
		}
	}
	return sb.String()
}

func toU64(v any) (uint64, bool) {
	switch n := v.(type) {
	case uint64:
		return n, true
	case int:
		return uint64(n), true
	case int64:
		return uint64(n), true
	}
	return 0, false
}
