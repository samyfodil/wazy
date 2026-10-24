package instance

import (
	"bytes"
	"embed"
	"path"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// ctenvFS holds testdata/ctenv: see its README.
//
//go:embed testdata/ctenv/*.wasm
var ctenvFS embed.FS

func decodeCTEnvFixture(t *testing.T, name string) *binary.Component {
	t.Helper()
	b, err := ctenvFS.ReadFile(path.Join("testdata/ctenv", name+".wasm"))
	if err != nil {
		t.Fatal(err)
	}
	comp, err := binary.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return comp
}

// Each fixture is type-checked as a root component would be, and refused
// with the reason given -- or, for "", accepted.
func TestCTEnvCheck(t *testing.T) {
	for name, want := range map[string]string{
		// Outer aliases.
		"outer_count_too_big":          "invalid outer alias count of 5",
		"outer_index_oob":              "unknown type 9: type index out of bounds",
		"outer_resource":               "type index 0 refers to resources not defined in the current component",
		"outer_func_with_resource":     "type index 2 refers to resources not defined in the current component",
		"outer_instance_with_resource": "type index 1 refers to resources not defined in the current component",
		// A resource a component or instance type binds is not free in it,
		// so such a type crosses into a nested component.
		"outer_component_bound": "",
		"outer_instance_bound":  "",
		"root_outer":            "invalid outer alias count of 1",
		"component_outer_bad":   "invalid outer alias count of 3",
		// Value and func types.
		"valref_oob":          "unknown type 9: type index out of bounds",
		"valref_not_value":    "type index 0 is a func, not a defined value type",
		"record_second_field": "unknown type 9: type index out of bounds",
		"param_oob":           "parameter `x`: unknown type 9",
		"result_oob":          "result: unknown type 9",
		"lift_type_oob":       "unknown type 9: type index out of bounds",
		"lift_not_func":       "type index 0 is not a function type",
		// Declarations of component and instance types.
		"decl_type_oob":         "declaration 0: unknown type 5: type index out of bounds",
		"decl_eq_oob":           "declaration 0: unknown type 5",
		"decl_not_func":         "declaration 1: type index 0 is not a function type",
		"decl_dup":              "export name `a` conflicts with previous name `a`",
		"decl_alias_inst_oob":   "unknown instance 0: instance index out of bounds",
		"decl_alias_no_export":  "instance 0 has no export named `a`",
		"decl_alias_wrong_sort": "export `a` for instance 0 is not a type",
		"decl_alias_instance":   "",
		"decl_alias_outer_bad":  "invalid outer alias count of 3",
		"decl_alias_func":       "aliases in a component or instance type may only refer to types or instances",
		// Imports and exports.
		"import_oob":      "import `f`: unknown type 9",
		"export_oob":      "export `f`: unknown function 9: function index out of bounds",
		"root_dup_import": "name `a` conflicts with previous name `a`",
		"reexports":       "",
		// A definition naming itself: the index is not yet defined.
		"import_self_type":     "unknown import 0: import index out of bounds (it names the definition it is part of)",
		"export_self_instance": "unknown export 0: export index out of bounds (it names the definition it is part of)",
		// Export ascriptions: checked against the item, and -- for `(sub
		// resource)` -- a fresh resource, for a resource item only.
		"ascribe_mismatch":          "ascribed type of export is not compatible with item's type: type mismatch for export `g`",
		"ascribe_oob":               "ascribed type of export is not compatible with item's type: unknown type 9",
		"ascribe_resource":          "export `v`: ascribed type of export is not compatible with item's type: type mismatch for export `v`: expected resource, found defined type",
		"ascribe_instance":          "",
		"local_resource_unexported": "local resource type found in export but not exported itself",
		"core_arg_later":            "core instance 0: unknown core instance 5: instance index out of bounds",
	} {
		t.Run(name, func(t *testing.T) {
			err := typeCheck(decodeCTEnvFixture(t, name), &config{})
			requireSubErr(t, err, want)
		})
	}
}

// A check's envs serve the checks after it (ctEnvs), whatever they checked
// and however it ended: the verdicts do not depend on what ran before.
func TestTypeCheckReusesEnvs(t *testing.T) {
	good, bad := decodeCTEnvFixture(t, "ascribe_instance"), decodeCTEnvFixture(t, "local_resource_unexported")
	for range 3 {
		for _, fixture := range []string{"ascribe_resource", "decl_alias_wrong_sort", "outer_component_bound"} {
			want := map[string]string{
				"ascribe_resource":      "expected resource, found defined type",
				"decl_alias_wrong_sort": "export `a` for instance 0 is not a type",
			}[fixture]
			requireSubErr(t, typeCheck(decodeCTEnvFixture(t, fixture), &config{}), want)
		}
		requireSubErr(t, typeCheck(good, &config{}), "")
		requireSubErr(t, typeCheck(bad, &config{}), "local resource type found in export but not exported itself")
		requireSubErr(t, coreLinkError(decodeCTEnvFixture(t, "core_arg_later"), -1, nil), "unknown core instance 5: instance index out of bounds")
	}
}

// An ascribed instance type's `(sub resource)` exports name the resources
// the item has at the same places, and the ascribed `(sub resource)` of a
// type export is a resource of its own.
func TestCTEnvAscribedResources(t *testing.T) {
	env := newCTEnv(decodeCTEnvFixture(t, "ascribe_instance"), nil)
	self, err := env.selfType()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := env.selfType(); again != self {
		t.Error("selfType is not memoized")
	}
	imported, _ := ctLookup(self.imports, "i")
	exported, _ := ctLookup(self.exports, "j")
	ir, _ := ctLookup(imported.inst.exports, "r")
	jr, _ := ctLookup(exported.inst.exports, "r")
	if ir.ty.res == nil || jr.ty.res == nil || ir.ty.res == jr.ty.res {
		t.Errorf("the ascribed export's resource %p must be fresh, not the imported %p", jr.ty.res, ir.ty.res)
	}
	jf, _ := ctLookup(exported.inst.exports, "f")
	if jf.fn.results[0].t.res != jr.ty.res {
		t.Error("the ascribed export's func must return its own resource")
	}

	env = newCTEnv(decodeCTEnvFixture(t, "ascribe_resource"), nil)
	u, err := env.exportAt(0)
	if err != nil {
		t.Fatal(err)
	}
	tImport, _ := env.importAt(0)
	if u.ty.res == nil || u.ty.res == tImport.ty.res || !env.created[u.ty.res] {
		t.Error("`(export \"u\" (type $t) (type (sub resource)))` must be a resource of its own, made here")
	}
}

// selfType reports the import or export it cannot type.
func TestCTEnvSelfTypeErrors(t *testing.T) {
	_, err := newCTEnv(decodeCTEnvFixture(t, "import_oob"), nil).selfType()
	requireSubErr(t, err, "import `f`: unknown type 9")
	_, err = newCTEnv(decodeCTEnvFixture(t, "export_oob"), nil).selfType()
	requireSubErr(t, err, "export `f`: unknown function 9")
	// checkSupplied: an import whose declared type cannot be computed.
	err = checkSupplied(newCTEnv(decodeCTEnvFixture(t, "import_oob"), nil), 0, &binary.Component{})
	requireSubErr(t, err, "import `f`: unknown type 9")
	// A hand-built component has no index spaces to check.
	if err := typeCheck(&binary.Component{}, &config{}); err != nil {
		t.Errorf("typeCheck(hand-built) = %v", err)
	}
}

// The branches a decoded binary cannot reach: what the decoder never
// produces, but a hand-built descriptor can carry.
func TestCTEnvUndecodable(t *testing.T) {
	s := &ctDeclScope{types: []ctType{{val: prim("u8")}}}
	nine, zero := uint32(9), uint32(0)
	if _, err := ctValRef(s, binary.TypeRef{}); err == nil || !strings.Contains(err.Error(), "neither a primitive nor a type index") {
		t.Errorf("empty TypeRef: %v", err)
	}
	if _, err := ctConvert(s, nil); err == nil || !strings.Contains(err.Error(), "not decoded") {
		t.Errorf("nil TypeDesc: %v", err)
	}
	if _, err := ctConvertVal(s, binary.FuncDesc{}); err == nil || !strings.Contains(err.Error(), "unsupported type definition func") {
		t.Errorf("a func as a value type: %v", err)
	}
	m, err := ctConvertVal(s, binary.MapDesc{Key: binary.TypeRef{Primitive: "string"}, Value: binary.TypeRef{TypeIndex: &zero}})
	if err != nil || m.kind != ctvMap || m.elems[1].prim != "u8" {
		t.Errorf("map: %+v, %v", m, err)
	}
	// Legacy named results.
	f, err := ctConvertFunc(s, binary.FuncDesc{Results: binary.FuncResults{Named: []binary.FuncResult{{Name: "a", Type: binary.TypeRef{Primitive: "u8"}}}}})
	if err != nil || len(f.results) != 1 || f.results[0].name != "a" {
		t.Errorf("named result: %+v, %v", f, err)
	}
	_, err = ctConvertFunc(s, binary.FuncDesc{Results: binary.FuncResults{Named: []binary.FuncResult{{Name: "a", Type: binary.TypeRef{TypeIndex: &nine}}}}})
	requireSubErr(t, err, "result `a`: unknown type 9")
	if ctHasBorrow(nil) {
		t.Error("nothing holds no borrow")
	}
	// A value-sort externdesc, and entity: values are not modelled.
	if _, err := ctEntityOfDesc(s, binary.ExternDesc{Sort: ctSortValue}); err == nil {
		t.Error("a value externdesc was typed")
	}
	env := newCTEnv(&binary.Component{}, nil)
	if _, err := env.entityAt(ctSortValue, 0); err == nil {
		t.Error("a value entity was typed")
	}
	if _, err := env.aliasedExport(binary.AliasDef{Sort: ctSortFunc, TargetKind: 0x02}); err == nil || !strings.Contains(err.Error(), "must name an instance export") {
		t.Errorf("an outer func alias: %v", err)
	}
	if env.enclosing() != nil {
		t.Error("a root env encloses nothing")
	}
	if (ctSubst{}).fn(&ctFunc{}) == nil {
		t.Error("an empty substitution lost a func")
	}
}
