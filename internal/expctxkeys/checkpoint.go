package expctxkeys

import "sync/atomic"

// EnableSnapshotterKey is a context key to indicate that snapshotting should be enabled.
// The context.Context passed to a exported function invocation should have this key set
// to a non-nil value, and host functions will be able to retrieve it using SnapshotterKey.
type EnableSnapshotterKey struct{}

// SnapshotterKey is a context key to access a Snapshotter from a host function.
// It is only present if EnableSnapshotter was set in the function invocation context.
type SnapshotterKey struct{}

// SnapshotterEnabled is a process-wide latch set once the first context
// carrying EnableSnapshotterKey is created (see experimental.WithSnapshotter).
// The snapshotter is an experimental, opt-in feature that almost no caller
// enables, yet every wasm entry and every interpreted call instruction used
// to pay for a ctx.Value(EnableSnapshotterKey{}) lookup (a walk of the
// context.Context Value chain) to find out. Checking this latch first lets
// engines skip that walk entirely until a process has actually used the
// snapshotter at least once.
//
// It is only ever set to true, never cleared: a process that has enabled the
// snapshotter once keeps paying for the ctx.Value chain walk from then on,
// which is fine since enabling it is already an explicit, deliberate opt-in.
var SnapshotterEnabled atomic.Bool
