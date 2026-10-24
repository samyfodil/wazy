package wasip2

import (
	"context"
	_ "embed"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// What wac composes with --import-dependencies, and the two real components
// composed: the Rust provider and the C consumer of examples/compose. See
// testdata/component_imports/README.md.
var (
	//go:embed testdata/component_imports/composed.wasm
	wacComposedWasm []byte
	//go:embed testdata/component_imports/provider.wasm
	composeProviderWasm []byte
	//go:embed testdata/component_imports/consumer.wasm
	composeConsumerWasm []byte
)

const (
	providerImport = "unlocked-dep=<wazy:provider>"
	consumerImport = "unlocked-dep=<wazy:consumer>"
)

// A component a real composer emitted imports two components -- one per
// dependency it did not embed -- and instantiates them itself, the provider
// with the WASI instances it imports and the consumer with the provider's
// greeter. Supplied with the two real components it was composed against, it
// runs end to end: every greeting crosses from the C consumer through the
// Rust provider and back.
func TestComponentImports_WacImportDependencies(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	opts := append(WithWASI(WASIConfig{}),
		component.WithComponentImport(providerImport, composeProviderWasm),
		component.WithComponentImport(consumerImport, composeConsumerWasm))
	in, err := component.Instantiate(ctx, r, wacComposedWasm, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	got, err := in.Call(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	want := []component.Value{[]component.Value{"Hello, wazy #42! (from Rust)", "a (via Rust)", "empty-len=0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run() = %#v, want %#v", got, want)
	}
}

// The composition declares what it expects of each import -- the type of the
// binary it was composed against -- and a component that does not have that
// type is refused before anything runs, whichever import it is supplied for.
func TestComponentImports_WacImportDependenciesRefusesTheWrongComponent(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	for _, tc := range []struct {
		name               string
		provider, consumer []byte
		want               string
	}{
		{
			// The consumer imports the greeter, which the declared provider
			// type does not -- the composition would never supply it.
			name:     "consumer for provider",
			provider: composeConsumerWasm, consumer: composeConsumerWasm,
			want: `component import "unlocked-dep=<wazy:provider>": the supplied component does not match the declared type: missing import named ` + "`wazy:compose/greeter@0.1.0`",
		},
		{
			// The provider imports WASI, which the declared consumer type
			// does not -- nor does it export `run`.
			name:     "provider for consumer",
			provider: composeProviderWasm, consumer: composeProviderWasm,
			want: `component import "unlocked-dep=<wazy:consumer>": the supplied component does not match the declared type: missing import named ` + "`wasi:cli/environment@0.2.3`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := append(WithWASI(WASIConfig{}),
				component.WithComponentImport(providerImport, tc.provider),
				component.WithComponentImport(consumerImport, tc.consumer))
			in, err := component.Instantiate(ctx, r, wacComposedWasm, opts...)
			if err == nil {
				in.Close(ctx)
				t.Fatal("instantiated; want it refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	_, err := component.Instantiate(ctx, r, wacComposedWasm, WithWASI(WASIConfig{})...)
	if err == nil || !strings.Contains(err.Error(), `component import "unlocked-dep=<wazy:provider>" was not supplied`) {
		t.Fatalf("with nothing supplied: %v", err)
	}
}

//go:embed testdata/component_imports/http_root.wasm
var httpRootWasm []byte

// A root that imports an HTTP handler as a component and re-exports its
// wasi:http/incoming-handler serves requests: the host mints the
// incoming-request and response-outparam handles in the table of the
// component that defines the handler -- the one whose canonical ABI reads
// them -- not in the root's, which only re-exposes the interface.
func TestComponentImports_ServeHTTPThroughSuppliedHandler(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	opts := append(WithWASI(WASIConfig{EnableHTTP: true}),
		component.WithComponentImport("unlocked-dep=<wazy:handler>", realHTTPIncomingWasm))
	in, err := component.Instantiate(ctx, r, httpRootWasm, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	for _, path := range []string{"/hello?x=1", "/again"} {
		status, _, body, _, err := serveHTTPCall(in, ctx, "GET", mustURL(path), http.Header{}, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if want := "method=Method::Get path=" + path + "\n"; status != 200 || string(body) != want {
			t.Fatalf("%s: %d %q, want 200 %q", path, status, body, want)
		}
	}
}
