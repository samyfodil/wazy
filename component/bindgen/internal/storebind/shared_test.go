package storebind

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component/bindgen/internal/storebind/tree/app"
	sharedkv "github.com/samyfodil/wazy/component/bindgen/internal/storebind/tree/example/store/v1.0.0/kv"
	"github.com/samyfodil/wazy/component/bindgen/internal/storebind/with"
	"github.com/samyfodil/wazy/component/witgo"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// tree/ is store.wit's world app as a tree of packages, and with/ the same
// world as one file using the tree's kv: cmd/wazy-bindgen -out tree, and -o
// with/bindings.go -with example:store/kv=.../tree/example/store/v1.0.0/kv.

// sharedKV implements kv.Host, the interface's own package's, which both
// worlds' bindings use.
type sharedKV struct {
	next    sharedkv.Bucket
	buckets map[sharedkv.Bucket]map[string][]byte
	dropped []sharedkv.Bucket
}

func (s *sharedKV) NewBucket(_ context.Context, _ string) (sharedkv.Bucket, error) {
	s.next++
	s.buckets[s.next] = map[string][]byte{}
	return s.next, nil
}

func (s *sharedKV) BucketPut(_ context.Context, self sharedkv.Bucket, key string, value []byte) error {
	b, ok := s.buckets[self]
	if !ok {
		return fmt.Errorf("no bucket %d", self)
	}
	b[key] = value
	return nil
}

func (s *sharedKV) BucketGet(_ context.Context, self sharedkv.Bucket, key string) (*[]byte, error) {
	v, ok := s.buckets[self][key]
	if !ok {
		return nil, nil
	}
	return &v, nil
}

func (s *sharedKV) ListKeys(_ context.Context, b sharedkv.Bucket) (witgo.Result[[]string, sharedkv.Error], error) {
	m, ok := s.buckets[b]
	if !ok {
		return witgo.Err[[]string](sharedkv.ErrorNoSuchBucket()), nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return witgo.Ok[[]string, sharedkv.Error](keys), nil
}

func (s *sharedKV) DropBucket(_ context.Context, self sharedkv.Bucket) error {
	s.dropped = append(s.dropped, self)
	delete(s.buckets, self)
	return nil
}

// One host implementation of the interface's package serves both worlds'
// bindings: the tree's, and the single file using the tree's kv.
func TestSharedInterfacePackage(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	host := &sharedKV{buckets: map[sharedkv.Bucket]map[string][]byte{}}

	a, err := app.Instantiate(ctx, r, storeWasm, app.Imports{Kv: host})
	require.NoError(t, err)
	defer a.Close(ctx)
	got, err := a.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, "hello wazy|missing=true|keys=greeting,target", got)

	w, err := with.Instantiate(ctx, r, storeWasm, with.Imports{Kv: host})
	require.NoError(t, err)
	defer w.Close(ctx)
	got, err = w.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, "hello wazy|missing=true|keys=greeting,target", got)

	require.Equal(t, []sharedkv.Bucket{1, 2}, host.dropped)
}
