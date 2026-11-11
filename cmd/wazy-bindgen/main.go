// Command wazy-bindgen writes Go bindings for a WIT world, for a host running
// its components on wazy. See package bindgen for what it generates.
//
//	//go:generate go run github.com/samyfodil/wazy/cmd/wazy-bindgen -wit wit -world app -package app -o app.go
//
// -wit names a directory read as wit.ParseDir reads one (its .wit files and
// deps/), or a single .wit file. -wasm names a compiled component instead,
// whose WIT is read from it (wit.FromComponent): its world is "root", so
// -world may be left out.
//
//	wazy-bindgen -wasm plugin.wasm -package plugin -o plugin.go
//
// -out writes a tree of packages instead of one file (bindgen.GenerateTree):
// one per imported interface, which every world generated into the same tree
// shares, and one for the world, named -package. -import is the tree's import
// path, read from the go.mod above it when left out.
//
//	wazy-bindgen -wasm a.wasm -package a -out bindings
//	wazy-bindgen -wasm b.wasm -package b -out bindings
//
// -with uses an existing package's bindings of an imported interface rather
// than writing them, in either form; it may be given more than once:
//
//	wazy-bindgen -wasm b.wasm -package b -o b.go -with wasi:http/types=example.com/bindings/wasi/http/v0.2.0/types
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samyfodil/wazy/component/bindgen"
	"github.com/samyfodil/wazy/component/wit"
)

func main() { os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr)) }

// withFlag is -with, given once per interface.
type withFlag map[string]string

func (w withFlag) String() string { return "" }

func (w withFlag) Set(v string) error {
	iface, path, ok := strings.Cut(v, "=")
	if !ok || iface == "" || path == "" {
		return fmt.Errorf("want interface=import/path, got %q", v)
	}
	w[iface] = path
	return nil
}

func cli(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wazy-bindgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	witPath := fs.String("wit", "wit", "WIT directory or file")
	wasmPath := fs.String("wasm", "", "compiled component to read the WIT of, instead of -wit")
	world := fs.String("world", "", "world to bind (\"root\" for -wasm)")
	pkgName := fs.String("package", "", "Go package name of the output, or of the world's package with -out")
	out := fs.String("o", "", "output file; standard output if empty")
	outDir := fs.String("out", "", "directory to write a tree of packages to, instead of -o")
	importPath := fs.String("import", "", "import path of -out; read from go.mod if empty")
	skip := fs.String("skip", "wasi:", "comma-separated prefixes of imported interfaces to leave unbound")
	with := withFlag{}
	fs.Var(with, "with", "interface=import/path of a package already binding it; repeatable")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	err := func() error {
		if *outDir != "" && *out != "" {
			return fmt.Errorf("-o and -out are exclusive")
		}
		pkg, w, err := load(*witPath, *wasmPath, flagSet(fs, "wit"), *world)
		if err != nil {
			return err
		}
		if *pkgName == "" {
			return fmt.Errorf("-package is required")
		}
		opts := bindgen.Options{Package: *pkgName, World: w, Skip: []string{}, With: with}
		for _, p := range strings.Split(*skip, ",") {
			if p = strings.TrimSpace(p); p != "" {
				opts.Skip = append(opts.Skip, p)
			}
		}
		if *outDir != "" {
			return writeTree(pkg, opts, *outDir, *importPath)
		}
		src, err := bindgen.Generate(pkg, opts)
		if err != nil {
			return err
		}
		if *out == "" {
			_, err = stdout.Write(src)
			return err
		}
		return os.WriteFile(*out, src, 0o644)
	}()
	if err != nil {
		fmt.Fprintln(stderr, "wazy-bindgen:", err)
		return 1
	}
	return 0
}

// load reads the WIT to bind, and the world to bind in it.
func load(witPath, wasmPath string, withWIT bool, world string) (*wit.Package, string, error) {
	if wasmPath != "" {
		if withWIT {
			return nil, "", fmt.Errorf("-wit and -wasm are exclusive")
		}
		if world == "" {
			world = "root"
		}
		wasm, err := os.ReadFile(wasmPath)
		if err != nil {
			return nil, "", err
		}
		pkg, err := wit.FromComponent(wasm)
		return pkg, world, err
	}
	if world == "" {
		return nil, "", fmt.Errorf("-world is required")
	}
	fi, err := os.Stat(witPath)
	if err != nil {
		return nil, "", err
	}
	var pkg *wit.Package
	if fi.IsDir() {
		pkg, err = wit.ParseDir(witPath)
	} else {
		pkg, err = wit.ParseFile(witPath)
	}
	return pkg, world, err
}

// writeTree writes GenerateTree's packages under dir, whose import path is
// importPath, or read from the go.mod above dir.
func writeTree(pkg *wit.Package, opts bindgen.Options, dir, importPath string) error {
	if importPath == "" {
		var err error
		if importPath, err = modulePath(dir); err != nil {
			return err
		}
	}
	files, err := bindgen.GenerateTree(pkg, opts, importPath)
	if err != nil {
		return err
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.Src, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// modulePath is the import path of dir: the module path in the go.mod of the
// nearest directory at or above it holding one, joined with the rest.
func modulePath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if mod, err := readModule(filepath.Join(d, "go.mod")); err == nil {
			rel, err := filepath.Rel(d, abs)
			if err != nil {
				return "", err
			}
			if rel == "." {
				return mod, nil
			}
			return mod + "/" + filepath.ToSlash(rel), nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no go.mod at or above %s: give -import", dir)
		}
	}
}

// readModule is the module path a go.mod declares.
func readModule(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if mod, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(mod), `"`), nil
		}
	}
	return "", fmt.Errorf("%s declares no module", path)
}

// flagSet reports whether the flag name was given.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}
