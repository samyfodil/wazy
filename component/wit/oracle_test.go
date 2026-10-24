package wit

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// testdata/wit-parser holds wit-parser's own UI tests, verbatim from
// bytecodealliance/wasm-tools at a pinned commit (see its README). Each
// ships its result: a .wit.json of the resolved packages, or a .wit.result
// error under parse-fail/.
//
//go:embed testdata/wit-parser
var testdata embed.FS

// TestWitParserUI parses each of wit-parser's inputs -- a .wit file, or a
// directory with deps/ -- and compares the result with wit-parser's: the
// same packages, interfaces, types, funcs and world items.
func TestWitParserUI(t *testing.T) {
	const dir = "testdata/wit-parser/ui"
	entries, err := fs.ReadDir(testdata, dir)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".wit")
		if e.Name() == "parse-fail" || !e.IsDir() && name == e.Name() {
			continue
		}
		ran++
		t.Run(name, func(t *testing.T) {
			want, err := fs.ReadFile(testdata, path.Join(dir, name+".wit.json"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseInput(t, path.Join(dir, e.Name()), e.IsDir())
			if err != nil {
				t.Fatalf("wit-parser accepts it: %v", err)
			}
			if g, w := summarize(got), summarizeJSON(t, want); !slices.Equal(g, w) {
				t.Fatalf("got\n\t%s\nwant\n\t%s", strings.Join(g, "\n\t"), strings.Join(w, "\n\t"))
			}
		})
	}
	if ran == 0 {
		t.Fatalf("no inputs in %s", dir)
	}
}

// parseInput parses one of wit-parser's inputs: a directory as ParseFS reads
// it, or a single file with its nested packages split out.
func parseInput(t *testing.T, p string, isDir bool) (*Package, error) {
	if isDir {
		sub, err := fs.Sub(testdata, p)
		if err != nil {
			t.Fatal(err)
		}
		return ParseFS(sub, ".")
	}
	src, err := fs.ReadFile(testdata, p)
	if err != nil {
		t.Fatal(err)
	}
	return ParseFS(splitNested(t, string(src)), ".")
}

// splitNested turns a source with nested "package x { ... }" blocks, which
// wit-parser's tests use for dependencies, into a root file and one deps/
// file per block, the layout ParseFS reads.
func splitNested(t *testing.T, src string) fstest.MapFS {
	fsys := fstest.MapFS{}
	open := regexp.MustCompile(`(?m)^package\s+([^\s;{]+)\s*\{`)
	for i := 0; ; i++ {
		loc := open.FindStringSubmatchIndex(src)
		if loc == nil {
			break
		}
		depth, end := 0, -1
		for j := loc[1] - 1; j < len(src) && end < 0; j++ {
			switch src[j] {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					end = j
				}
			}
		}
		if end < 0 {
			t.Fatalf("unbalanced package block %s", src[loc[2]:loc[3]])
		}
		body := "package " + src[loc[2]:loc[3]] + ";\n" + src[loc[1]:end]
		fsys[fmt.Sprintf("deps/dep%d.wit", i)] = &fstest.MapFile{Data: []byte(body)}
		src = src[:loc[0]] + src[end+1:]
	}
	fsys["root.wit"] = &fstest.MapFile{Data: []byte(src)}
	return fsys
}

// summarize lists what a parse resolved, one sorted line per package,
// interface and world, in the form summarizeJSON gives wit-parser's.
func summarize(root *Package) []string {
	var out []string
	for _, p := range append([]*Package{root}, root.Deps...) {
		out = append(out, "package "+p.Name)
		for _, i := range p.Interfaces {
			types := append(slices.Clone(i.scope.order), i.scope.used...)
			var funcs []string
			for _, f := range i.Funcs {
				funcs = append(funcs, f.Name)
			}
			out = append(out, ifaceLine(p.Name, lastSegment(i.Name), types, funcs))
		}
		for _, w := range p.Worlds {
			var imports, exports []string
			for _, it := range w.Imports {
				imports = append(imports, it.Name)
			}
			for _, it := range w.Exports {
				exports = append(exports, it.Name)
			}
			out = append(out, worldLine(p.Name, w.Name, imports, exports))
		}
	}
	slices.Sort(out)
	return out
}

func ifaceLine(pkg, name string, types, funcs []string) string {
	slices.Sort(types)
	slices.Sort(funcs)
	return fmt.Sprintf("interface %s/%s types=%v funcs=%v", pkg, name, types, funcs)
}

func worldLine(pkg, name string, imports, exports []string) string {
	slices.Sort(imports)
	slices.Sort(exports)
	return fmt.Sprintf("world %s/%s imports=%v exports=%v", pkg, name, imports, exports)
}

// witJSON is the part of wit-parser's resolved JSON that summarizeJSON reads.
type witJSON struct {
	Worlds []struct {
		Name    string                  `json:"name"`
		Imports map[string]witWorldItem `json:"imports"`
		Exports map[string]witWorldItem `json:"exports"`
		Package int                     `json:"package"`
	} `json:"worlds"`
	Interfaces []struct {
		Name      *string                    `json:"name"`
		Types     map[string]int             `json:"types"`
		Functions map[string]json.RawMessage `json:"functions"`
		Package   *int                       `json:"package"`
	} `json:"interfaces"`
	Packages []struct {
		Name       string         `json:"name"`
		Interfaces map[string]int `json:"interfaces"`
		Worlds     map[string]int `json:"worlds"`
	} `json:"packages"`
}

type witWorldItem struct {
	Interface *struct {
		ID int `json:"id"`
	} `json:"interface"`
	Type *int `json:"type"`
}

// summarizeJSON lists wit-parser's resolved packages as summarize does ours.
// A world's types are not world items here, so they are skipped; an
// interface it imports by name is listed by its qualified name.
func summarizeJSON(t *testing.T, src []byte) []string {
	var d witJSON
	if err := json.Unmarshal(src, &d); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range d.Packages {
		out = append(out, "package "+p.Name)
		for name, id := range p.Interfaces {
			i := d.Interfaces[id]
			var types, funcs []string
			for ty := range i.Types {
				types = append(types, ty)
			}
			for f := range i.Functions {
				funcs = append(funcs, f)
			}
			out = append(out, ifaceLine(p.Name, name, types, funcs))
		}
		for name, id := range p.Worlds {
			w := d.Worlds[id]
			names := func(items map[string]witWorldItem) []string {
				var out []string
				for key, it := range items {
					switch {
					case it.Type != nil:
						continue
					case it.Interface != nil && strings.HasPrefix(key, "interface-"):
						i := d.Interfaces[it.Interface.ID]
						key = qualify(d.Packages[*i.Package].Name, *i.Name)
					}
					out = append(out, key)
				}
				return out
			}
			out = append(out, worldLine(p.Name, name, names(w.Imports), names(w.Exports)))
		}
	}
	slices.Sort(out)
	return out
}

// wordingDiffers are wit-parser failing inputs that fail here too, for the
// same reason, under different words.
var wordingDiffers = map[string]string{
	"async-bad-world.wit":      "x is defined more than once",
	"async-bad-world2.wit":     "x is declared more than once",
	"async-bad-world3.wit":     "x is defined more than once",
	"async-bad-world4.wit":     "x is declared more than once",
	"bad-world-type1.wit":      "a is defined more than once",
	"world-same-fields2.wit":   "foo is declared more than once",
	"world-same-fields3.wit":   "foo is declared more than once",
	"world-top-level-func.wit": "foo is defined more than once",
}

// TestWitParserParseFail runs wit-parser's own failing inputs: each must
// fail here too, with wit-parser's text -- or, for a syntax error
// ("expected ..."), which comes from this package's own parser, with its own
// "expected ...", and for wordingDiffers, with the text listed there.
func TestWitParserParseFail(t *testing.T) {
	const dir = "testdata/wit-parser/ui/parse-fail"
	entries, err := fs.ReadDir(testdata, dir)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".result") {
			continue
		}
		ran++
		t.Run(e.Name(), func(t *testing.T) {
			result, err := fs.ReadFile(testdata, path.Join(dir, strings.TrimSuffix(e.Name(), ".wit")+".wit.result"))
			if err != nil {
				t.Fatal(err)
			}
			want, _, _ := strings.Cut(string(result), "\n")
			if alt, ok := wordingDiffers[e.Name()]; ok {
				want = alt
			} else if strings.HasPrefix(want, "expected ") {
				want = "expected "
			}
			_, gotErr := parseInput(t, path.Join(dir, e.Name()), e.IsDir())
			switch {
			case gotErr == nil:
				t.Fatalf("parsed; wit-parser rejects it: %s", want)
			case !strings.Contains(gotErr.Error(), want):
				t.Fatalf("error %q\nwant it to contain %q", gotErr, want)
			}
		})
	}
	if ran == 0 {
		t.Fatal("no inputs")
	}
}
