package storebind

import (
	"context"
	_ "embed"
	"fmt"
	"sort"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component/witgo"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed store.wasm
var storeWasm []byte

// kv implements the generated Kv: the same host examples/custom-wit writes by
// hand against component.Values, here in Go types.
type kv struct {
	next    Bucket
	buckets map[Bucket]map[string][]byte
	dropped []Bucket
	log     []string
}

func (s *kv) NewBucket(_ context.Context, name string) (Bucket, error) {
	s.next++
	s.buckets[s.next] = map[string][]byte{}
	s.log = append(s.log, fmt.Sprintf("created %q (rep %d)", name, s.next))
	return s.next, nil
}

func (s *kv) BucketPut(_ context.Context, self Bucket, key string, value []byte) error {
	b, ok := s.buckets[self]
	if !ok {
		return fmt.Errorf("no bucket %d", self)
	}
	b[key] = value
	return nil
}

func (s *kv) BucketGet(_ context.Context, self Bucket, key string) (*[]byte, error) {
	v, ok := s.buckets[self][key]
	if !ok {
		return nil, nil
	}
	return &v, nil
}

func (s *kv) ListKeys(_ context.Context, b Bucket) (witgo.Result[[]string, Error], error) {
	m, ok := s.buckets[b]
	if !ok {
		return witgo.Err[[]string](ErrorNoSuchBucket()), nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return witgo.Ok[[]string, Error](keys), nil
}

func (s *kv) DropBucket(_ context.Context, self Bucket) error {
	s.dropped = append(s.dropped, self)
	delete(s.buckets, self)
	return nil
}

// The generated bindings drive the guest examples/custom-wit drives by hand,
// to the same result, and the guest's drop of its bucket reaches DropBucket.
func TestGeneratedBindings(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	host := &kv{buckets: map[Bucket]map[string][]byte{}}
	app, err := Instantiate(ctx, r, storeWasm, Imports{Kv: host})
	require.NoError(t, err)
	defer app.Close(ctx)

	got, err := app.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, "hello wazy|missing=true|keys=greeting,target", got)
	require.Equal(t, []string{`created "cache" (rep 1)`}, host.log)
	require.Equal(t, []Bucket{1}, host.dropped)
}

func TestInstantiateRequiresImports(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	_, err := Instantiate(ctx, r, storeWasm, Imports{})
	require.EqualError(t, err, "app: imports.Kv is nil")
}
