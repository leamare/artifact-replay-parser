package dcg

import (
	"os"
	"path/filepath"
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
		if r.MatchID == 0 {
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
		game := BuildGame(filepath.Base(path), r, BuildOptions{IncludeBoard: true})
		if len(game.Turns) < 3 {
			t.Errorf("%s: expected several turns, got %d", path, len(game.Turns))
		}
		// The final turn's board should contain decoded card objects.
		last := game.Turns[len(game.Turns)-1]
		var realCards int
		for _, o := range last.FinalBoard {
			if o.TemplateCardID >= 10000 {
				realCards++
			}
		}
		if realCards == 0 {
			t.Errorf("%s: no real cards decoded in final board", path)
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
