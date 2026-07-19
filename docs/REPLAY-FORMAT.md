# Artifact replay format

Covers both eras of Valve's DCG (a Source 2 title). Both share one container and
one board-state serialization; they differ mainly in the trace-type enum.

| Era | Ext | Notes |
| --- | --- | --- |
| 1.0 Classic | `.dcg` | 2018 launch build. GC-archive copies are whole-file Snappy-compressed. |
| 2.0 Foundry | `.dem` | 2020–21 beta build. Same framing, different trace enum. |

Both are **version-7** streams, **not** PBDEMS2 demos. The `.dcg`/`.dem`
extension does not identify the era — the build does (see *Version detection*).

## Container

```
[u32 crc/len] [u32 version = 7] [u32 matchId] ... records ...
```

Records are `0xb6`-framed length-delimited `EDCGGameMessages` (BoardGameState=2,
Handoff=5, ChatMsg=7/8, Venchatoquist=1001, …).

**Whole-file Snappy:** GC-served archive `.dcg` files are Google-Snappy over the
*entire* file; the leading 4 bytes are the Snappy varint uncompressed length.
`snappy.Decode` yields the plain version-7 container. Detected by: first `u32 !=
7`, decompresses, and the result starts with `7`.

## Board state = a codec map ("Trace Events" buffer)

`CMsgGameBoardGameState.payload` is **not** protobuf/KV3. It is a flat codec map
after a 2-byte `01 02` prefix:

```
map := ( [u32 key] [u8 type] [value] )*   until key == 0
key := MakeStringToken(name) = murmur2(lowercase(name), seed 0x31415926)
```

Key tokens (little-endian on the wire): `type`=`9c71f11c`, `objectId`=`32d5487c`,
`traceMsg`=`3e3bae62`. Value types (jump table, bound `<= 0x27`):

| type | encoding |
| --- | --- |
| `0x1d` | blob: varint len + bytes (holds a `CTraceMsg` protobuf) |
| `0x10` | fixed `u32` |
| `1` | string: `u16` len + bytes |
| `6` / `7` | nested map / array |
| others (`2`,`0x11`,`0x17`,`0x1f`,…) | varint (7-bit groups) |

The buffer is the game's **trace-event stream**: an entity header + a run of
trace records, each `{ type=<id>, objectId=<id>, traceMsg=<CTraceMsg protobuf> }`.

## Traces

`traceMsg` carries no self-tag; the record's `type` id (an even internal enum)
plus the protobuf field signature identify the `CTraceMsg_*` (CardCreated,
DamageAttempted, GoldEarned, ScriptBlockBegin/End, …). We classify by the stable
type id, falling back to field-shape and enclosing script block.

**The enum differs per build** — the id→name table is not derivable from proto
order and must be keyed on the build:

| | 1.0 Classic | 2.0 Foundry |
| --- | --- | --- |
| table | `classicTypeIDs` | `knownTraceTypeIDs2` |
| ScriptBlockBegin / End | 106 / 108 | 108 / 110 |
| CardPlayed | 76 | (76 = DamageAttempted) |
| UnitTargeted | 90, 92 | 92 |

Build is detected per replay from the ScriptBlock id voting (Begin=106 ⇒
Classic). `EScriptBlockType` names are shared 1–35 across builds.

### Full vs diff states

Full states (`is_diff=false`) decode cleanly. Diff states carry only changed
fields and delta-mangle: a trace body can lose its type/objectId, and blob
fields (`card_template_data`, `card`) are delta-encoded so the full-state codec
over-reads and drifts. Guards: records with neither `type` nor `objectId` are
misalignment (a `traceMsg`-token byte-match inside blob data) and dropped; blob
decode runs without resync and keeps only resolved-name, plausible fields.

### Attribute tokens

`card_instance_attributes` and applied-effect `properties` are murmur-keyed
attribute maps. The names are compiled out of the binary by `MakeStringToken`,
so they are recovered by hashing candidates: engine field names
(`hitPoints`, `attackPower`, `currentArmor`, `turnCardEnteredHand`, …), and
per-card effect attributes via `effect_source_card_id` → card keyword/mechanic
(`Feeble`, `isOathed`, `hasModifiedBounty`, `tracked`). Confirmed patterns:
keyword→participle, aura→`hasModified<Stat>`/`<stat>Modifier`, membership→`is<X>`.

## What is decoded

- **Traces** — full per-turn event stream, classified by type, for both eras.
- **Objects** — id → {cardId, owner, attributes, zone} from full-state snapshots
  + the CardCreated stream.
- **Board snapshots** — per-object zone/lane/combat state tracked over the trace
  stream, emitted per turn.
- **Decks / players / match date** — from decklists, Handoff, chat timestamps.
- **GC metadata** — archive blob join via `-gc` (players, heroes, outcome, …).

## Boundary

Diff-state per-attribute deltas (health/mana/position over time inside the
index-referenced records) are delta-encoded against a runtime positional schema
whose per-field wire sizes live in a `.bss` table absent from the static binary.
The self-describing layer (the `0x1d` `CTraceMsg` blobs, keyed fields, and
full-state snapshots) is what the per-file output is built from.
