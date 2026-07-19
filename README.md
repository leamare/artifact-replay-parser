# Artifact replay parser

Parses [Artifact](https://en.wikipedia.org/wiki/Artifact_(video_game)) (Valve's
DCG, a Source 2 title) replay files into a turn-grouped JSON state log, decoded
board objects, an `obtrace`-style phase trace, and exploratory packet dumps.

| Game | Ext | Container | Board state |
| --- | --- | --- | --- |
| Artifact 2.0 (Foundry) | `.dem` | **Fully decoded** | **Objects (cards/heroes/creeps/towers) with owner, template id, attributes** |
| Artifact 1.0 (Classic) | `.dcg` | **Fully decoded** (whole-file Snappy) | **Full trace stream, objects, decks, GC metadata** |

See [`docs/REPLAY-FORMAT.md`](docs/REPLAY-FORMAT.md) for the container and
board-state format (both eras).

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
  **decks** (reconstructed per-owner card lists from the card-create events) and
  **deckCodes** (raw `ADC…`/`RTFACT…` base64 codes scavenged from the stream),
  record-type counts.
- **`.packets.jsonl`** — one JSON object per line, including a full
  **`boardState` snapshot at each turn end**: per player
  `{hand, deck, graveyard, fountain, lanes:[3]}` with cards as
  `{objectId, cardId, position, attackTarget, arrowDirection, flags}`
  (flags: revealed, locked, condemned, equippedTo). Non-board-state records are
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
**tracesWithoutId** (proto `CTraceMsg_*` no id mapped to). `metadata/` holds an
editable `tracetypes.json` (loadable via `-tracetypes`). Recording a console
obtrace alongside a reference replay lets the unknown ids be matched to names and
filled into `tracetypes.json`.

The trace tables (`internal/dcg/traces_data.go`) are generated from the 2.0
proto by `internal/dcg/gen_traces.py`.

## How Artifact saves replays (the big picture)

Artifact replays are **not** Source 2 DEM containers (no `CDemoFileHeader` — and
no match date, server name, network protocol or end time anywhere in the file;
verified by token and timestamp scans). They are **logged GC game messages**:
the client's `EDCGGameMessages` stream written to disk behind a tiny header.
The board-state messages carry two serialized **entity trees**:

- the **game-data entity** ("Card Game Data" + per-player "Private Player Info"
  + lanes/towers): match/game mode, gauntlet id, mana, shop level, initiative —
  and the **decklists**;
- the **trace-events entity** ("Trace Events"): the `CTraceMsg_*` stream that a
  console obtrace shows.

2.0 sends them as two network groups (group 1 = game data, group 2 = traces);
1.0 interleaves both in one stream with field-path delta encoding.

### Decks

Deck codes live in the game-data entity under the `decklist` murmur token:
`[token][valtype 0x01][u16 length][base64 code]`, followed by a second deck
field (token `0x85b82cd5`, source name unknown). Each player has both:
`decklist` = the **current** deck (post-flop it no longer contains the heroes —
why some codes look "incomplete"), the second field = the **full starting
code**. The delta serializer **elides the constant `ADCE` prefix** when a code
repeats an earlier value, which is why 1.0 opponent codes can surface as a bare
tail like `ZK04…==`; `RepairDeckCode` reconstructs them (verified byte-exact
against another replay that stores the same code in full).

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

**1.0 records** are mixed: periodic keyframe board states use the `0xb6` frame,
while the per-action data is a **byte-aligned field-path entity-update stream**
(verified byte-aligned, not bit-packed: top-16 byte values cover 38% vs ~6% for
random). Each keyframe holds **one** baseline object in the full 2.0 inline
murmur-token format (its card + attributes decode with the same grammar —
`ownerId`/`zoneId`/… resolve); every other object is a **field-path delta**
against that baseline, delineated by incrementing marker bytes (`4a`,`4b`,…) with
op-codes (`0x05` field-ref, `0x3e` tick, valtype `0x1d`) not yet fully pinned.

The parser decodes the recoverable structure (`internal/dcg/state_classic.go`):
each board state's **object ids** (op `0x10` + 5-byte varint), the **inline card
protobufs** (`08 <owner> 10 <template> 1a <attrs>`, validated by the ownerId/
zoneId token), and the embedded **zone names** (`Deck`,`Hand`,`Shop`,`Fountain`,
`Graveyard`,…). Plus **match id**, **players**, and **deck codes** by content.
So 1.0 `.json`/`.info.json` carry the board objects, cards, zones, players and
decks — not just a header.

What's **not** yet decoded is the full field-path op table needed to (a) recover
*every* object's template/attributes from the baseline delta (only objects with
an inline card get a template today) and (b) decode the per-action **delta event
stream** into the full trace log. That table is the remaining work; the most
direct way to pin it is **aligning a 1.0 console obtrace with its matching
replay** (ground truth), the same approach as naming the 2.0 type-ids.

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
delta-truncated shapes). Full states carry the id behind the `type` token;
**delta records keep the raw `[0x02][typeid]` immediately before the objectId
token**, so diffs are typed too. All **49 observed 2.0 type ids are mapped**
(`knownTraceTypeIDs2`, identified via proto field semantics, enclosing script
blocks, and the reference console logs in `test-replays/logs-20`); the enum maps
n:1 onto the `CTraceMsg` payloads (e.g. ids 2 and 150 are both `CardKilled` —
non-combat vs combat deaths). ~98% of the trace records in the sample 2.0
replays classify; `-tracetypes` can supply future ids.

**1.0 shares the same enum, shifted**: 2.0 inserted `GoldSpent` at 66, so a 1.0
type id ≥ 66 maps to the 2.0 name at id+2 (verified: 1.0 `ScriptBlockBegin` =
106 = 108−2). 1.0 traces come from a whole-file scan (the delta gaps included):
inline-token records give typed script blocks, and card-shaped protobufs give
every card event, so the 1.0 obtrace shows the real block/card structure.

## What's still open

- **1.0 scalar delta records** — 1.0 back-references even the record headers,
  so scalar traces (damage, gold, moves) in the delta gaps still need the
  back-reference op-table (`[XX][0x1c][len]`-style refs; coordinate space
  unknown). Card events, script blocks, decks, players and dates are recovered.
- **Unknown attribute/token names** — surfaced as `#<hash>`; grow the wordlist
  in `tokens.go` (or feed the game's attribute table) to name more.
- **card-id → card name** mapping — supply a table via `-cards` (needs the
  external card set).
- Some 1.0 files still expose only one **deck code** (the second is delta-elided
  in a form the repair doesn't catch); both players' decks are nonetheless
  reconstructed from the card events in every file.

## Layout

```
cmd/replay-parser/   CLI
internal/dcg/        container framing, protobuf wire reader, message decoders,
                     board-state object decoding, turn grouping, dumps, outputs
protos/              reference .proto schemas (artifact = 1.0, artifact2 = 2.0)
test-replays/        sample replays
output/              generated dumps for the sample replays (study material)
```
