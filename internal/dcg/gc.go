package dcg

// GC match-metadata join. The replay archive ships each match's Game Coordinator
// response as data/blobs/<matchId>.json; joining it onto a parsed replay adds the
// authoritative players, heroes, tower health, outcome and timing that the replay
// stream itself doesn't carry cleanly.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// gcDir, when set, is the directory of GC blob responses (<matchId>.json). Set
// once by the CLI; consulted by BuildDemoInfo.
var gcDir string

// SetGCDir points the metadata join at a directory of GC blob responses.
func SetGCDir(dir string) { gcDir = dir }

// GCHero is a hero card in the GC roster, with its name resolved when known.
type GCHero struct {
	CardID   int    `json:"cardId"`
	CardName string `json:"cardName,omitempty"`
}

// GCMatch is the joined-in Game Coordinator match metadata.
type GCMatch struct {
	Player1      int64    `json:"player1,omitempty"`
	Player2      int64    `json:"player2,omitempty"`
	StartTime    int64    `json:"startTime,omitempty"`
	StartDate    string   `json:"startDate,omitempty"`
	Duration     int      `json:"duration,omitempty"`
	Turns        int      `json:"turns,omitempty"`
	MatchID      int64    `json:"matchId,omitempty"`
	Outcome      int      `json:"outcome,omitempty"`
	MatchMode    string   `json:"matchMode,omitempty"`
	TowerHealth1 []int    `json:"towerHealth1,omitempty"`
	TowerHealth2 []int    `json:"towerHealth2,omitempty"`
	Heroes1      []GCHero `json:"heroes1,omitempty"`
	Heroes2      []GCHero `json:"heroes2,omitempty"`
	Result       string   `json:"result,omitempty"`
}

// gcBlob is the raw on-disk GC response shape.
type gcBlob struct {
	Data struct {
		Player1      int64  `json:"player1"`
		Player2      int64  `json:"player2"`
		StartTime    int64  `json:"start_time"`
		Duration     int    `json:"duration"`
		Turns        int    `json:"turns"`
		MatchID      int64  `json:"match_id"`
		Outcome      int    `json:"outcome"`
		MatchMode    string `json:"match_mode"`
		TowerHealth1 []int  `json:"tower_health1"`
		TowerHealth2 []int  `json:"tower_health2"`
		Heroes1      []int  `json:"heroes1"`
		Heroes2      []int  `json:"heroes2"`
		Result       string `json:"result"`
	} `json:"data"`
	Success bool `json:"success"`
}

// LoadGCMatch reads and parses the GC response for a match id from dir. Returns
// nil when the blob is missing, unreadable, or a non-match response (the archive
// stores eResult_InvalidMatch placeholders for unavailable matches).
func LoadGCMatch(dir string, matchID uint32) *GCMatch {
	if dir == "" || matchID == 0 {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, strconv.FormatUint(uint64(matchID), 10)+".json"))
	if err != nil {
		return nil
	}
	var blob gcBlob
	if json.Unmarshal(b, &blob) != nil || blob.Data.MatchID == 0 {
		return nil
	}
	d := blob.Data
	m := &GCMatch{
		Player1: d.Player1, Player2: d.Player2,
		StartTime: d.StartTime, Duration: d.Duration, Turns: d.Turns,
		MatchID: d.MatchID, Outcome: d.Outcome, MatchMode: d.MatchMode,
		TowerHealth1: d.TowerHealth1, TowerHealth2: d.TowerHealth2,
		Heroes1: gcHeroes(d.Heroes1), Heroes2: gcHeroes(d.Heroes2),
		Result: d.Result,
	}
	if d.StartTime > 0 {
		m.StartDate = time.Unix(d.StartTime, 0).UTC().Format(time.RFC3339)
	}
	return m
}

func gcHeroes(ids []int) []GCHero {
	if len(ids) == 0 {
		return nil
	}
	out := make([]GCHero, len(ids))
	for i, id := range ids {
		out[i] = GCHero{CardID: id, CardName: CardName(uint32(id))}
	}
	return out
}
