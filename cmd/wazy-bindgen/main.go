// Command wazy-bindgen writes Go bindings for a WIT world, for a host running
// its components on wazy. See package bindgen for what it generates.
//
//	//go:generate go run github.com/samyfodil/wazy/cmd/wazy-bindgen -wit wit -world app -package app -o app.go
//
// -wit names a directory read as wit.ParseDir reads one (its .wit files and
// deps/), or a single .wit file.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/samyfodil/wazy/component/bindgen"
	"github.com/samyfodil/wazy/component/wit"
)

func main() { os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr)) }

func cli(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wazy-bindgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	witPath := fs.String("wit", "wit", "WIT directory or file")
	world := fs.String("world", "", "world to bind")
	pkgName := fs.String("package", "", "Go package name of the output")
	out := fs.String("o", "", "output file; standard output if empty")
	skip := fs.String("skip", "wasi:", "comma-separated prefixes of imported interfaces to leave unbound")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	src, err := run(*witPath, *world, *pkgName, *skip)
	if err == nil {
		if *out == "" {
			_, err = stdout.Write(src)
		} else {
			err = os.WriteFile(*out, src, 0o644)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "wazy-bindgen:", err)
		return 1
	}
	return 0
}

func run(witPath, world, pkgName, skip string) ([]byte, error) {
	if world == "" || pkgName == "" {
		return nil, fmt.Errorf("-world and -package are required")
	}
	fi, err := os.Stat(witPath)
	if err != nil {
		return nil, err
	}
	var pkg *wit.Package
	if fi.IsDir() {
		pkg, err = wit.ParseDir(witPath)
	} else {
		pkg, err = wit.ParseFile(witPath)
	}
	if err != nil {
		return nil, err
	}
	opts := bindgen.Options{Package: pkgName, World: world, Skip: []string{}}
	for _, p := range strings.Split(skip, ",") {
		if p = strings.TrimSpace(p); p != "" {
			opts.Skip = append(opts.Skip, p)
		}
	}
	return bindgen.Generate(pkg, opts)
}
