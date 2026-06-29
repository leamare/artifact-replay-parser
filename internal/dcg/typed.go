package dcg

// Typed, named decoding of each known EDCGGameMessages payload for the packet
// dump. Unlike the generic fN decoder, this maps fields to their proto names
// and enums to their string values. Unknown message types fall back to the
// generic decoder so nothing is hidden.

// TypedPacketData decodes a record payload into a named JSON object.
func TypedPacketData(rec Record) map[string]any {
	switch rec.Type {
	case MsgBoardGameState:
		return boardStateData(rec.Payload)
	case MsgReplayActionMark:
		return actionMarkerData(rec.Payload)
	case MsgHandoff:
		return handoffData(rec.Payload)
	case MsgVenchatoquist:
		return venchatData(rec.Payload)
	case MsgConnectionStatus:
		return connectionStatusData(rec.Payload)
	default:
		return DecodeGeneric(rec.Payload)
	}
}

// CMsgGameReplayActionMarker { action(1) }
func actionMarkerData(p []byte) map[string]any {
	id := DecodeActionMarker(p)
	return map[string]any{"action": ActionName(id), "actionId": id}
}

// CMsgGameHandoff { player_count(1), is_spectator(3), game_mode(4),
// players(5: {user_id(1), player_index(2)}), is_draft(6) }.
func handoffData(p []byte) map[string]any {
	h := DecodeHandoff(p)
	d := map[string]any{
		"player_count": h.PlayerCount,
		"game_mode":    h.GameMode,
		"is_spectator": h.IsSpectator,
	}
	players := make([]map[string]any, 0, len(h.Players))
	for _, pl := range h.Players {
		players = append(players, map[string]any{"user_id": pl.UserID, "player_index": pl.PlayerIndex})
	}
	if len(players) > 0 {
		d["players"] = players
	}
	m, _ := fieldMap(p)
	if v, ok := first(m, 6); ok {
		d["is_draft"] = v.V != 0
	}
	return d
}

// chatSourceName maps CMsgVenchatoquistEvent.EChatSource.
func chatSourceName(v uint32) string {
	switch v {
	case 1:
		return "eObjectID"
	case 2:
		return "ePlayer"
	case 3:
		return "eDeckImp"
	}
	return ""
}

// gestureName maps the 2.0 EGesture enum (field 8): board interactions, mostly
// hovering a card on/off.
func gestureName(v uint64) string {
	switch v {
	case 1:
		return "eLook"
	case 2:
		return "eStopLooking"
	case 3:
		return "eDrag"
	}
	return ""
}

// CMsgVenchatoquistEvent { text(1), object_id(2), chat_source(3),
// persona_name(4), user_id(5), line_id(6), toggle_mute(7), gesture(8) }.
// (1.0 carries the text in field 3 instead of field 1.)
func venchatData(p []byte) map[string]any {
	c := DecodeVenchat(p)
	d := map[string]any{}
	if c.Text != "" {
		d["text"] = c.Text
	}
	if c.ObjectID != 0 {
		d["object_id"] = c.ObjectID
	}
	if c.ChatSource != 0 {
		d["chat_source"] = enumStr(chatSourceName(c.ChatSource), c.ChatSource)
	}
	if c.PersonaName != "" {
		d["persona_name"] = c.PersonaName
	}
	if c.UserID != 0 {
		d["user_id"] = c.UserID
	}
	if c.LineID != 0 {
		d["line_id"] = c.LineID
	}
	m, _ := fieldMap(p)
	if v, ok := first(m, 7); ok && v.V != 0 {
		d["toggle_mute"] = v.V != 0
	}
	if v, ok := first(m, 8); ok {
		d["gesture"] = enumStr(gestureName(v.V), uint32(v.V))
	}
	return d
}

// CMsgGameServerToClientConnectionStatus { players(1: {player_id(1),
// status(2), inactivity_ticking(3), inactivity_ms_remaining(4),
// inactivity_anim_ms_remaining(5)}) }.
func connectionStatusData(p []byte) map[string]any {
	m, _ := fieldMap(p)
	players := make([]map[string]any, 0, len(m[1]))
	for _, f := range m[1] {
		pm, _ := fieldMap(f.Bytes)
		pd := map[string]any{}
		if v, ok := first(pm, 1); ok {
			pd["player_id"] = v.V
		}
		if v, ok := first(pm, 2); ok {
			pd["status"] = connStatusName(uint32(v.V))
		}
		if v, ok := first(pm, 3); ok && v.V != 0 {
			pd["inactivity_ticking"] = true
		}
		if v, ok := first(pm, 4); ok && v.V != 0 {
			pd["inactivity_ms_remaining"] = v.V
		}
		players = append(players, pd)
	}
	return map[string]any{"players": players}
}

func connStatusName(v uint32) any {
	switch v {
	case 1:
		return "Connected"
	case 2:
		return "Disconnected"
	}
	return v
}

// CMsgGameBoardGameState envelope + decoded trace events.
func boardStateData(p []byte) map[string]any {
	bs, _ := DecodeBoardState(p)
	traces := ExtractTraces(bs.Payload)
	d := map[string]any{
		"is_diff":             bs.IsDiff,
		"is_last":             bs.IsLast,
		"network_group":       bs.NetworkGroup,
		"unpacked_state_size": bs.UnpackedSize,
		"payload_len":         len(bs.Payload),
		"trace_count":         len(traces),
	}
	if len(traces) > 0 {
		d["traces"] = tracesToJSON(traces)
	}
	return d
}

// tracesToJSON renders decoded traces, classified by CTraceMsg name where known.
func tracesToJSON(traces []Trace) []map[string]any {
	out := make([]map[string]any, 0, len(traces))
	for _, t := range traces {
		td := map[string]any{}
		if t.Type != "" {
			td["type"] = t.Type
		} else {
			td["typeId"] = t.TypeID
		}
		if t.ObjectID != 0 {
			td["objectId"] = t.ObjectID
		}
		for k, v := range t.Fields {
			td[k] = v
		}
		out = append(out, td)
	}
	return out
}

// objectsToJSON renders decoded objects with named fields.
func objectsToJSON(objs []Object) []map[string]any {
	out := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		od := map[string]any{
			"object_id":        o.ObjectID,
			"template_card_id": o.TemplateCardID,
		}
		if o.OwnerID != nil {
			od["owner_id"] = *o.OwnerID
		}
		if len(o.Attributes) > 0 {
			attrs := map[string]any{}
			for _, a := range o.Attributes {
				attrs[a.Hash] = a.Value
			}
			od["attributes"] = attrs
		}
		if len(o.Flags) > 0 {
			od["flags"] = o.Flags
		}
		out = append(out, od)
	}
	return out
}

// enumStr renders an enum as its name when known, else the raw number.
func enumStr(name string, v uint32) any {
	if name != "" {
		return name
	}
	return v
}
