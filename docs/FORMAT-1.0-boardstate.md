# Artifact 1.0 board-state format (reverse-engineered)

This documents the on-disk board-state serialization used by Artifact 1.0
(Classic, `.dcg`), established by static analysis of `libserver.so` (Ghidra) and
by matching against the replay bytes. It records both what is decoded today and
the precise boundary of what remains.

## Container

`.dcg` header: `u32 crc, u32 version(=7), u32 matchId`. Board-state keyframes are
`0xb6`-framed; the bulk of per-action data is the byte-aligned state stream.

## The board state is a custom Source2 "AttributeList" typed tree

The `CMsgGameBoardGameState.payload` bytes are **not** protobuf and **not**
standard KV3 (verified: the KV3 loader rejects the payload — no `VKV3`/encoding
header). It is Valve's Source2 **AttributeList**: a typed key→value tree.

Pipeline (server): game objects → `dcg_game_state_builder.cpp` builds an
in-memory AttributeList tree → wire serialize → `payload` bytes → replay.

### In-memory node / field layout (from decompilation)

- A parsed container is a `std::vector` of field records, **stride `0x40`**:
  - `+0x00` `u32` field key = `MakeStringToken(name)` (murmur2, seed
    `0x31415926`, lowercased name)
  - `+0x08` count/flag
  - `+0x10` `u32` value kind (0..0x27 — the schema "kind")
  - `+0x18` value
- Value nodes are a tagged tree: node type = `(byte0 >> 1) & 0xf`
  (2=bool/int8, 3=i32, 4=u32, 5=double, 6=indirect/string, 9=object/map,
  0xa/0xb=string, 0xc=null); map lookups are by murmur key.
- Lookup: `FUN_013b0fb0(node, token)`; typed getters `FUN_013b1030` (int),
  `FUN_013b15a0` (object), `FUN_013b10e0` (bool).

### Wire framing

- Keys are **interned**: a key is written inline as a 4-byte murmur token on
  first use, then referenced by a **1-byte index** afterwards. In a keyframe the
  `type` and `traceMsg` tokens appear exactly once; the other ~39 records
  reference them by index.
- A keyframe declares its record count up front:
  `[murmur "count" token][kind][u8 count]` (observed: 40).
- Value encoding is **schema-reflective**: the wire "kind" byte selects a schema
  kind whose read size comes from a **runtime-initialized** descriptor table
  (`DAT_01ee1928`, 40 entries, stride `0x38`, built by `_INIT_44`). This table
  is `.bss` — it does not exist in the static file. Fixed-size kinds are
  therefore **not self-describing on the wire**.

## What this parser decodes today

Everything self-describing is recovered (see `state_classic.go`,
`traces_classic.go`):

- **Object ids** (`0x10` + 5-byte varint), and card templates for the inline /
  card-proto records, merged with the card-create trace stream.
- **Card events**: `CardCreated` (owner, template, attributes), plus the
  typed-scalar keyframe records (`CardKilled`, `TowerKilled`,
  `UnitChangedCombatTarget/Position`, `CardMovedToDeployment`, …).
- **Script-block sections** (`EScriptBlockType`), with canonical-chain
  completion for the delta-hidden neighbours.
- **Zones / containers**: Deck, Hand, Shop, Fountain, Graveyard, Hidden, Public,
  Card Game Data, Private Player Info, Script Lock, Trace Events.
- **Decks** (reconstructed per owner), **deck codes**, **players**, **match
  date**.

Per file this yields ~58–109 exact traces.

## The boundary (what is not decoded)

The ~39 index-referenced records per keyframe cannot be fully reconstructed from
the replay alone, because:

1. Per-kind **wire sizes** live in the runtime schema table (`.bss`), absent
   from the static binary; and
2. The custom AttributeList **wire codec** is inlined/templated — it has no
   named function or source-file string in `libserver.so`/`libclient.so`.

So fixed-size fields inside the indexed records are not statically recoverable.
Closing this needs runtime introspection: `libserver.so` `dlopen`s standalone
(its static initializers populate the schema table), so a runtime dump of
`DAT_01ee1928` or of a parsed tree — or a gdb breakpoint in the running game —
yields the per-kind sizes and completes the decode.

## Update: the value codec (reverse-engineered from libclient)

The client reads board-state values with `FUN_00d165e0` (libclient.so), a
recursive reader over a byte buffer:

```
value := [u32 key = MakeStringToken(name)] [u8 type] [data]
```

Types (jump table at ELF `0x1bba2c8`, 40 entries, bound `<= 0x27`):

| type | encoding |
| --- | --- |
| most (2, 5, 0xa, 0x1f, …) | varint (7-bit groups, 0x80 continuation) |
| `0x1d` | varint length + that many bytes (a blob — the `CTraceMsg` protobuf) |
| `0x10` | fixed `u32` (4 bytes) |
| `1` | string: `u16` length (< 0x400) + bytes |
| `6` | nested map (recurse until key == 0) |
| `7` | array: `u32` count + elements |

Verified against the bytes: `9c71f11c 02 6a` = key `type` + varint `106`;
`3e3bae62 1d 04 08031801` = key `traceMsg` + blob(4) = the ScriptBlockBegin
proto `{script_type: 3, object_id: 1}`.

A keyframe payload begins with a `count` field (record count) and record 0
fully keyed; **records 1..N omit field keys** (schema-positional / delta —
no field token recurs across records), so a byte-exact decode of every record
needs the positional-schema layer that `count` + record 0 establish. This
parser extracts the self-describing parts (the `0x1d` `CTraceMsg` blobs and the
keyed fields), which is where the per-file trace output comes from.

## Update 2: exact payload framing (2026-07-07)

The board-state protobuf payload is:

```
[2-byte prefix 01 02] [flat codec map]
```

The codec map (`FUN_00d165e0`, looped until key 0) decodes cleanly:

```
#0170e6db (t0x16)=7792   #1a3523bf (t2)=131072   #794f4cb9 (t2)=4
name (t1)="Trace Events" #bfc7bfda (t0x11)=1     objectId (t0x1f)=1894177538
count (t0xa)=40          type (t2)=106           traceMsg (t0x1d)=08031801
```

i.e. the entity header + **record 0 fully keyed** (type + traceMsg protobuf).

After record 0, the byte where a value-type should be is `0x3e` (> 0x27, an
invalid type) — records 1..39 are **positional field-path deltas** against
record 0's schema (no field keys recur; the `05 16 24 00` / `50 00 00 4a 1a…`
bytes are field-path ops, `05` = copy-from-baseline). Decoding every field of
records 1..39 byte-exact needs that field-path op table (schema-driven, in the
apply path). The keyed codec + the proto-signature/creep-copy scans recover the
`CTraceMsg` events and card templates; the per-attribute deltas (health, mana,
combat position over time) are the remaining layer.
