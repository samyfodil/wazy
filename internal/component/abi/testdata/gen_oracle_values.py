#!/usr/bin/env python3
"""Generates testdata/oracle_values_golden.json from testdata/oracle_values.json by
storing component model values into memory using the vendored canonical-ABI reference
implementation and capturing the resulting bytes and load round-trips.

This is the reference (Python) half of the value differential oracle: values_oracle_test.go
builds the SAME battery of values as Go Value objects and asserts that Store produces
byte-identical results and Load round-trips the value correctly.

Usage:
    python3 testdata/gen_oracle_values.py

Run from the internal/component/abi directory (or anywhere; paths below are
resolved relative to this script's location).
"""
import importlib.util
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def load_definitions():
    """Import the vendored definitions.py as a module named 'refabi'."""
    path = os.path.join(HERE, "definitions.py")
    spec = importlib.util.spec_from_file_location("refabi", path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["refabi"] = mod
    spec.loader.exec_module(mod)
    return mod


ref = load_definitions()

# Canonical ABI options: 32-bit pointers, no async
PTR_TYPE = "i32"


class Opts:
    """Stand-in for CanonicalOptions, configured for value store/load."""

    class _Mem:
        def __init__(self, mem_bytes):
            self.bytes = mem_bytes
            self.addrtype = 'i32'

        def ptr_type(self):
            return self.addrtype

        def ptr_size(self):
            return 4

        def __getitem__(self, i):
            return self.bytes[i]

        def __setitem__(self, i, v):
            self.bytes[i] = v

        def __len__(self):
            return len(self.bytes)

    def __init__(self, mem_bytes):
        self.memory = Opts._Mem(mem_bytes)
        self.string_encoding = 'utf8'
        self.async_ = False
        self.callback = None
        self.realloc = None  # Will be set by caller


class BumpAllocator:
    """A deterministic bump allocator for testing."""

    def __init__(self, start=1024, mem=None):
        self.current = start
        self.start = start
        self.mem = mem

    def alloc(self, orig_ptr, orig_size, align, new_size):
        """Allocate new_size bytes aligned to align."""
        # If this is a growth (orig_ptr == 0), just bump
        if orig_ptr == 0:
            # Align current position
            if align > 0:
                self.current = (self.current + align - 1) // align * align
            ptr = self.current
            self.current += new_size
            return ptr
        else:
            # Realloc: allocate new space and return new pointer
            # (We don't copy data in this simple test allocator)
            if align > 0:
                self.current = (self.current + align - 1) // align * align
            ptr = self.current
            self.current += new_size
            return ptr


class Ctx:
    """Minimal context for store/load."""

    def __init__(self, opts, alloc):
        self.opts = opts
        self.alloc = alloc
        self.inst = type('obj', (object,), {'handles': ref.Table()})()
        self.borrow_scope = None


def build_type(spec, by_name):
    """Recursively build a ValType from JSON spec."""
    kind = spec.get("kind")

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

    if kind == "record":
        fields = [ref.FieldType(f["name"], build_type(f["type"], by_name)) for f in spec["fields"]]
        return ref.RecordType(fields)

    if kind == "variant":
        cases = [
            ref.CaseType(c["name"], build_type(c["type"], by_name) if c.get("type") is not None else None)
            for c in spec["cases"]
        ]
        return ref.VariantType(cases)

    if kind == "list":
        return ref.ListType(build_type(spec["elem"], by_name), None)

    if kind == "tuple":
        return ref.TupleType([build_type(e, by_name) for e in spec["elems"]])

    if kind == "flags":
        return ref.FlagsType(list(spec["names"]))

    if kind == "enum":
        return ref.EnumType(list(spec["cases"]))

    if kind == "option":
        return ref.OptionType(build_type(spec["elem"], by_name))

    if kind == "result":
        ok = build_type(spec["ok"], by_name) if spec.get("ok") is not None else None
        err = build_type(spec["err"], by_name) if spec.get("err") is not None else None
        return ref.ResultType(ok, err)

    if kind == "own":
        return ref.OwnType(ref.ResourceType(impl=None))

    if kind == "borrow":
        return ref.BorrowType(ref.ResourceType(impl=None))

    if kind == "ref":
        return by_name[spec["name"]]

    raise ValueError(f"unknown type kind: {kind}")


def convert_value_to_refabi(v, t):
    """Convert a JSON value to reference ABI format.

    JSON representation:
    - bool: true/false
    - int/float: numbers
    - string: string
    - list: [...]
    - record: [field_values] (array in field order)
    - tuple: [element_values]
    - variant: {"disc": N, "payload": value or null}
    - flags: bitset as number
    - enum: case index as number (converted to variant internally by despecialize)
    - option: null (none) or value (some) (converted to variant internally by despecialize)
    - result: {"isErr": bool, "payload": value or null} (converted to variant internally by despecialize)
    """
    if isinstance(t, ref.BoolType):
        return bool(v)
    elif isinstance(t, (ref.U8Type, ref.U16Type, ref.U32Type, ref.S8Type, ref.S16Type, ref.S32Type, ref.S64Type, ref.U64Type)):
        return int(v)
    elif isinstance(t, (ref.F32Type, ref.F64Type)):
        return float(v)
    elif isinstance(t, ref.CharType):
        return chr(v)
    elif isinstance(t, ref.StringType):
        # String in ref ABI is (str, encoding, code_units)
        # For UTF-8, code_units is the byte length
        s = str(v)
        byte_len = len(s.encode('utf-8'))
        return (s, 'utf8', byte_len)
    elif isinstance(t, ref.ListType):
        return [convert_value_to_refabi(e, t.t) for e in v]
    elif isinstance(t, ref.RecordType):
        # v is array of field values in order
        rec = {}
        for i, field in enumerate(t.fields):
            rec[field.label] = convert_value_to_refabi(v[i], field.t)
        return rec
    elif isinstance(t, ref.TupleType):
        # Tuples are deserialized to RecordType with string indices as field labels
        # v is array of element values in order
        rec = {}
        for i, elem in enumerate(v):
            rec[str(i)] = convert_value_to_refabi(elem, t.ts[i])
        return rec
    elif isinstance(t, ref.VariantType):
        # v is {"disc": N, "payload": ...}
        disc = v["disc"]
        payload_v = v.get("payload")
        case_t = t.cases[disc].t
        payload = None
        if case_t is not None:
            payload = convert_value_to_refabi(payload_v, case_t)
        return {t.cases[disc].label: payload}
    elif isinstance(t, ref.FlagsType):
        return {label: bool((v >> i) & 1) for i, label in enumerate(t.labels)}
    elif isinstance(t, ref.EnumType):
        # Enum is represented as a variant: just the label with None payload
        label = t.labels[v]
        return {label: None}
    elif isinstance(t, ref.OptionType):
        if v is None:
            # Option none is variant {"none": None}
            return {"none": None}
        else:
            # Option some is variant {"some": value}
            converted = convert_value_to_refabi(v, t.t)
            return {"some": converted}
    elif isinstance(t, ref.ResultType):
        # v is {"isErr": bool, "payload": ...}
        is_err = v["isErr"]
        payload_v = v.get("payload")
        if is_err:
            label = "error"
            case_t = t.error
        else:
            label = "ok"
            case_t = t.ok
        payload = None
        if case_t is not None:
            payload = convert_value_to_refabi(payload_v, case_t)
        return {label: payload}
    else:
        raise ValueError(f"unsupported type: {type(t)}")


def convert_value_from_refabi(v, t):
    """Convert a reference ABI value back to JSON representation."""
    if isinstance(t, ref.BoolType):
        return bool(v)
    elif isinstance(t, (ref.U8Type, ref.U16Type, ref.U32Type, ref.S8Type, ref.S16Type, ref.S32Type, ref.S64Type, ref.U64Type)):
        return int(v)
    elif isinstance(t, (ref.F32Type, ref.F64Type)):
        return float(v)
    elif isinstance(t, ref.CharType):
        return ord(v)
    elif isinstance(t, ref.StringType):
        # String in ref ABI is (str, encoding, code_units)
        s, _, _ = v
        return s
    elif isinstance(t, ref.ListType):
        return [convert_value_from_refabi(e, t.t) for e in v]
    elif isinstance(t, ref.RecordType):
        # Return array of field values in order
        result = []
        for field in t.fields:
            result.append(convert_value_from_refabi(v[field.label], field.t))
        return result
    elif isinstance(t, ref.TupleType):
        # Tuples are loaded as RecordType with string indices as field labels
        # Return array of element values in order
        result = []
        for i in range(len(t.ts)):
            result.append(convert_value_from_refabi(v[str(i)], t.ts[i]))
        return result
    elif isinstance(t, ref.VariantType):
        # Return {"disc": N, "payload": ...}
        [label] = v.keys()
        [disc] = [i for i, c in enumerate(t.cases) if c.label == label]
        [payload] = v.values()
        case_t = t.cases[disc].t
        result_payload = None
        if case_t is not None:
            result_payload = convert_value_from_refabi(payload, case_t)
        return {"disc": disc, "payload": result_payload}
    elif isinstance(t, ref.FlagsType):
        bits = 0
        for i, label in enumerate(t.labels):
            if v[label]:
                bits |= (1 << i)
        return bits
    elif isinstance(t, ref.EnumType):
        # Enum value is variant {"label": None}
        [label] = v.keys()
        return t.labels.index(label)
    elif isinstance(t, ref.OptionType):
        # Option value is variant {"none": None} or {"some": value}
        [label] = v.keys()
        if label == "none":
            return None
        else:  # "some"
            [payload] = v.values()
            return convert_value_from_refabi(payload, t.t)
    elif isinstance(t, ref.ResultType):
        # Return {"isErr": bool, "payload": ...}
        if "ok" in v:
            label = "ok"
            payload_v = v["ok"]
            is_err = False
            case_t = t.ok
        else:
            label = "error"
            payload_v = v["error"]
            is_err = True
            case_t = t.error
        result_payload = None
        if case_t is not None:
            result_payload = convert_value_from_refabi(payload_v, case_t)
        return {"isErr": is_err, "payload": result_payload}
    else:
        raise ValueError(f"unsupported type: {type(t)}")


def main():
    values_path = os.path.join(HERE, "oracle_values.json")
    golden_path = os.path.join(HERE, "oracle_values_golden.json")

    with open(values_path) as f:
        battery = json.load(f)["values"]

    # Allocate a large memory buffer for testing
    mem_bytes = bytearray(65536)
    golden = {}

    for entry in battery:
        name = entry["name"]
        type_spec = entry["type"]
        value_json = entry["value"]

        # Build the type
        t = build_type(type_spec, {})

        # Convert JSON value to reference ABI format
        refabi_value = convert_value_to_refabi(value_json, t)

        # Create allocator and context
        alloc = BumpAllocator(start=1024, mem=mem_bytes)
        opts = Opts(mem_bytes)
        opts.realloc = alloc.alloc
        ctx = Ctx(opts, alloc)

        # Store the value
        try:
            ref.store(ctx, refabi_value, t, 1024)
        except Exception as e:
            print(f"ERROR in {name}: store failed: {e}")
            continue

        # Capture the memory used
        mem_start = 1024
        mem_end = alloc.current
        mem_hex = mem_bytes[mem_start:mem_end].hex()

        # Load it back
        try:
            loaded_refabi = ref.load(ctx, 1024, t)
        except Exception as e:
            print(f"ERROR in {name}: load failed: {e}")
            continue

        # Convert back to JSON
        loaded_json = convert_value_from_refabi(loaded_refabi, t)

        golden[name] = {
            "ptr": 1024,
            "mem_hex": mem_hex,
            "mem_len": mem_end - mem_start,
            "loaded_value": loaded_json,
        }

    with open(golden_path, "w") as f:
        json.dump(golden, f, indent=2, sort_keys=True)
        f.write("\n")

    print(f"wrote {len(golden)} golden value entries to {golden_path}")


if __name__ == "__main__":
    main()
