package wasm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestWatchCall(t *testing.T) {
	t.Run("a context that is never done", func(t *testing.T) {
		w := WatchCall(context.Background(), nil)
		require.Nil(t, w)
		require.False(t, w.Canceled())
		w.Stop()
	})
	t.Run("already done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ran := false
		w := WatchCall(ctx, func() { ran = true })
		require.True(t, w.Canceled())
		require.True(t, ran)
	})
	t.Run("done later", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fired := make(chan struct{})
		w := WatchCall(ctx, func() { close(fired) })
		require.False(t, w.Canceled())
		cancel()
		<-fired
		require.True(t, w.Canceled())
	})
	t.Run("stopped before done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		w := WatchCall(ctx, func() { t.Error("ran after Stop") })
		w.Stop()
		cancel()
		time.Sleep(10 * time.Millisecond)
		require.False(t, w.Canceled())
	})
}

func TestFailCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := recoverError(func() { FailCanceled(ctx) })
	require.True(t, errors.Is(err, context.Canceled))
}

func recoverError(f func()) (err error) {
	defer func() { err = recover().(error) }()
	f()
	return nil
}
