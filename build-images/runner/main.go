// Command runner executes a WASI 0.2 component under wazy for the build-image
// tests. It drives the component through the public component API and the
// imports/wasip2 host, the same way an embedding Go program would:
//
//	runner run  component.wasm         # wasi:cli/command: call run, stream stdio
//	runner http component.wasm [path]  # wasi:http/proxy: serve one GET, print status and body
//
// The exit status is the guest's: 0 for a clean run (including exit(ok)), 1
// for run returning err or any instantiation/trap error.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/imports/wasip2"
	"github.com/samyfodil/wazy/sys"
)

func main() {
	os.Exit(doMain(os.Args[1:], os.Stdout, os.Stderr))
}

func doMain(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || (args[0] != "run" && args[0] != "http") {
		fmt.Fprintln(stderr, "usage: runner run|http component.wasm [path]")
		return 2
	}
	bin, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	// WebAssembly 3.0 features: componentize-py's CPython uses extended-const,
	// and other toolchains are moving the same way.
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV3))
	defer r.Close(ctx)

	cfg := wasip2.Config{Stdout: stdout, Stderr: stderr, EnableHTTP: args[0] == "http"}
	inst, err := component.Instantiate(ctx, r, bin, wasip2.With(cfg)...)
	if err != nil {
		fmt.Fprintln(stderr, "instantiate:", err)
		return 1
	}
	defer inst.Close(ctx)

	if args[0] == "http" {
		path := "/"
		if len(args) > 2 {
			path = args[2]
		}
		rec := httptest.NewRecorder()
		wasip2.Handler(inst).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		fmt.Fprintf(stdout, "%d\n%s", rec.Code, rec.Body.String())
		return 0
	}

	run := ""
	for _, name := range inst.InstanceExports() {
		if strings.HasPrefix(name, "wasi:cli/run@") {
			run = name
		}
	}
	if run == "" {
		fmt.Fprintln(stderr, "component exports no wasi:cli/run")
		return 1
	}
	res, err := inst.CallExport(ctx, run, "run")
	var exit *sys.ExitError
	switch {
	case errors.As(err, &exit):
		return int(exit.ExitCode())
	case err != nil:
		fmt.Fprintln(stderr, "run:", err)
		return 1
	}
	if len(res) == 1 {
		if rv, ok := res[0].(component.ResultValue); ok && rv.IsErr {
			return 1
		}
	}
	return 0
}
