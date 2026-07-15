#!/usr/bin/env python3
"""
Generate oracle_flat_golden.json for flat ABI tests.

This script reads oracle_flat.json, uses definitions.py to lower/lift values
to/from the flat ABI, and emits oracle_flat_golden.json containing:
  - the lowered flat representation as {kind, bits} for each test value
    (core_vals), and
  - the linear-memory bytes actually written by that lowering (mem_hex),
    starting at mem_base.

Every value is lowered against a *fresh* 64KiB memory and a fresh
deterministic bump allocator that starts handing out pointers at
mem_base=1024. The Go half (flat_oracle_test.go) uses the exact same
bump-allocator scheme, so the emitted pointers are byte-for-byte
reproducible across languages: this is what lets the oracle catch a
lowerFlatString/lowerFlatList stub that returns (0,0) instead of doing the
real realloc+copy -- the golden ptr is provably non-zero and the golden
mem_hex is the proof the bytes actually landed in memory.
"""

import importlib.util
import json
import sys
import os
import struct

HERE = os.path.dirname(os.path.abspath(__file__))

def load_definitions():
    """Import the vendored definitions.py as a module."""
    path = os.path.join(HERE, "definitions.py")
    spec = importlib.util.spec_from_file_location("refabi", path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["refabi"] = mod
    spec.loader.exec_module(mod)
    return mod

ref = load_definitions()

# Canonical ABI options: 32-bit pointers, 64KiB linear memory.
PTR_TYPE = "i32"
MEM_SIZE = 65536

# The bump allocator's base address. Both this script and
# flat_oracle_test.go's Go bump allocator start handing out pointers here, so
# a fresh allocator run over the same sequence of realloc(0, 0, align, size)
# calls produces byte-identical pointers in both languages.
BASE_PTR = 1024

class BumpAllocator:
    """Deterministic realloc(): every call is treated as a fresh allocation
    (orig_ptr/orig_size are ignored -- lower_flat never grows an existing
    allocation for the plain-ASCII/no-growth test data this oracle uses), so
    the address handed out only depends on the *order* of calls. That order
    is fixed by the value being lowered (e.g. list<string> allocates the
    (ptr,len) table first, then each element's string bytes in order), so it
    is identical between the Python reference and the Go implementation
    under test.
    """

    def __init__(self, base=BASE_PTR):
        self.next_free = base

    def __call__(self, orig_ptr, orig_size, align, new_size):
        ptr = ref.align_to(self.next_free, align)
        self.next_free = ptr + new_size
        return ptr

def make_context():
    """Builds a fresh (cx, mem, allocator) triple for lowering one value."""
    mem = ref.MemInst(bytearray(MEM_SIZE), PTR_TYPE)
    opts = ref.CanonicalOptions()
    opts.memory = mem
    opts.string_encoding = "utf8"
    allocator = BumpAllocator()
    opts.realloc = allocator
    cx = ref.LiftLowerContext(opts, None, None)
    return cx, mem, allocator

def build_type(spec, by_name):
    """Recursively builds a ValType from a TypeSpec JSON object."""
    kind = spec["kind"]

    if kind == "primitive":
        prim_map = {
            "bool": ref.BoolType,
            "s8": ref.S8Type,
            "u8": ref.U8Type,
            "s16": ref.S16Type,
            "u16": ref.U16Type,
            "s32": ref.S32Type,
            "u32": ref.U32Type,
            "s64": ref.S64Type,
            "u64": ref.U64Type,
            "f32": ref.F32Type,
            "f64": ref.F64Type,
            "char": ref.CharType,
            "string": ref.StringType,
        }
        return prim_map[spec["prim"]]()

    if kind == "list":
        return ref.ListType(build_type(spec["elem"], by_name))

    if kind == "ref":
        return by_name[spec["name"]]

    if kind == "record":
        fields = [ref.FieldType(f["name"], build_type(f["type"], by_name)) for f in spec["fields"]]
        return ref.RecordType(fields)

    if kind == "variant":
        cases = [
            ref.CaseType(c["name"], build_type(c["type"], by_name) if c.get("type") is not None else None)
            for c in spec["cases"]
        ]
        return ref.VariantType(cases)

    if kind == "option":
        return ref.OptionType(build_type(spec["elem"], by_name))

    if kind == "result":
        ok = build_type(spec["ok"], by_name) if spec.get("ok") is not None else None
        err = build_type(spec["err"], by_name) if spec.get("err") is not None else None
        return ref.ResultType(ok, err)

    if kind == "tuple":
        elems = [build_type(e, by_name) for e in spec["elems"]]
        return ref.TupleType(elems)

    if kind == "flags":
        return ref.FlagsType(spec["names"])

    if kind == "enum":
        return ref.EnumType(spec["names"])

    raise ValueError(f"Unknown kind: {kind}")

def to_string_triple(s):
    """Builds the (str, encoding, tagged_code_units) triple definitions.py's
    store_string_into_range() expects for a String value. For plain UTF-8
    test data the tagged code unit count is just the UTF-8 byte length."""
    return ref.String((s, "utf8", len(s.encode("utf-8"))))

def convert_value(raw_value, t):
    """Convert a JSON value into the shape definitions.py's own lower_flat()/
    store() expect for type t. This mirrors how the reference implementation
    represents each composite kind internally (see match_case, store_record,
    store_variant, pack_flags_into_int, despecialize):
      - record:  {label: value, ...}
      - tuple:   despecializes to a record with str(index) labels ->
                 {"0": v0, "1": v1, ...}
      - variant: {case_label: payload_or_None}
      - option:  despecializes to a 2-case variant -> {"none": None} or
                 {"some": value}
      - result:  despecializes to a 2-case variant (labels "ok"/"error",
                 note: NOT "err") -> {"ok": value_or_None} or
                 {"error": value_or_None}
      - flags:   despecializes to per-label booleans -> {label: bool, ...}
      - enum:    despecializes to a no-payload variant -> {label: None}
      - list:    a plain Python list of converted elements
      - string:  the (str, encoding, tagged_code_units) triple above
    """
    match t:
        case ref.BoolType():
            return bool(raw_value)
        case ref.U8Type() | ref.U16Type() | ref.U32Type() | ref.U64Type():
            return int(raw_value)
        case ref.S8Type() | ref.S16Type() | ref.S32Type() | ref.S64Type():
            return int(raw_value)
        case ref.F32Type() | ref.F64Type():
            return float(raw_value)
        case ref.CharType():
            return chr(int(raw_value))
        case ref.StringType():
            return to_string_triple(str(raw_value))
        case ref.ListType(t=elem_t):
            return [convert_value(v, elem_t) for v in raw_value]
        case ref.RecordType(fields=fields):
            return {f.label: convert_value(v, f.t) for v, f in zip(raw_value, fields)}
        case ref.TupleType(ts=ts):
            return {str(i): convert_value(v, et) for i, (v, et) in enumerate(zip(raw_value, ts))}
        case ref.VariantType(cases=cases):
            [(label, val)] = raw_value.items()
            case_type = next((c.t for c in cases if c.label == label), None)
            return {label: convert_value(val, case_type) if case_type is not None else None}
        case ref.OptionType(t=elem_t):
            if raw_value is None:
                return {"none": None}
            return {"some": convert_value(raw_value, elem_t)}
        case ref.ResultType(ok=ok_t, error=err_t):
            if "ok" in raw_value:
                v = raw_value["ok"]
                return {"ok": convert_value(v, ok_t) if ok_t is not None else None}
            v = raw_value["err"]
            return {"error": convert_value(v, err_t) if err_t is not None else None}
        case ref.FlagsType(labels=labels):
            bits = int(raw_value)
            return {name: bool(bits & (1 << i)) for i, name in enumerate(labels)}
        case ref.EnumType(labels=labels):
            idx = int(raw_value)
            return {labels[idx]: None}
        case _:
            return raw_value

def load_oracle_flat():
    """Load oracle_flat.json."""
    with open(os.path.join(HERE, 'oracle_flat.json')) as f:
        return json.load(f)

def core_vals_to_dicts(flat_vals, flat_types):
    """Convert core values (int/float) to {kind, bits} dicts, using flat_types to determine sizes."""
    result = []
    for fv, ft in zip(flat_vals, flat_types):
        if ft == 'f32':
            bits = struct.unpack('<I', struct.pack('<f', float(fv)))[0]
            result.append({"kind": "f32", "bits": bits})
        elif ft == 'f64':
            bits = struct.unpack('<Q', struct.pack('<d', float(fv)))[0]
            result.append({"kind": "f64", "bits": bits})
        elif ft == 'i32':
            result.append({"kind": "i32", "bits": int(fv) & 0xFFFFFFFF})
        elif ft == 'i64':
            result.append({"kind": "i64", "bits": int(fv) & 0xFFFFFFFFFFFFFFFF})
    return result

def main():
    oracle = load_oracle_flat()

    # Build type table from types
    type_table = {}
    for type_entry in oracle["types"]:
        name = type_entry["name"]
        spec = type_entry["type"]
        if isinstance(spec, str):
            spec = json.loads(spec)
        type_table[name] = build_type(spec, type_table)

    # Process each value
    golden = []

    for value_entry in oracle["values"]:
        type_name = value_entry["type"]
        value_name = value_entry["name"]
        raw_value = value_entry["value"]

        t = type_table[type_name]
        v = convert_value(raw_value, t)

        # Fresh memory + fresh deterministic bump allocator per value, so
        # pointers are reproducible and independent of iteration order.
        cx, mem, allocator = make_context()

        try:
            # Lower the value to flat
            flat_vals = ref.lower_flat(cx, v, t)

            # Get the flat types for proper kind inference
            flat_types = ref.flatten_type(t, cx.opts)

            # Convert core values to dicts
            core_vals = core_vals_to_dicts(flat_vals, flat_types)

            # Capture the bytes written into linear memory by this lowering
            # (if any). watermark is where the bump allocator would hand out
            # the *next* pointer, i.e. one past the last byte written.
            watermark = allocator.next_free
            mem_hex = bytes(mem.bytes[BASE_PTR:watermark]).hex()

            golden.append({
                "name": value_name,
                "type": type_name,
                "core_vals": core_vals,
                "mem_base": BASE_PTR,
                "mem_hex": mem_hex,
            })
        except Exception as e:
            print(f"Error lowering {value_name} ({type_name}): {e}", file=sys.stderr)
            import traceback
            traceback.print_exc()
            pass

    # Write golden file
    output_path = os.path.join(HERE, 'oracle_flat_golden.json')
    with open(output_path, 'w') as f:
        json.dump(golden, f, indent=2)
    print(f"Generated {output_path} with {len(golden)} entries")

if __name__ == "__main__":
    main()
