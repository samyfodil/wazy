package wazy

import (
	"context"
	"testing"

	internalsock "github.com/samyfodil/wazy/internal/sock"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestInstantiateModule_doesNotMutateSharedConfig guards A13: a ModuleConfig is
// documented immutable and may be reused across (concurrent) instantiations, so
// InstantiateModule must not write the per-ctx socket config into the caller's
// value. Before the fix it did `config.sockConfig = sockConfig` on the passed
// pointer, leaking one ctx's listener into every later instantiation sharing
// the config (and racing under concurrency).
func TestInstantiateModule_doesNotMutateSharedConfig(t *testing.T) {
	r := NewRuntime(testCtx)
	defer r.Close(testCtx)

	compiled, err := r.CompileModule(testCtx, binaryNamedZero)
	require.NoError(t, err)

	cfg := NewModuleConfig()
	mc := cfg.(*moduleConfig)

	sockCtx := context.WithValue(testCtx, internalsock.ConfigKey{},
		(&internalsock.Config{}).WithTCPListener("127.0.0.1", 0))

	mod, err := r.InstantiateModule(sockCtx, compiled, cfg)
	require.NoError(t, err)
	defer mod.Close(testCtx)

	// The caller's config must be untouched: the sockConfig belongs to the
	// instance built from sockCtx, not to the reusable config.
	require.Nil(t, mc.sockConfig)
}
