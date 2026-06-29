package dcg

// Card-name resolution. The replay carries template card ids (e.g. 1003), not
// names. Mapping ids to names ("Melee Creep") needs the game's card set, which
// isn't bundled, so names are resolved through an optional, user-supplied table
// (a JSON object of {"<id>": "<name>"}). When no table is loaded, ids are still
// surfaced and names are simply omitted.

import (
	"encoding/json"
	"os"
	"strconv"
)

var cardNames = map[uint32]string{}

// CardName returns the display name for a template card id, or "" if unknown.
func CardName(id uint32) string { return cardNames[id] }

// LoadCardNames populates the card-name table from a JSON file mapping card id
// (as a string key) to name, e.g. {"1003":"Melee Creep","10038":"Storm Spirit"}.
func LoadCardNames(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	raw := map[string]string{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		if n, err := strconv.ParseUint(k, 10, 32); err == nil {
			cardNames[uint32(n)] = v
		}
	}
	return nil
}

// LoadTraceTypes seeds the trace-type-id table from a JSON file mapping the
// game's internal trace-type id (string key) to a CTraceMsg name, e.g.
// {"118":"CardMovedToHand","150":"CardKilled"}. Ambiguous-shape trace types
// can't be learned from the data alone, so this lets identified ids be supplied
// without code changes.
func LoadTraceTypes(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	raw := map[string]string{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		if n, err := strconv.ParseUint(k, 10, 64); err == nil {
			setTraceTypeID(n, v)
		}
	}
	return nil
}
