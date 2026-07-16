#!/usr/bin/env python3
"""
Adds a battery of COMPLEX / nested / edge-case types + values to
oracle_flat.json, then regenerates oracle_flat_golden.json from the reference
definitions.py.

These stress the parts of the Canonical ABI where subtle marshalling bugs
hide: record field alignment/padding, variant discriminant width (u8 vs u16),
the variant flat-join type coercion (f32/i32, f64/i64), deep nesting, spilling
composites, and list-of-composite strides. The Go oracle (flat_oracle_test.go)
and the compiled-lower equivalence test (plan_test.go) both consume this
battery, so any Go-side divergence from the spec surfaces as a golden mismatch.

Run:  python3 internal/component/abi/testdata/gen_complex_flat.py
(idempotent: merges by name, then re-invokes gen_oracle_flat.py)
"""
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# --- type helpers ---------------------------------------------------------
def prim(p): return {"kind": "primitive", "prim": p}
def ref(n): return {"kind": "ref", "name": n}
def lst(e): return {"kind": "list", "elem": e}
def rec(*fields): return {"kind": "record", "fields": [{"name": n, "type": t} for n, t in fields]}
def var(*cases): return {"kind": "variant", "cases": [({"name": c[0]} if len(c) == 1 else {"name": c[0], "type": c[1]}) for c in cases]}
def opt(e): return {"kind": "option", "elem": e}
def res(ok=None, err=None):
    d = {"kind": "result"}
    if ok is not None: d["ok"] = ok
    if err is not None: d["err"] = err
    return d
def tup(*es): return {"kind": "tuple", "elems": list(es)}
def flags(*names): return {"kind": "flags", "names": list(names)}
def enum(*names): return {"kind": "enum", "names": list(names)}

U32, S32, U64, S64, F32, F64, U8, U16, STR, BOOL, CHAR = (
    prim("u32"), prim("s32"), prim("u64"), prim("s64"), prim("f32"),
    prim("f64"), prim("u8"), prim("u16"), prim("string"), prim("bool"), prim("char"))

# 300-case enum/variant to force a u16 in-memory discriminant (>256 cases).
BIG_LABELS = [f"c{i}" for i in range(300)]

COMPLEX_TYPES = [
    # padding torture: u8,u64,u8,u16,u8 -> alignment 8, lots of pad bytes
    ("mixed_align_record", rec(("a", U8), ("b", U64), ("c", U8), ("d", U16), ("e", U8))),
    # variant flat-join coercion: f32 vs u32 share one i32 flat slot
    ("variant_f32_u32", var(("f", F32), ("u", U32))),
    # variant flat-join coercion: f64 vs u64 share one i64 flat slot
    ("variant_f64_u64", var(("f", F64), ("u", U64))),
    # variant whose cases have different flat arities (join widens)
    ("variant_ragged", var(("empty",), ("one", U32), ("pair", tup(U64, F64)))),
    # nested record of records + an enum + a list
    ("nested_record", rec(("pt", rec(("x", S32), ("y", S32))), ("tag", enum("red", "green", "blue")), ("data", lst(U32)))),
    # variant with composite payloads (record / list<string> / tuple)
    ("variant_composite", var(("empty",), ("pt", rec(("x", U32), ("y", U32))), ("names", lst(STR)), ("pair", tup(U64, F64)))),
    # list of records containing a nested list (stride + inner realloc)
    ("list_of_records", lst(rec(("name", STR), ("nums", lst(U32))))),
    # list of lists
    ("list_of_lists", lst(lst(U32))),
    # result<record, variant>
    ("result_rec_var", res(ok=rec(("code", U32), ("msg", STR)), err=var(("io",), ("bad", STR)))),
    # result with only an err arm
    ("result_only_err", res(err=STR)),
    # option<list<string>>
    ("option_list_string", opt(lst(STR))),
    # record of variants
    ("record_of_variants", rec(("a", var(("n",), ("v", U32))), ("b", var(("s", STR), ("t", U64))))),
    # deep nesting: record { a: record { b: record { c: list<result<u32,string>> } } }
    ("deeply_nested", rec(("a", rec(("b", rec(("c", lst(res(ok=U32, err=STR)))),))),)),
    # (a >16-flat spilling tuple is intentionally NOT here: wazy's LowerFlat
    # bundles the function-param spill into value-lowering, which the spec's
    # standalone lower_flat does not, so they legitimately diverge -- the spill
    # path is covered directly by plan_test.go's TestLowerStepSpill instead.)
    # >256-case enum -> u16 discriminant in memory
    ("big_enum", enum(*BIG_LABELS)),
    # >256-case variant (no payloads) -> u16 discriminant in memory
    ("big_variant", var(*[(l,) for l in BIG_LABELS])),
    # option<result<...>> combo
    ("option_result", opt(res(ok=tup(U32, STR), err=U64))),
    # record with a bool + char + string (mixed small + realloc)
    ("record_bool_char_str", rec(("flag", BOOL), ("ch", CHAR), ("label", STR))),
    # flags near the 32-bit boundary (32 flags = still one i32)
    ("flags32", flags(*[f"f{i}" for i in range(32)])),
]

COMPLEX_VALUES = [
    ("mixed_align_record", "mixed_align_vals", [255, 1099511627775, 1, 4096, 7]),
    ("variant_f32_u32", "vfu_f", {"f": 3.5}),
    ("variant_f32_u32", "vfu_u", {"u": 123456}),
    ("variant_f64_u64", "vfu64_f", {"f": 2.718281828}),
    ("variant_f64_u64", "vfu64_u", {"u": 9876543210}),
    ("variant_ragged", "vr_empty", {"empty": None}),
    ("variant_ragged", "vr_one", {"one": 77}),
    ("variant_ragged", "vr_pair", {"pair": [100, 6.25]}),
    ("nested_record", "nr_vals", [[-3, 9], 1, [10, 20, 30]]),
    ("variant_composite", "vc_pt", {"pt": [4, 5]}),
    ("variant_composite", "vc_names", {"names": ["alpha", "beta", "gamma"]}),
    ("variant_composite", "vc_pair", {"pair": [42, 1.5]}),
    ("variant_composite", "vc_empty", {"empty": None}),
    ("list_of_records", "lor_vals", [["a", [1, 2]], ["bb", []], ["ccc", [9]]]),
    ("list_of_lists", "lol_vals", [[1, 2], [], [3, 4, 5]]),
    ("result_rec_var", "rrv_ok", {"ok": [200, "OK"]}),
    ("result_rec_var", "rrv_err", {"err": {"bad": "nope"}}),
    ("result_rec_var", "rrv_err_io", {"err": {"io": None}}),
    ("result_only_err", "roe_err", {"err": "boom"}),
    ("option_list_string", "ols_some", ["x", "yy", "zzz"]),
    ("option_list_string", "ols_none", None),
    ("record_of_variants", "rov_vals", [{"v": 8}, {"s": "hi"}]),
    ("deeply_nested", "dn_vals", [[[[{"ok": 1}, {"err": "e"}, {"ok": 999}]]]]),
    ("big_enum", "be_first", 0),
    ("big_enum", "be_299", 299),
    ("big_variant", "bv_first", {"c0": None}),
    ("big_variant", "bv_299", {"c299": None}),
    ("option_result", "or_some_ok", {"ok": [7, "seven"]}),
    ("option_result", "or_some_err", {"err": 42}),
    ("option_result", "or_none", None),
    ("record_bool_char_str", "rbcs_vals", [True, 0x1F600, "emoji"]),
    ("flags32", "flags32_hi", 0x80000001),
]


# --- hoisting -------------------------------------------------------------
# The Go oracle builder models nested composites the way a real component
# binary does: a record field / variant case / list element / etc. is a
# TypeRef, which holds a primitive OR a type-space index -- never an inline
# composite. So every nested composite must be its own NAMED type, referenced
# by name. hoist() walks a type tree and hoists every nested composite into a
# named entry (returned in dependency order: children before parents, so the
# Go builder's single backward-reference pass resolves them).
_hoisted = {}  # ordered name -> typedef


def hoist(tdef, prefix):
    """Return a node usable as a TypeRef: primitives/refs stay inline; a nested
    composite is hoisted to a named type and replaced with a ref."""
    k = tdef["kind"]
    if k in ("primitive", "ref"):
        return tdef
    hoisted_def = _hoist_children(tdef, prefix)
    name = prefix
    n = 0
    while name in _hoisted and _hoisted[name] != hoisted_def:
        n += 1
        name = f"{prefix}{n}"
    _hoisted[name] = hoisted_def
    return {"kind": "ref", "name": name}


def _hoist_children(tdef, prefix):
    k = tdef["kind"]
    if k == "record":
        return {"kind": "record", "fields": [{"name": f["name"], "type": hoist(f["type"], f"{prefix}_{f['name']}")} for f in tdef["fields"]]}
    if k == "variant":
        out = []
        for c in tdef["cases"]:
            if "type" in c and c["type"] is not None:
                out.append({"name": c["name"], "type": hoist(c["type"], f"{prefix}_{c['name']}")})
            else:
                out.append({"name": c["name"]})
        return {"kind": "variant", "cases": out}
    if k == "list":
        return {"kind": "list", "elem": hoist(tdef["elem"], f"{prefix}_e")}
    if k == "tuple":
        return {"kind": "tuple", "elems": [hoist(e, f"{prefix}_{i}") for i, e in enumerate(tdef["elems"])]}
    if k == "option":
        return {"kind": "option", "elem": hoist(tdef["elem"], f"{prefix}_o")}
    if k == "result":
        d = {"kind": "result"}
        if tdef.get("ok") is not None:
            d["ok"] = hoist(tdef["ok"], f"{prefix}_ok")
        if tdef.get("err") is not None:
            d["err"] = hoist(tdef["err"], f"{prefix}_err")
        return d
    return tdef  # flags / enum: no nested types


def main():
    path = os.path.join(HERE, "oracle_flat.json")
    with open(path) as f:
        battery = json.load(f)

    # Hoist nested composites of every complex type, then emit hoisted
    # sub-types (dependency order) followed by the top-level type.
    emitted = []
    for name, tdef in COMPLEX_TYPES:
        _hoisted.clear()
        top = _hoist_children(tdef, name)
        for hname, hdef in _hoisted.items():
            emitted.append((hname, hdef))
        emitted.append((name, top))

    by_type_name = {t["name"] for t in battery["types"]}
    for name, tdef in emitted:
        if name not in by_type_name:
            battery["types"].append({"name": name, "type": tdef})
            by_type_name.add(name)
        else:  # overwrite so edits take effect
            for t in battery["types"]:
                if t["name"] == name:
                    t["type"] = tdef

    existing_val_names = {(v["type"], v["name"]) for v in battery["values"]}
    for tname, vname, val in COMPLEX_VALUES:
        key = (tname, vname)
        entry = {"type": tname, "name": vname, "value": val}
        if key in existing_val_names:
            for v in battery["values"]:
                if v["type"] == tname and v["name"] == vname:
                    v["value"] = val
        else:
            battery["values"].append(entry)

    with open(path, "w") as f:
        json.dump(battery, f, indent=2)
        f.write("\n")
    print(f"merged {len(COMPLEX_TYPES)} complex types, {len(COMPLEX_VALUES)} values into oracle_flat.json")

    # Regenerate the golden from the reference implementation.
    subprocess.run([sys.executable, os.path.join(HERE, "gen_oracle_flat.py")], check=True)
    print("regenerated oracle_flat_golden.json")


if __name__ == "__main__":
    main()
