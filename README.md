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
stdout), `-out <dir>` (batch, writes all four), `-objects` (per-state object
updates in the turn JSON), `-board` (running board snapshot per turn, default
on), `-payloads` (raw card hex + base64), `-chat`, `-summary`.

### Output files

- **`.info.json`** — the demo-info summary: match id, version, game mode,
  player roster (user id + index, joined with persona names from chat),
  decklists if present, record-type counts.
- **`.packets.jsonl`** — one JSON object per packet,
  `{"message":"packet","type":<EDCGGameMessages name>,"data":{…}}`. Known
  messages are decoded into **named fields with enum names** (Handoff,
  ReplayActionMarker → action string, VenchatoquistEvent, ConnectionStatus,
  BoardGameState → decoded objects); unknown types fall back to generic `fN`
  fields. The primary tool for exploring message contents.
- **`.json`** — turn-grouped state: Pregame → Initial Flop → per combat round,
  each with its ordered events and the running board snapshot.
- **`.trace.txt`** — `obtrace`-style phase blocks with the objects per state.

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

## Board-state decoding

The `payload` inside a `CMsgGameBoardGameState` is an uncompressed Source 2
schema-serialized snapshot, **the same serialization in both game versions**.
Game objects appear as a symbol-keyed stream; each is
`[symObjectID][object_id][symCard][len][card protobuf]`, present in both full
snapshots and deltas. The card protobuf decodes to `{owner_id, template_card_id,
attributes, …}`, so the parser reconstructs a running board (object id → latest
card state) and snapshots it per turn. Attribute keys are the game's attribute
name **hashes** (surfaced as hex) — resolving them to names needs the attribute
table, which isn't bundled.

## What's still open

- **1.0 per-action delta stream** — the length-delimited GC-message framing
  (board-state deltas, markers, full chat/decks). The scavenger and the
  generic packet dump are the tools to crack it; see `output/*.packets.jsonl`.
- **Object location** (hand / lane / fountain / graveyard) — lives in the
  per-object attribute map. The recurring hashes are surfaced (so they can be
  correlated), but their names need the game's attribute table, and the
  authoritative per-card zone/move events live in the payload's embedded
  "Trace Events" buffer (CardCreated, CardMovedToHand, … — same set as a real
  obtrace), which is the next decoding target.
- **Attribute deltas** — full snapshots decode cleanly; in *diff* states the
  attribute values are delta-encoded, so attribute decoding stops early
  (drift-guarded) rather than emit garbage. Object identity, owner, and card id
  are reliable in both.
- **Attribute-hash → name** and **card-id → card name** mapping — external data.

## Layout

```
cmd/replay-parser/   CLI
internal/dcg/        container framing, protobuf wire reader, message decoders,
                     board-state object decoding, turn grouping, dumps, outputs
protos/              reference .proto schemas (artifact = 1.0, artifact2 = 2.0)
test-replays/        sample replays
output/              generated dumps for the sample replays (study material)
```
