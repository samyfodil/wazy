package wasm

import (
	"context"
	"sync/atomic"
)

// CallWatch ends a call when its context is done. Engines register one lazily,
// where the call returns to Go anyway (see WatchCall), and look at it there.
type CallWatch struct {
	canceled atomic.Bool
	stop     func() bool
}

// WatchCall returns a watcher for a call's context, or nil if the context can
// never be done. context.AfterFunc starts no goroutine until the context is done;
// then, on a goroutine of its own, the watcher is marked canceled and onDone, if
// not nil, runs. A context that is already done does both at once.
func WatchCall(ctx context.Context, onDone func()) *CallWatch {
	done := ctx.Done()
	if done == nil {
		return nil
	}
	w := &CallWatch{}
	fire := func() {
		w.canceled.Store(true)
		if onDone != nil {
			onDone()
		}
	}
	select {
	case <-done:
		fire()
	default:
		w.stop = context.AfterFunc(ctx, fire)
	}
	return w
}

// Canceled reports whether the call's context is done. It is false on a nil
// watcher.
func (w *CallWatch) Canceled() bool {
	return w != nil && w.canceled.Load()
}

// Stop unregisters the watcher once the call is over. It is a no-op on a nil
// watcher.
func (w *CallWatch) Stop() {
	if w != nil && w.stop != nil {
		w.stop()
	}
}

// FailCanceled ends a call whose context is done the way a trap does, by
// panicking with an error that wraps the context's. The module stays usable.
func FailCanceled(ctx context.Context) {
	panic(context.Cause(ctx))
}
