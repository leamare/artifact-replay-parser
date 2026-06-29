#!/usr/bin/env python3
"""Generate traces_data.go from protos/artifact2/dcg_gamemessages.proto.

Emits traceSignatures (field set + length-delimited fields, for classification)
and traceFieldNames (field number -> proto name, for decoding) for every
CTraceMsg_* message. Run from the repo root:  python3 internal/dcg/gen_traces.py
"""
import re
import sys

PROTO = "protos/artifact2/dcg_gamemessages.proto"
OUT = "internal/dcg/traces_data.go"

src = open(PROTO).read()

# Collect message and enum names so a ".Type" field can be told apart
# (messages serialize as length-delimited; enums as varint).
msg_names = set(re.findall(r'\bmessage\s+(\w+)', src))
enum_names = set(re.findall(r'\benum\s+(\w+)', src))

SCALAR_BYTES = {"bytes", "string"}

def is_bytes_type(typ: str) -> bool:
    if typ in SCALAR_BYTES:
        return True
    if typ.startswith("."):
        leaf = typ.lstrip(".").split(".")[-1]
        return leaf in msg_names and leaf not in enum_names
    return False

field_re = re.compile(r'^\s*(optional|repeated|required)\s+([.\w]+)\s+(\w+)\s*=\s*(\d+)\s*;')

traces = []  # (name, [(num, fname, is_bytes)])
for m in re.finditer(r'message\s+(CTraceMsg_\w+)\s*\{', src):
    name = m.group(1)[len("CTraceMsg_"):]
    # body from after '{' to the matching '}' (messages here don't nest braces
    # except enums, which we skip by tracking depth)
    i = m.end()
    depth = 1
    body_start = i
    while i < len(src) and depth:
        if src[i] == '{':
            depth += 1
        elif src[i] == '}':
            depth -= 1
        i += 1
    body = src[body_start:i-1]
    fields = []
    for line in body.splitlines():
        fm = field_re.match(line)
        if not fm:
            continue
        label, typ, fname, num = fm.group(1), fm.group(2), fm.group(3), int(fm.group(4))
        # repeated scalars are proto2-unpacked (varint) -> not length-delimited;
        # repeated messages and bytes/string/message are length-delimited.
        bytes_field = is_bytes_type(typ)
        fields.append((num, fname, bytes_field))
    traces.append((name, fields))

# Stable order: by min field number then name, matching the proto's flow.
def keyf(t):
    nums = [f[0] for f in t[1]]
    return (min(nums) if nums else 0, t[0])

with open(OUT, "w") as o:
    o.write("package dcg\n\n")
    o.write("// Code generated from protos/artifact2/dcg_gamemessages.proto by\n")
    o.write("// gen_traces.py. DO NOT EDIT. Regenerate with: python3 internal/dcg/gen_traces.py\n")
    o.write("// traceSignatures lists each trace's field signature for classification;\n")
    o.write("// traceFieldNames maps field numbers to proto names for decoding.\n\n")
    o.write("var traceSignatures = []traceSig{\n")
    for name, fields in traces:
        nums = sorted(f[0] for f in fields)
        bnums = sorted(f[0] for f in fields if f[2])
        nstr = "{" + ", ".join(map(str, nums)) + "}"
        bstr = "{" + ", ".join(map(str, bnums)) + "}"
        o.write(f'\t{{name: "{name}", fields: []int{nstr}, bytesFields: []int{bstr}}},\n')
    o.write("}\n\n")
    o.write("var traceFieldNames = map[string]map[int]string{\n")
    for name, fields in traces:
        if not fields:
            continue
        inner = ", ".join(f'{num}: "{fname}"' for num, fname, _ in sorted(fields))
        o.write(f'\t"{name}": {{{inner}}},\n')
    o.write("}\n")

print(f"wrote {OUT}: {len(traces)} trace messages", file=sys.stderr)
