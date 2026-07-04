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

import (
	"sort"
	"sync"
)

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
// whose field1 is an EScriptBlockType; 150: CardKilled {owner,is_creep,is_hero,
// is_combat}.)
var knownTraceTypeIDs2 = map[uint64]string{
	14:  "UnitChangedCombatPosition",
	16:  "UnitChangedCombatTarget",
	108: "ScriptBlockBegin",
	110: "ScriptBlockEnd",
	150: "CardKilled",
	82:  "PreviewDeathResultChanged", // {old,new death result, owner, is_creep/hero/tower}
	// Identified by script-block context + distinctive field structure:
	118: "ShoppingCardAppeared",  // SHOPPING_START; field1 is an EShoppingSlot
	144: "PreviewDamageChanged",  // UPDATE_COMBAT_PREVIEW; {old,new,owner}
	50:  "GainedAppliedEffectEx", // field4 = repeated {attr_token, value_diff}
	52:  "LostAppliedEffectEx",   // ditto, with negative value_diffs
	156: "RandomShopItemCreated", // SHOPPING; {owner_id, template_card_id}

	// Cross-identified from proto field semantics + enclosing block + the
	// reference console logs (test-replays/logs-20). Strong matches:
	54:  "ContinuousAppliedEffectChangedEx", // trio with 50/52; f4 = properties
	56:  "AttributeValueChanged",            // f1 = murmur attr token (huge), f2 = value
	24:  "UnitLeavingCombat",                // pairs with 22=UnitEnteringCombat; {lane, zone}
	40:  "CardMovedToDeployment",            // HERO_LANE_ASSIGNMENT; {lane, order, desired_pos=-1}
	64:  "GoldEarned",                       // {owner, source_obj, amount, available}
	66:  "GoldSpent",                        // pair with 64; negative amount
	70:  "UnitHealed",                       // {source_obj, heal, source_effect_card}
	74:  "TriggerTriggered",                 // {source_obj, source_effect_card, event_name}
	84:  "ColorsPlayableChanged",            // UPDATE_OBJECT_ATTRIBUTES; two color bitmasks
	88:  "CardDrawn",                        // {owner, turn, order, source, is_overdrawn}
	92:  "UnitTargeted",                     // {source_obj, target_group, group_number}
	94:  "UnitLocked",                       // {turns, source_obj}
	96:  "TowerManaRestored",                // {restored, attempted}
	104: "TowerManaSpent",                   // CARD_PLAY; {source_obj, mana}
	122: "ShopAction",                       // SHOPPING_UPDATE; empty
	126: "ShopCardRemoved",                  // SHOPPING; empty, frequent
	128: "ShopEnded",                        // SHOPPING_FINISHED; {owner}
	130: "CardBought",                       // {turn, order, is_drawn_shop_item, remaining}
	136: "PlayerResult",                     // GAME_OVER; {owner, has_won}
	152: "CasterCasting",                    // {caster_object_id, card_object_id}
	// Plausible (shape/context fit, weaker corroboration):
	48:  "CardMovedToGraveyard",         // CARD_PLAY/DEATH_CHECK; empty
	78:  "UnitUserTargeted",             // CARD_PLAY/HERO_LANE_ASSIGNMENT; empty
	80:  "EffectProcced",                // {source_object_id}
	98:  "CardPlayed",                   // CARD_PLAY/GENERIC_TRIGGER; empty
	102: "UnitCondemned",                // {source_object_id}
	114: "PlayerPassed",                 // {player, pass_to_combat, had_actions}
	132: "PrepareHeroForLaneAssignment", // PREPARE_HEROES block
	140: "ChargesChanged",               // GENERIC_TRIGGER; {delta, new}
	68:  "CardDuplicated",               // {source_obj, copied_obj, effect_card, lane, zone}
	112: "StateBasedEffectsUpdateStart", // SBE block; empty
	// The internal trace enum maps n:1 onto the CTraceMsg payloads — 2 and 150
	// are both CardKilled (non-combat deaths with damageContributors vs combat
	// deaths).
	2:   "CardKilled",
	4:   "TowerKilled",           // {owner, is_combat, is_defeat_for_owner, damageContributors}
	138: "CardMovedToSpellStack", // CARD_PLAY; empty (spell entering the stack)
}

// learnedTypeIDs accumulates type-id -> name across every payload parsed in the
// process, seeded from knownTraceTypeIDs2. Full states confidently classify a
// type id by field shape; that mapping then resolves the same type id when it
// recurs in delta states (which carry the id but an ambiguous/partial shape).
var (
	learnedTypeIDsMu sync.Mutex
	learnedTypeIDs   = func() map[uint64]string {
		m := map[uint64]string{}
		for k, v := range knownTraceTypeIDs2 {
			m[k] = v
		}
		return m
	}()
)

func learnTypeID(id uint64, name string) {
	if id == 0 || name == "" {
		return
	}
	learnedTypeIDsMu.Lock()
	if _, ok := learnedTypeIDs[id]; !ok {
		learnedTypeIDs[id] = name
	}
	learnedTypeIDsMu.Unlock()
}

func lookupTypeID(id uint64) string {
	learnedTypeIDsMu.Lock()
	defer learnedTypeIDsMu.Unlock()
	return learnedTypeIDs[id]
}

// setTraceTypeID records an id -> name mapping (used by LoadTraceTypes).
func setTraceTypeID(id uint64, name string) {
	learnedTypeIDsMu.Lock()
	learnedTypeIDs[id] = name
	learnedTypeIDsMu.Unlock()
}

// WarmTraceTable runs a learning pass over every board-state payload so the
// type-id table is fully populated before any output is emitted. Without this,
// records early in the stream would miss names that are only shape-learned from
// later full states (extraction is otherwise single-pass).
func WarmTraceTable(r *Reader) {
	for _, rec := range r.Records {
		if rec.Type != MsgBoardGameState {
			continue
		}
		bs, _ := DecodeBoardState(rec.Payload)
		ExtractTraces(bs.Payload) // side effect: learnTypeID
	}
}

// ExtractTraces pulls every trace record out of a board-state payload (full or
// diff). It scans for the traceMsg token, reads the length-delimited CTraceMsg,
// and pairs it with the nearest preceding objectId and type id. Each trace is
// classified by field signature; records ambiguous by shape are resolved via
// their stable type id (seeded table + propagation from confidently-typed
// records), and the fields are then decoded under the resolved type.
func ExtractTraces(payload []byte) []Trace {
	type raw struct {
		tr         Trace
		msg        []byte
		shapeClass string // confident classification by field signature
	}
	var rows []raw

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
		tr := Trace{
			ObjectID: nearestObjectID(payload, j),
			TypeID:   nearestTypeID(payload, j),
		}
		// A zero-length traceMsg is a valid fieldless trace (the proto has many
		// empty CTraceMsg_* like DeathSyncPoint/ShopAction/CardMovedToGraveyard);
		// it's identified solely by its type id, not NotParsed.
		if ln == 0 {
			rows = append(rows, raw{tr: tr, msg: []byte{}})
			i = no
			continue
		}
		// The declared length is a container that, in delta states, includes a
		// trailer past the message; the message itself is the clean protobuf
		// prefix. Parse that prefix (it equals the whole content in full states).
		prefix, ok := parseTracePrefix(payload[no : no+int(ln)])
		if !ok {
			// Delta damage can prepend junk bytes; retry from the next few
			// offsets before giving up.
			for skip := 1; skip <= 3 && !ok; skip++ {
				if no+skip < no+int(ln) {
					prefix, ok = parseTracePrefix(payload[no+skip : no+int(ln)])
				}
			}
		}
		if !ok {
			// With a valid type id this is a real (typed) record whose body was
			// delta-mangled; without one, the "token" is a byte coincidence in
			// an unconsumed delta trailer, not a record — skip it silently.
			if tr.TypeID != 0 {
				tr.Type = "NotParsed" // resolved to its name below when known
				rows = append(rows, raw{tr: tr})
			}
			i = j + 1
			continue
		}
		row := raw{tr: tr, msg: prefix}
		row.shapeClass = classifyShapeOnly(prefix)
		if row.shapeClass != "" {
			// A confident shape match teaches the stable type-id its name, so the
			// same type id resolves later in delta states that lack a full shape.
			learnTypeID(tr.TypeID, row.shapeClass)
		}
		rows = append(rows, row)
		i = no + int(ln)
	}
	// Resolve each trace, preferring the stable type-id table (authoritative,
	// and correct even for delta-truncated shapes) over the per-message shape,
	// then the value heuristic (which uses the enclosing script block as
	// context, tracked from the resolved Begin/End records). Decode fields
	// under the resolved type.
	traces := make([]Trace, 0, len(rows))
	block := ""
	prevName := ""
	var blockStack []string
	for _, r := range rows {
		if r.msg == nil { // unparseable body
			// A valid type id still names the trace; only the fields are lost.
			// Ids not yet in the table surface as UnknownTrace with the id.
			if name := lookupTypeID(r.tr.TypeID); name != "" {
				r.tr.Type = name
			} else {
				r.tr.Type = ""
			}
			traces = append(traces, r.tr)
			continue
		}
		name := lookupTypeID(r.tr.TypeID)
		if name == "" {
			name = r.shapeClass
		}
		if name == "" {
			name = classifyByValue(r.msg, block, prevName)
		}
		if name == "" && soleShortBytesField(r.msg) && r.tr.TypeID == 0 {
			continue // delta fragment (a stray length-delimited read), not a record
		}
		if name == "" && singleHugeValue(r.msg) {
			// A lone huge scalar is a mangled varint read. With a type id the
			// record is still typed (fieldless — a clean copy follows in the
			// next state refresh); without one it's a delta fragment: drop it.
			if idName := lookupTypeID(r.tr.TypeID); idName != "" {
				r.tr.Type = idName
				r.tr.Fields = nil
				traces = append(traces, r.tr)
			} else if r.tr.TypeID != 0 {
				r.tr.Type = "" // unknown id: surface for the metadata table
				r.tr.Fields = nil
				traces = append(traces, r.tr)
			}
			continue
		}
		r.tr.Type = name
		if len(r.msg) > 0 { // fieldless (empty) traces carry no fields
			r.tr.Fields = decodeTraceFields(r.msg, name)
		}
		switch name {
		case "ScriptBlockBegin":
			if v, ok := r.tr.Fields["script_type"].(string); ok {
				blockStack = append(blockStack, v)
				block = v
			}
		case "ScriptBlockEnd":
			if len(blockStack) > 0 {
				blockStack = blockStack[:len(blockStack)-1]
			}
			block = ""
			if len(blockStack) > 0 {
				block = blockStack[len(blockStack)-1]
			}
		}
		if name != "" {
			prevName = name
		}
		traces = append(traces, r.tr)
	}
	return traces
}

// singleHugeValue reports whether msg decodes to exactly one scalar exceeding
// 32 bits (the signature of a corrupted varint read).
func singleHugeValue(msg []byte) bool {
	vals := firstScalarValues(msg)
	return len(vals) == 1 && vals[1] > 1<<35
}

// parseTracePrefix returns the leading run of msg that forms a valid protobuf
// trace: fields with plausible numbers (1..20) and consistent wire types. In
// delta states this strips the trailing delta bytes; in full states it returns
// the whole message. ok is false if not even one field parses.
func parseTracePrefix(msg []byte) ([]byte, bool) {
	r := newPBReader(msg)
	seen := map[int]int{}
	end := 0
	for {
		f, ok, err := r.next()
		if err != nil || !ok {
			break
		}
		if f.Num <= 0 || f.Num > 20 {
			break
		}
		if w, dup := seen[f.Num]; dup && w != f.Wire {
			break
		}
		seen[f.Num] = f.Wire
		end = r.pos
	}
	if end == 0 {
		return nil, false
	}
	return msg[:end], true
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
// using distinctive scalar value patterns (delta records can lose their
// trailing fields, so truncated forms are matched too). block is the enclosing
// script-block name and prev the previous resolved trace, both "" when unknown;
// they give context for the combat-preview cluster whose deltas lose fields.
func classifyByValue(msg []byte, block, prev string) string {
	present, wire := messageSignature(msg)
	for _, w := range wire {
		if w == wireBytes {
			return "" // these patterns are scalar-only
		}
	}
	vals := firstScalarValues(msg)
	// Object ids are drawn from the full uint31 range; the low bound just has
	// to clear small scalars/enums. Murmur attribute tokens overwhelmingly land
	// above the uint31 ceiling.
	isObj := func(v uint64) bool { return v >= 1_000_000 && v < 1<<31 }
	inPreview := block == "UPDATE_COMBAT_PREVIEW" ||
		prev == "PreviewDeathResultChanged" || prev == "PreviewDamageChanged" ||
		prev == "UnitChangedCombatTarget" // target updates run inside the preview
	switch {
	// UnitChangedCombatTarget: {new_target, lane, old_arrow, arrow, old_target}
	// with new_target == old_target (the two combat-target object ids match).
	case exactly(present, 1, 2, 3, 4, 5) && vals[1] == vals[5] && vals[1] > 1000:
		return "UnitChangedCombatTarget"
	// … delta-truncated to 4 fields: {target(objid), lane 1..3, arrows 0..3}.
	case exactly(present, 1, 2, 3, 4) && isObj(vals[1]) &&
		vals[2] >= 1 && vals[2] <= 3 && vals[3] <= 3 && vals[4] <= 3:
		return "UnitChangedCombatTarget"
	// UnitChangedCombatPosition: {new_pos, old_pos(int32), owner, lane} where a
	// unit entering combat has old_combat_position == -1.
	case exactly(present, 1, 2, 3, 4) && vals[2] == negOne:
		return "UnitChangedCombatPosition"
	// AttributeValueChanged: f1 is a murmur attribute token — a hash, so it
	// lands above the object-id range with high probability.
	case (exactly(present, 1, 2) || exactly(present, 1, 2, 3)) && vals[1] >= 1<<31:
		return "AttributeValueChanged"
	// The combat-preview cluster alternates PreviewDeathResultChanged (id-typed)
	// with PreviewDamageChanged; small-scalar 1-3 field records adjacent to a
	// preview trace are the damage preview (owner/new fields delta-truncated).
	case inPreview && len(present) <= 3 && allSmall(vals, 100):
		return "PreviewDamageChanged"
	// A bare {old, new} small pair with no type id only occurs as the flood of
	// damage-preview updates in diffs (real 2-field scalar traces carry their
	// type id); context may be lost at a payload boundary.
	case exactly(present, 1, 2) && allSmall(vals, 100):
		return "PreviewDamageChanged"
	// {owner, card template id}: the shop stocking a random item.
	case exactly(present, 1, 2) && vals[1] < 4 && vals[2] >= 1000 && vals[2] <= maxTemplateCardID:
		return "RandomShopItemCreated"
	}
	return ""
}

// allSmall reports whether every value is below limit.
func allSmall(vals map[int]uint64, limit uint64) bool {
	for _, v := range vals {
		if v >= limit {
			return false
		}
	}
	return true
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

// nearestTypeID finds the trace-type id preceding the traceMsg token. Full
// states carry it behind the `type` token; delta states drop the token but
// keep the raw [0x02 valtype][typeid varint] immediately before the objectId
// token, so that is the fallback.
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
	if last >= 0 {
		v, _ := uvarint(p, last+5)
		return v
	}
	// Delta fallback: locate the objectId token of this record, then read the
	// varint that ends right before it (1- or 2-byte), validating the range.
	oidTok := tokenLE(MakeStringToken("objectId"))
	oid := -1
	for o := start; o < pos; o++ {
		if o+5 <= len(p) && string(p[o:o+4]) == string(oidTok) {
			oid = o
		}
	}
	if oid < 1 {
		return 0
	}
	return typeIDBefore(p, oid)
}

// typeIDBefore reads the trace-type id varint that ends at position end
// (exclusive). Type ids are small even numbers, encoded as one byte (< 0x80)
// or two bytes (0x80.. 0x01 for 128..155).
func typeIDBefore(p []byte, end int) uint64 {
	if end >= 2 && p[end-1] < 0x80 && p[end-1] >= 2 {
		v := uint64(p[end-1])
		if plausibleTypeID(v) {
			return v
		}
	}
	if end >= 3 && p[end-1] == 0x01 && p[end-2] >= 0x80 {
		v := uint64(p[end-2]&0x7f) | 1<<7
		if plausibleTypeID(v) {
			return v
		}
	}
	return 0
}

// plausibleTypeID bounds the game's trace-type enum: small even ids.
func plausibleTypeID(v uint64) bool { return v >= 2 && v <= 200 && v%2 == 0 }

// tokenLE returns a token as its 4 little-endian wire bytes.
func tokenLE(tok uint32) []byte {
	return []byte{byte(tok), byte(tok >> 8), byte(tok >> 16), byte(tok >> 24)}
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

// refreshDedupe suppresses the re-emission of trace records that full-state
// refreshes carry: the append→truncate→refresh cycle re-serializes the last N
// records verbatim, so a refresh contributes a record only when it wasn't
// already emitted (which also repairs records whose diff body was mangled).
type refreshDedupe struct {
	recent []([2]uint64) // (typeId, objectId) ring of recent emissions
}

const refreshWindow = 64

func (d *refreshDedupe) remember(tr Trace) {
	d.recent = append(d.recent, [2]uint64{tr.TypeID, tr.ObjectID})
	if len(d.recent) > refreshWindow {
		d.recent = d.recent[len(d.recent)-refreshWindow:]
	}
}

func (d *refreshDedupe) seen(tr Trace) bool {
	key := [2]uint64{tr.TypeID, tr.ObjectID}
	for _, k := range d.recent {
		if k == key {
			return true
		}
	}
	return false
}

// soleShortBytesField reports whether msg decodes to exactly one short bytes
// field — the signature of a stray delta fragment misread as a message.
func soleShortBytesField(msg []byte) bool {
	present, wire := messageSignature(msg)
	return len(present) == 1 && present[1] && wire[1] == wireBytes
}
