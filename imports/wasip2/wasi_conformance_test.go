package wasip2

import (
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/component/componenttest"
	"github.com/samyfodil/wazy/internal/component/wit"
)

// This file checks wazy's WASI host imports against the REFERENCE WASI 0.2
// interface definitions (testdata/wasi-wit, copied verbatim from the wasip2
// crate's vendored WebAssembly/WASI sources) rather than against wazy's own
// beliefs about them.
//
// # Why this exists
//
// Everything else that could have caught interface drift does not. The
// wasi-testsuite job in CI targets wasi_snapshot_preview1, not the component
// world. The 35 differential fixtures in conformance_test.go only cover what
// their guests happen to call, and a guest cannot call a func no host
// implements -- so a method wazy never registered is invisible to them by
// construction. The wast suites cover Canonical ABI *value* lifting, not WASI
// interfaces. So the whole wasi:* surface had no check tying it to the spec,
// which is how `[method]incoming-response.headers` stayed unimplemented and
// how a misspelled registration would sit silently behind the graph engine's
// trap-stub fallback, indistinguishable from "not implemented yet".
//
// # What it can and cannot catch
//
// It catches names: a registered func that does not exist in the reference
// (a typo, a renamed method, an interface that moved), and it reports the
// converse -- reference funcs wazy does not implement -- as an explicit,
// reviewable inventory rather than a silence.
//
// It does NOT catch semantics. That types.wit requires `fields.entries` to
// return names "in the original casing and in the order in which they will
// be serialized" is prose no signature check can enforce; that one was found
// by reading the reference and is pinned by a behavioral test instead. A
// green run here means the surface lines up, not that each func is right.

//go:embed testdata/wasi-wit/*.wit
var wasiWITFS embed.FS

// witFuncNames parses one reference .wit file and returns, per interface, the
// set of canonical func names a component would import from it -- the same
// spelling withImportCustom registers ("[method]fields.get",
// "[constructor]fields", "[static]fields.from-list", or a bare "get-stdin").
func witFuncNames(t *testing.T, file string) map[string]map[string]bool {
	t.Helper()
	src, err := wasiWITFS.ReadFile(path.Join("testdata/wasi-wit", file))
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	pkg, err := wit.Parse(file, string(src))
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	out := map[string]map[string]bool{}
	for _, item := range pkg.Items {
		iface, ok := item.(*wit.Interface)
		if !ok || iface.Name == "" {
			continue
		}
		names := map[string]bool{}
		for _, ii := range iface.Items {
			switch v := ii.(type) {
			case *wit.InterfaceFunc:
				names[v.Name] = true
			case *wit.TypeDef:
				res, isResource := v.Type.(*wit.Resource)
				if !isResource {
					continue
				}
				for _, m := range res.Methods {
					switch {
					case m.IsConstructor:
						names["[constructor]"+v.Name] = true
					case m.IsStatic:
						names["[static]"+v.Name+"."+m.Name] = true
					default:
						names["[method]"+v.Name+"."+m.Name] = true
					}
				}
			}
		}
		// Key by the package-qualified interface name, minus the version --
		// exactly what mkImportKey reduces a registration to.
		out[strings.Split(pkg.Name, "@")[0]+"/"+iface.Name] = names
	}
	return out
}

// referenceWASI parses every vendored reference file into one
// iface -> funcs index.
func referenceWASI(t *testing.T) map[string]map[string]bool {
	t.Helper()
	entries, err := wasiWITFS.ReadDir("testdata/wasi-wit")
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".wit") {
			continue
		}
		for iface, funcs := range witFuncNames(t, e.Name()) {
			all[iface] = funcs
		}
	}
	if len(all) == 0 {
		t.Fatal("no interfaces parsed from the reference WIT; the embed or the parser is broken")
	}
	return all
}

// registeredWASI returns every wasi:* host import wazy registers, keyed the
// same way, with every optional surface switched on so nothing is missed.
func registeredWASI(t *testing.T) map[string]map[string]bool {
	t.Helper()
	h := componenttest.New(WithWASI(WASIConfig{
		AllowTCP: true, AllowUDP: true, EnableHTTP: true,
	})...)
	out := map[string]map[string]bool{}
	for iface, names := range h.Registered() {
		if !strings.HasPrefix(iface, "wasi:") {
			continue
		}
		out[iface] = map[string]bool{}
		for _, n := range names {
			out[iface][n] = true
		}
	}
	return out
}

// TestWASIConformance_RegisteredFuncsExistInReference is the assertion: every
// name wazy registers must be a real func in the reference WIT. A failure
// here means a guest's import silently resolves to a trap stub (wazy
// registered a name nothing declares), or an interface has moved.
func TestWASIConformance_RegisteredFuncsExistInReference(t *testing.T) {
	ref := referenceWASI(t)
	got := registeredWASI(t)

	for iface, funcs := range got {
		refFuncs, ok := ref[iface]
		if !ok {
			t.Errorf("wazy registers imports for %q, which does not exist in the reference WIT", iface)
			continue
		}
		for name := range funcs {
			if !refFuncs[name] {
				t.Errorf("wazy registers %q %q, which the reference WIT does not declare", iface, name)
			}
		}
	}
}

// TestWASIConformance_UnimplementedInventory reports what the reference
// declares and wazy does not. It does not fail: an unimplemented func is a
// deliberate, documented state (see wasi.go's package doc on the trap-stub
// fallback). The point is that the list is visible and reviewable in CI
// output, so a gap is a decision someone made rather than something nobody
// noticed -- and so closing one shows up as a diff here.
func TestWASIConformance_UnimplementedInventory(t *testing.T) {
	ref := referenceWASI(t)
	got := registeredWASI(t)

	var lines []string
	for iface, refFuncs := range ref {
		impl := got[iface]
		if impl == nil {
			continue // an interface wazy implements none of; not this test's subject
		}
		var missing []string
		for name := range refFuncs {
			if !impl[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			lines = append(lines, fmt.Sprintf("%s: %s", iface, strings.Join(missing, ", ")))
		}
	}
	sort.Strings(lines)
	t.Logf("reference funcs not implemented, in interfaces wazy partially implements:\n%s", strings.Join(lines, "\n"))
}
