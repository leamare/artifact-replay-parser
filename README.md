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
Likewise `-tracetypes <path>` supplies a `{"<typeId>":"<CTraceMsg name>"}` table
to name trace types whose stable internal id isn't in the built-in table yet.

### Trace-type coverage report

`-report <out.json>` (optionally `-tracetemplate <table.json>`), given one or
more replays, writes a coverage report grouping trace-type ids into **known**
(`id → CTraceMsg`), **unknown** (observed ids not yet named, with their field
shape, enclosing script block, and a sample to identify them), and
**tracesWithoutId** (proto `CTraceMsg_*` no id mapped to). `metadata/` holds a
generated `trace-report.json` and an editable `tracetypes.json` (loadable via
`-tracetypes`). Recording a console obtrace alongside a reference replay lets the
unknown ids be matched to names and filled into `tracetypes.json`.

The trace tables (`internal/dcg/traces_data.go`) are generated from the 2.0
proto by `internal/dcg/gen_traces.py`.

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
shows. In `is_diff` states each `traceMsg` is a clean protobuf **prefix** followed by a
delta trailer, so we parse the prefix (the leading run of valid fields) rather
than requiring a fully well-formed message; zero-length `traceMsg`s are valid
fieldless traces (DeathSyncPoint, ShopAction, …) identified by type id alone.
The stable **type-id table is authoritative** (it stays correct for
delta-truncated shapes); it's learned from full-state shapes and seeded with the
ids that no shape can disambiguate, with `-tracetypes` to add more. ~81% of the
~7k trace records in a sample 2.0 replay classify into 15+ types, with only a
handful left `NotParsed`; the rest are `UnknownTrace` tagged with their type id.

## What's still open

- **Remaining `UnknownTrace`** — trace types whose stable internal type id isn't
  mapped to a `CTraceMsg` name yet (their field shapes are ambiguous, so they
  can't be learned from the data). They're surfaced with their `typeId`; supply
  the mapping via `-tracetypes` as ids are identified.
- **Unknown attribute/token names** — surfaced as `#<hash>`; grow the wordlist
  in `tokens.go` (or feed the game's attribute table) to name more.
- **card-id → card name** mapping — supply a table via `-cards` (needs the
  external card set).
- **1.0 board-state serialization** — unlike 2.0 (which inlines the murmur
  tokens per record), 1.0 declares each field token once in a schema table and
  then encodes objects compactly by field-path/index (the Source2 entity-update
  format). Locating the keyframe states works, but decoding their records into
  traces needs a field-path decoder. The 1.0 per-action GC-message stream
  framing is likewise still partial (chat/personas are scavenged).

## Layout

```
cmd/replay-parser/   CLI
internal/dcg/        container framing, protobuf wire reader, message decoders,
                     board-state object decoding, turn grouping, dumps, outputs
protos/              reference .proto schemas (artifact = 1.0, artifact2 = 2.0)
test-replays/        sample replays
output/              generated dumps for the sample replays (study material)
```
