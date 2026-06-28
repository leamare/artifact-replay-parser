package dcg

// WriteTrace renders a replay's record stream in the indented phase-block style
// of the game's debug obtrace.txt: each replay action marker opens a phase
// block and the messages that follow (board-state updates, chat, etc.) are
// listed beneath it until the next marker.
//
// Caveat: the per-card trace events (CardCreated, DamageAttempted, ...) seen in
// a real obtrace live inside the opaque schema-serialized board-state payload,
// which still needs the game's send-tables to decode. This output reconstructs
// the phase/marker skeleton and the state-update cadence, not those leaf events.

import (
	"fmt"
	"strings"
)

// WriteTrace returns the obtrace-style text for a parsed container.
func WriteTrace(r *Reader) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s  match %d  (%d records)\n", r.Version, r.MatchID, len(r.Records))
	if !r.ContainerDecoded {
		fmt.Fprintf(&b, "# %s\n", r.Note)
		return b.String()
	}

	open := false
	closeBlock := func() {
		if open {
			b.WriteString("]\n")
			open = false
		}
	}
	openBlock := func(name string) {
		closeBlock()
		fmt.Fprintf(&b, "%s [\n", name)
		open = true
	}

	// Everything before the first marker is the pregame setup block.
	openBlock("PREGAME")

	for _, rec := range r.Records {
		switch rec.Type {
		case MsgReplayActionMark:
			openBlock(ActionName(DecodeActionMarker(rec.Payload)))
		case MsgBoardGameState:
			bs, _ := DecodeBoardState(rec.Payload)
			kind := "FullState"
			if bs.IsDiff {
				kind = "StateDiff"
			}
			objs := ExtractObjects(bs.Payload, false)
			fmt.Fprintf(&b, " .  %s - %s objects=%d unpacked=%d packed=%d last=%v\n",
				kind, groupKey(bs.NetworkGroup), len(objs), bs.UnpackedSize, len(bs.Payload), bs.IsLast)
			for _, ob := range objs {
				owner := "-"
				if ob.OwnerID != nil {
					owner = itoa(int(*ob.OwnerID))
				}
				fmt.Fprintf(&b, " .   .  Object(%d) card=%d owner=%s attrs=%d\n",
					ob.ObjectID, ob.TemplateCardID, owner, len(ob.Attributes))
			}
		case MsgHandoff:
			h := DecodeHandoff(rec.Payload)
			fmt.Fprintf(&b, " .  Handoff - players=%d mode=%d spectator=%v\n",
				h.PlayerCount, h.GameMode, h.IsSpectator)
		case MsgConnectionStatus:
			fmt.Fprintf(&b, " .  ConnectionStatus - players=%d\n", ConnectionStatusPlayers(rec.Payload))
		case MsgVenchatoquist:
			// Skip the very frequent text-less gesture ("look at card") events;
			// only surface actual chat lines, matching the real obtrace.
			if c := DecodeVenchat(rec.Payload); c.Text != "" {
				fmt.Fprintf(&b, " .  Chat - %q (obj=%d)\n", c.Text, c.ObjectID)
			}
		default:
			fmt.Fprintf(&b, " .  %s\n", MsgName(rec.Type))
		}
	}
	closeBlock()
	return b.String()
}
