#!/usr/bin/env python3
"""Generates testdata/oracle_golden.json from testdata/oracle_types.json by
building the corresponding canonical-ABI type objects using the vendored
reference implementation (testdata/definitions.py) and computing size,
alignment, and flatten (and flatten_functype for func entries) for each.

This is the reference (Python) half of the differential oracle: oracle_test.go
builds the SAME battery of types as Go binary.TypeDesc values and asserts that
Size/Alignment/Flatten/FlattenFunc agree exactly with this golden file.

Usage:
    python3 testdata/gen_oracle.py

Run from the internal/component/abi directory (or anywhere; paths below are
resolved relative to this script's location).
"""
import importlib.util
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def load_definitions():
    """Import the vendored definitions.py as a module named 'refabi'.

    definitions.py uses `from __future__ import annotations` plus @dataclass,
    whose introspection needs to find the module in sys.modules by name, so we
    register it there before exec_module runs.
    """
    path = os.path.join(HERE, "definitions.py")
    spec = importlib.util.spec_from_file_location("refabi", path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["refabi"] = mod
    spec.loader.exec_module(mod)
    return mod


ref = load_definitions()

# Canonical ABI options: 32-bit pointers (the Go implementation in
# internal/component/abi is 32-bit only), no async.
PTR_TYPE = "i32"


class Opts:
    """Minimal stand-in for CanonicalOptions, just enough for flatten_functype
    and alignment/elem_size (which only need memory.ptr_type() and async_)."""

    class _Mem:
        def ptr_type(self_inner):
            return PTR_TYPE

    def __init__(self):
        self.memory = Opts._Mem()
        self.async_ = False
        self.callback = None


OPTS = Opts()

# A dummy ResourceType for own/borrow: alignment/elem_size/flatten never
# dereference rt.impl, so None is a safe stand-in.
DUMMY_RESOURCE = ref.ResourceType(impl=None)


def build_type(spec, by_name):
    """Recursively builds a ValType from a TypeSpec JSON object. `by_name` maps
    already-built battery entry names to their ValType, for 'ref' resolution."""
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

    if kind == "record":
        fields = [ref.FieldType(f["name"], build_type(f["type"], by_name)) for f in spec["fields"]]
        return ref.RecordType(fields)

    if kind == "variant":
        cases = [
            ref.CaseType(c["name"], build_type(c["type"], by_name) if c["type"] is not None else None)
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
        return ref.OwnType(DUMMY_RESOURCE)

    if kind == "borrow":
        return ref.BorrowType(DUMMY_RESOURCE)

    if kind == "ref":
        return by_name[spec["name"]]

    raise ValueError(f"unknown type kind: {kind}")


class FuncTypeSpec:
    """Minimal FuncType-alike exposing param_types()/result_type() as required
    by flatten_functype, built from our JSON func spec."""

    def __init__(self, spec, by_name):
        self.params = [build_type(p["type"], by_name) for p in spec["params"]]
        results = spec["results"]
        if "unnamed" in results:
            self.results = [build_type(results["unnamed"], by_name)]
        else:
            self.results = [build_type(r["type"], by_name) for r in results["named"]]

    def param_types(self):
        return self.params

    def result_type(self):
        return self.results


def core_type_names(flat):
    """flatten_type/flatten_functype return raw core type tags ('i32','i64',
    'f32','f64') already as strings for value types; ptr_type() also yields
    'i32' strings, so no translation is needed."""
    return list(flat)


def main():
    types_path = os.path.join(HERE, "oracle_types.json")
    golden_path = os.path.join(HERE, "oracle_golden.json")

    with open(types_path) as f:
        battery = json.load(f)["types"]

    by_name = {}
    golden = {}

    for entry in battery:
        name = entry["name"]
        spec = entry["type"]

        if spec["kind"] == "func":
            ft = FuncTypeSpec(spec, by_name)
            lift = ref.flatten_functype(OPTS, ft, "lift")
            lower = ref.flatten_functype(OPTS, ft, "lower")
            golden[name] = {
                "kind": "func",
                "lift": {
                    "params": core_type_names(lift.params),
                    "results": core_type_names(lift.results),
                },
                "lower": {
                    "params": core_type_names(lower.params),
                    "results": core_type_names(lower.results),
                },
            }
            # Func types aren't referenceable by later entries; skip by_name.
            continue

        t = build_type(spec, by_name)
        by_name[name] = t

        size = ref.elem_size(t, PTR_TYPE)
        align = ref.alignment(t, PTR_TYPE)
        flat = ref.flatten_type(t, OPTS)

        golden[name] = {
            "kind": "value",
            "size": size,
            "alignment": align,
            "flatten": core_type_names(flat),
        }

    with open(golden_path, "w") as f:
        json.dump(golden, f, indent=2, sort_keys=True)
        f.write("\n")

    print(f"wrote {len(golden)} golden entries to {golden_path}")


if __name__ == "__main__":
    main()
