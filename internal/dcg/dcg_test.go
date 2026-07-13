package dcg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func replayDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "test-replays")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("test-replays not available: %v", err)
	}
	return dir
}

// TestStringTokens verifies the murmur2 MakeStringToken against known symbols
// observed in the board-state serialization.
func TestStringTokens(t *testing.T) {
	cases := map[string]uint32{
		"objectid": 0x7c48d532,
		"ownerid":  0xa3c734e7,
		"zoneid":   0x3825eb34,
		"type":     0x1cf1719c,
		"tracemsg": 0x62ae3b3e,
	}
	for name, want := range cases {
		if got := MakeStringToken(name); got != want {
			t.Errorf("MakeStringToken(%q) = %#08x, want %#08x", name, got, want)
		}
	}
	// Dictionary resolves the location field.
	if TokenName(MakeStringToken("zoneId")) != "zoneId" {
		t.Errorf("zoneId token not resolved by dictionary")
	}
}

// TestFoundryTraces checks the board-state trace decoding classifies the
// distinctive gameplay traces and reconstructs the script-block structure.
func TestFoundryTraces(t *testing.T) {
	dir := replayDir(t)
	matches, _ := filepath.Glob(filepath.Join(dir, "*.dem"))
	if len(matches) == 0 {
		t.Skip("no .dem replays")
	}
	buf, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(matches[0], buf)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	var total, classified int
	for _, rec := range r.Records {
		if rec.Type != MsgBoardGameState {
			continue
		}
		bs, _ := DecodeBoardState(rec.Payload)
		for _, tr := range ExtractTraces(bs.Payload) {
			if !tr.IsParsed() { // skip delta-encoded NotParsed tokens
				continue
			}
			total++
			if tr.Type != "" {
				classified++
				seen[tr.Type]++
			}
		}
	}
	if total == 0 {
		t.Fatal("no traces decoded")
	}
	if classified*100/total < 50 {
		t.Errorf("trace classification too low: %d/%d", classified, total)
	}
	for _, want := range []string{"CardCreated", "ScriptBlockBegin", "ScriptBlockEnd", "DamageAttempted"} {
		if seen[want] == 0 {
			t.Errorf("expected to classify some %s traces", want)
		}
	}
}

// TestParseAll checks every sample replay parses without error and that the
// version/header is recognized.
func TestParseAll(t *testing.T) {
	dir := replayDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if ext != ".dem" && ext != ".dcg" {
			continue
		}
		seen++
		path := filepath.Join(dir, e.Name())
		buf, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Parse(e.Name(), buf)
		if err != nil {
			t.Errorf("%s: parse error: %v", e.Name(), err)
			continue
		}
		if r.Version == VersionUnknown {
			t.Errorf("%s: unknown version", e.Name())
		}
		// Locally-generated replays (bot/practice, "replay_<n>_*.dcg") carry no
		// GC match id; only GC-served replays are guaranteed one.
		if r.MatchID == 0 && !strings.HasPrefix(e.Name(), "replay_") {
			t.Errorf("%s: zero match id", e.Name())
		}
		game := BuildGame(e.Name(), r, BuildOptions{})
		if len(game.Turns) == 0 {
			t.Errorf("%s: no turns produced", e.Name())
		}
	}
	if seen == 0 {
		t.Skip("no replay files found")
	}
}

// TestFoundryStructure verifies the 2.0 container fully decodes: contiguous
// frames, a handoff, board states, action markers, and a sensible turn split.
func TestFoundryStructure(t *testing.T) {
	dir := replayDir(t)
	matches, _ := filepath.Glob(filepath.Join(dir, "*.dem"))
	if len(matches) == 0 {
		t.Skip("no .dem replays")
	}
	for _, path := range matches {
		buf, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Parse(path, buf)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if r.Version != VersionFoundry {
			t.Fatalf("%s: expected Foundry, got %s", path, r.Version)
		}
		if !r.ContainerDecoded {
			t.Fatalf("%s: container not decoded", path)
		}
		var markers, states, handoff int
		for _, rec := range r.Records {
			switch rec.Type {
			case MsgReplayActionMark:
				markers++
			case MsgBoardGameState:
				states++
			case MsgHandoff:
				handoff++
			}
		}
		if handoff == 0 {
			t.Errorf("%s: no handoff record", path)
		}
		if markers < 10 {
			t.Errorf("%s: too few markers: %d", path, markers)
		}
		if states < 50 {
			t.Errorf("%s: too few board states: %d", path, states)
		}
		game := BuildGame(filepath.Base(path), r, BuildOptions{IncludeTraces: true})
		if len(game.Turns) < 3 {
			t.Errorf("%s: expected several turns, got %d", path, len(game.Turns))
		}
		// The object index should contain decoded real cards.
		var realCards int
		for _, o := range game.Objects {
			if o.CardID >= 10000 {
				realCards++
			}
		}
		if realCards == 0 {
			t.Errorf("%s: no real cards decoded in object index", path)
		}
		// Some turn should carry a decoded trace log.
		var anyTraces bool
		for _, tn := range game.Turns {
			if len(tn.Traces) > 0 {
				anyTraces = true
			}
		}
		if !anyTraces {
			t.Errorf("%s: no traces in any turn", path)
		}
		// The demo info should recover the player roster.
		di := BuildDemoInfo(filepath.Base(path), r)
		if len(di.Players) == 0 {
			t.Errorf("%s: no players recovered", path)
		}
		// Board states should carry decoded envelope metadata.
		for _, rec := range r.Records {
			if rec.Type == MsgBoardGameState {
				bs, err := DecodeBoardState(rec.Payload)
				if err != nil {
					t.Errorf("%s: board state decode: %v", path, err)
				}
				if len(bs.Payload) == 0 {
					t.Errorf("%s: empty board-state payload", path)
				}
				break
			}
		}
	}
}
