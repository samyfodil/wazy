package storebind

import (
	"context"
	"sort"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component/bindgen/internal/storebind/tree/app"
	sharedkv "github.com/samyfodil/wazy/component/bindgen/internal/storebind/tree/example/store/v1.0.0/kv"
	"github.com/samyfodil/wazy/component/bindgen/internal/storebind/with"
	"github.com/samyfodil/wazy/component/witgo"
)

// BenchmarkRun runs the guest through each form of its bindings -- one file,
// a tree, and one file using the tree's kv -- with the same host: what
// crosses into a shared package costs what it does in one file.
func BenchmarkRun(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	single, err := Instantiate(ctx, r, storeWasm, Imports{Kv: &plainKV{buckets: map[Bucket]map[string][]byte{}}})
	if err != nil {
		b.Fatal(err)
	}
	host := &sharedKV{buckets: map[sharedkv.Bucket]map[string][]byte{}}
	tree, err := app.Instantiate(ctx, r, storeWasm, app.Imports{Kv: host})
	if err != nil {
		b.Fatal(err)
	}
	w, err := with.Instantiate(ctx, r, storeWasm, with.Imports{Kv: host})
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name string
		run  func(context.Context) (string, error)
	}{{"single", single.Run}, {"tree", tree.Run}, {"with", w.Run}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := c.run(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// plainKV is sharedKV against the single file's types.
type plainKV struct {
	next    Bucket
	buckets map[Bucket]map[string][]byte
	dropped []Bucket
}

func (s *plainKV) NewBucket(_ context.Context, _ string) (Bucket, error) {
	s.next++
	s.buckets[s.next] = map[string][]byte{}
	return s.next, nil
}

func (s *plainKV) BucketPut(_ context.Context, self Bucket, key string, value []byte) error {
	b, ok := s.buckets[self]
	if !ok {
		return nil
	}
	b[key] = value
	return nil
}

func (s *plainKV) BucketGet(_ context.Context, self Bucket, key string) (*[]byte, error) {
	v, ok := s.buckets[self][key]
	if !ok {
		return nil, nil
	}
	return &v, nil
}

func (s *plainKV) ListKeys(_ context.Context, b Bucket) (witgo.Result[[]string, Error], error) {
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

func (s *plainKV) DropBucket(_ context.Context, self Bucket) error {
	s.dropped = append(s.dropped, self)
	delete(s.buckets, self)
	return nil
}
