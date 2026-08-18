package instance

import (
	"context"
	"errors"
	"testing"
)

// Destructors for one resource type tag compose rather than replace.
//
// These call what dtorFor resolves, which is what the guest's resource.drop
// canon invokes (graph.go). handleTable.Drop is the host-side removal and
// deliberately runs no destructor, so it is the wrong door for this.
//
// A tag is not owned by a single host implementation: wasi:filesystem,
// wasi:sockets and wasi:http all mint input-stream and output-stream handles
// under the shared stream tags, each backed by its own state and its own
// disjoint range of reps. Replacing on a second registration would have let
// whichever registered last take over releasing every stream, and the others
// would have gone on leaking with nothing to report it -- which is exactly the
// bug this behavior exists to prevent.
func TestRegisterDtorComposes(t *testing.T) {
	const tag uint32 = 42

	t.Run("every destructor runs", func(t *testing.T) {
		tbl := newHandleTable()
		var order []string
		tbl.registerDtor(tag, func(context.Context, uint32) error {
			order = append(order, "first")
			return nil
		})
		tbl.registerDtor(tag, func(context.Context, uint32) error {
			order = append(order, "second")
			return nil
		})

		if err := tbl.dtorFor(tag)(context.Background(), 7); err != nil {
			t.Fatalf("dtor: %v", err)
		}
		if len(order) != 2 || order[0] != "first" || order[1] != "second" {
			t.Errorf("destructors ran %v, want [first second] in registration order", order)
		}
	})

	t.Run("each sees the rep", func(t *testing.T) {
		tbl := newHandleTable()
		var seen []uint32
		for range 2 {
			tbl.registerDtor(tag, func(_ context.Context, rep uint32) error {
				seen = append(seen, rep)
				return nil
			})
		}
		if err := tbl.dtorFor(tag)(context.Background(), 99); err != nil {
			t.Fatalf("dtor: %v", err)
		}
		if len(seen) != 2 || seen[0] != 99 || seen[1] != 99 {
			t.Errorf("destructors saw %v, want both to see rep 99", seen)
		}
	})

	// A destructor that fails must not stop the others from releasing what
	// they own, or one subsystem's error becomes another's leak.
	t.Run("a failure does not skip the rest", func(t *testing.T) {
		tbl := newHandleTable()
		boom := errors.New("boom")
		ran := false
		tbl.registerDtor(tag, func(context.Context, uint32) error { return boom })
		tbl.registerDtor(tag, func(context.Context, uint32) error {
			ran = true
			return nil
		})

		err := tbl.dtorFor(tag)(context.Background(), 1)
		if !errors.Is(err, boom) {
			t.Errorf("dtor error = %v, want it to report the failing destructor", err)
		}
		if !ran {
			t.Error("the second destructor did not run after the first failed")
		}
	})

	t.Run("the first error is the one reported", func(t *testing.T) {
		tbl := newHandleTable()
		first, second := errors.New("first"), errors.New("second")
		tbl.registerDtor(tag, func(context.Context, uint32) error { return first })
		tbl.registerDtor(tag, func(context.Context, uint32) error { return second })

		if err := tbl.dtorFor(tag)(context.Background(), 1); !errors.Is(err, first) {
			t.Errorf("dtor error = %v, want the first failure", err)
		}
	})

	// One registration is still just that registration, with no wrapper.
	t.Run("a single destructor is unaffected", func(t *testing.T) {
		tbl := newHandleTable()
		calls := 0
		tbl.registerDtor(tag, func(context.Context, uint32) error {
			calls++
			return nil
		})
		if err := tbl.dtorFor(tag)(context.Background(), 3); err != nil {
			t.Fatalf("dtor: %v", err)
		}
		if calls != 1 {
			t.Errorf("destructor ran %d times, want 1", calls)
		}
	})
}
