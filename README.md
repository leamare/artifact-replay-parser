# Artifact replay parser

Parses [Artifact](https://en.wikipedia.org/wiki/Artifact_(video_game)) (Valve's
DCG, a Source 2 title) replay files into a turn-grouped JSON state log, decoded
board objects, an `obtrace`-style phase trace, and exploratory packet dumps.

| Game | Ext | Container | Board state |
| --- | --- | --- | --- |
| Artifact 2.0 (Foundry) | `.dem` | **Fully decoded** | **Objects (cards/heroes/creeps/towers) with owner, template id, attributes** |
| Artifact 1.0 (Classic) | `.dcg` | Partial (keyframes + scavenged GC msgs) | Recovered from keyframe snapshots |

## Usage

```sh
go build ./cmd/replay-parser

# turn-grouped JSON to stdout
./replay-parser test-replays/2050565.dem

# everything per file into out/:  <name>.{json, trace.txt, info.json, packets.jsonl}
./replay-parser -summary -out out test-replays/*.dcg test-replays/*.dem
```

Flags: `-json`, `-trace`, `-demoinfo`, `-packets` (each takes a path or `-` for
stdout), `-out <dir>` (batch, writes all four), `-traces` (per-turn trace log in
the JSON, default on), `-cards <path>` (a card-name table, see below),
`-summary`.

### Output files

- **`.info.json`** — the demo-info summary: match id, version, game mode,
  player roster (user id + index, joined with persona names from chat),
  decklists if present, record-type counts.
- **`.packets.jsonl`** — one JSON object per line. Non-board-state records are
  `{"message":"packet","type":<EDCGGameMessages name>,"data":{…}}` with **named
  fields and enum names** (Handoff, ReplayActionMarker → action string,
  VenchatoquistEvent with `gesture`, ConnectionStatus). Board states are
  **exploded into their traces**: `{"message":"trace","type":<CTraceMsg name>,
  "objectId":…,"data":{…}}`; parseable-but-unclassified traces are
  `UnknownTrace`, delta-encoded ones `NotParsed`. The primary exploration tool.
- **`.json`** — `$.handoff` (players with personas), `$.objects` (index of every
  object id → `{cardId, cardName, owner}`), and `$.turns` (Pregame → Initial
  Flop → per combat round, each with its markers and decoded `traces`).
- **`.trace.txt`** — `obtrace`-style nested `EScriptBlockType` blocks
  (`INITIAL_FLOP`, `FLOP_DEPLOYMENT_TO_COMBAT`, `COMBAT`, …) with the decoded
  `CTraceMsg_*` leaf events, each annotated with the subject's card.

### Card names

The replay carries template card ids, not names. Supply a JSON table
(`{"<id>":"<name>"}`) via `-cards <path>` or the `DCG_CARDS` env var to render
card names in the traces, obtrace, and `$.objects`; otherwise ids are shown.

## Container format

Header (little-endian): **2.0** `u32 version(=7), u32 matchId`, 4 reserved bytes;
**1.0** `u32 crc, u32 version(=7), u32 matchId` (the leading CRC is the only
header difference).

**2.0 records** tile the file exactly, each
`[0xb6][u32 a][u16 type][u32 size][payload]`, where `type` is an
`EDCGGameMessages` id (2=BoardGameState, 5=Handoff, 6=ReplayActionMarker,
10=ConnectionStatus, 1001=VenchatoquistEvent). Turn structure comes from the
ReplayActionMarker stream (InitialFlopStart…End, then rounds of
LaneStart/PlayerAction*/LaneCombat/LaneEnd/Shopping/Turn…, ending
TowerDestroyed/MatchComplete).

**1.0 records** are mixed: periodic keyframe board states and some markers use
the same `0xb6` frame (~a few % of the file), while the bulk per-action stream
is a sequence of length-delimited protobuf GC messages with a framing still
being reverse-engineered. The parser recovers the keyframes and scavenges the
GC messages it can confidently classify (venchatoquist chat/personas), so player
identity and chat are recovered for 1.0 as well.

## Board-state decoding (trace events)

The `payload` inside a `CMsgGameBoardGameState` is the uncompressed Source 2
**"Trace Events" buffer** — a stream of records, each
`type=<id>  objectId=<id>  traceMsg=<serialized CTraceMsg_*>`. Field and type
names are keyed by Valve's `MakeStringToken` (murmur2, seed `0x31415926`) of the
lowercased name, so the hashes are reversible from a dictionary
(`internal/dcg/tokens.go`) — e.g. `0x7c48d532`=`objectId`, `0x3825eb34`=`zoneId`
(location), `0x62ae3b3e`=`traceMsg`. Each field is `[4-byte token][valtype][value]`.

Each `traceMsg` is a `CTraceMsg_*` event. There is no explicit type tag inside
it; instead:

- the record's **`type` id** is the game's stable internal trace enum, and
- the message is identified by its **protobuf field signature**
  (`internal/dcg/traces.go` + the generated `traces_data.go`), which is robust
  across versions and works in delta states too.

So the parser classifies traces (CardCreated, CardRevealed, DamageAttempted,
UnitEnteringCombat, UnitChangedCombatTarget/Position, PreviewDeathResultChanged,
ScriptBlockBegin/End, …) and decodes their fields under the proto names, with
`card_instance_attributes` expanded to named attributes (`zoneId`, `ownerId`, …)
and `script_type` rendered as its `EScriptBlockType` name. `ScriptBlockBegin`/
`End` reconstruct the nested block structure (`CARD_PLAY_SCRIPT`,
`UPDATE_OBJECT_ATTRIBUTES`, `BEGINNING_OF_SPELLCASTING`, …) that the obtrace
shows. Delta (`is_diff`) states carry no `type` id, so their traces are
classified by field signature plus a few high-confidence value patterns
(e.g. `UnitChangedCombatTarget` when `new_combat_target == old_combat_target`).
~84% of parseable trace records in the sample 2.0 replays classify; the rest are
surfaced as `UnknownTrace` with their fields.

## What's still open

- **Delta (diff) trace bodies** — in `is_diff` board states each `traceMsg` is
  delta-encoded against the prior state, so it can't be read as a standalone
  message yet (those are skipped rather than emitted as garbage). Full/keyframe
  states decode fully; cracking the delta opcodes unlocks the rest of the
  per-action traces.
- **Remaining ~16% of trace types** (`UnknownTrace`) — scalar-only signatures
  ambiguous by shape with no `type` id (delta states); surfaced with their fields.
- **Unknown attribute/token names** — surfaced as `#<hash>`; grow the wordlist
  in `tokens.go` (or feed the game's attribute table) to name more.
- **card-id → card name** mapping — supply a table via `-cards` (needs the
  external card set).
- **1.0 per-action delta stream** — the length-delimited GC-message framing;
  the scavenger + packet dump are the tools (see `output/*.packets.jsonl`).

## Layout

```
cmd/replay-parser/   CLI
internal/dcg/        container framing, protobuf wire reader, message decoders,
                     board-state object decoding, turn grouping, dumps, outputs
protos/              reference .proto schemas (artifact = 1.0, artifact2 = 2.0)
test-replays/        sample replays
output/              generated dumps for the sample replays (study material)
```
