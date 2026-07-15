package instance

import (
	"context"
	"sync"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
)

// CompileCache caches wazy.CompiledModule handles for core module bytes,
// letting repeated Instantiate calls against the SAME component skip
// re-compiling (re-JITting) its embedded core modules every time. Pass one
// via WithCompileCache to opt in; the default (no cache) behavior is
// unchanged -- every Instantiate call recompiles its core modules exactly as
// before.
//
// # Key
//
// A cache entry is keyed by the exact core-module bytes handed to
// CompileModule -- after any component-layer rewrite (e.g.
// rewriteEmptyImportModuleName's anonymous-import-name patch), so the key
// always matches what was actually compiled. This is sufficient because
// nothing about how a core module is COMPILED varies by caller here: the
// only ModuleConfig settings the component layer applies
// (WithName/WithStartFunctions) take effect at InstantiateModule time, not
// CompileModule time, so two instantiations that differ only in those still
// safely share one CompiledModule. A future caller needing compile-affecting
// ModuleConfig (e.g. a per-call FunctionListenerFactory) would need to fold
// that into the key too -- this cache does not do so today.
//
// # Ownership and lifetime
//
// A CompiledModule stored here is owned BY THE CACHE, not by any Instance
// built from it. Instance.Close only closes the api.Module instances it
// created (via InstantiateModule); it never closes the CompiledModule that
// produced them -- unlike Runtime.InstantiateWithConfig's implicit-compile
// path (which marks its CompiledModule closeWithModule so an instance's
// Close cleans it up too), CompileModule+InstantiateModule through this
// cache never sets that flag. So a cached CompiledModule, and the compiled
// code it references, stays alive across many Instance lifetimes until the
// cache itself is closed. Call Close on the CompileCache (typically
// alongside the Runtime it was built against) once it's no longer needed;
// after that, every Instance ever built through it is invalid.
//
// # Runtime pairing
//
// A CompiledModule belongs to the Runtime that compiled it -- wazy has no
// cross-Runtime CompiledModule reuse. A CompileCache must therefore be used
// with exactly one Runtime: the same r passed to every Instantiate call that
// also passes this cache. Using one CompileCache across two different
// Runtimes is a caller bug (the second Runtime's InstantiateModule would
// receive a CompiledModule it never compiled); CompileCache does not detect
// or guard against this -- pairing cache and Runtime correctly is the
// caller's responsibility.
//
// # Concurrency
//
// CompileCache is safe for concurrent use: multiple goroutines may call
// Instantiate with the same cache concurrently, including racing a first
// Instantiate of the same component bytes. A miss race compiles at most
// twice; the loser's redundant CompiledModule is closed immediately rather
// than stored, and every caller (winner or loser) still gets back a valid,
// usable CompiledModule.
type CompileCache struct {
	mu    sync.Mutex
	byKey map[string]wazy.CompiledModule
}

// NewCompileCache returns an empty CompileCache ready to pass to
// WithCompileCache. See CompileCache's doc for its ownership/lifetime and
// Runtime-pairing rules.
func NewCompileCache() *CompileCache {
	return &CompileCache{byKey: make(map[string]wazy.CompiledModule)}
}

// getOrCompile returns the CompiledModule for coreBytes, compiling via r on
// a miss and storing the result for future callers. See CompileCache's doc.
func (c *CompileCache) getOrCompile(ctx context.Context, r wazy.Runtime, coreBytes []byte) (wazy.CompiledModule, error) {
	key := string(coreBytes) // copies into the map key; safe even if the caller's coreBytes backing array is reused/mutated later

	c.mu.Lock()
	if cm, ok := c.byKey[key]; ok {
		c.mu.Unlock()
		return cm, nil
	}
	c.mu.Unlock()

	cm, err := r.CompileModule(ctx, coreBytes)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.byKey[key]; ok {
		// Lost a race with a concurrent compile of the same bytes: use the
		// winner's CompiledModule and don't leak the redundant one just built.
		_ = cm.Close(ctx)
		return existing, nil
	}
	c.byKey[key] = cm
	return cm, nil
}

// Close releases every CompiledModule this cache holds. Every Instance ever
// built through this cache becomes invalid once this returns -- its
// underlying compiled code is gone. Safe to call once, typically alongside
// closing the Runtime this cache was paired with.
func (c *CompileCache) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	for k, cm := range c.byKey {
		if err := cm.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(c.byKey, k)
	}
	return firstErr
}

// instantiateCoreModule instantiates coreBytes as a core module via r, using
// cfg's CompileCache (if any) to reuse an already-compiled CompiledModule
// instead of recompiling coreBytes. moduleCfg is applied at instantiate time
// only (WithName/WithStartFunctions, the only settings the component layer
// ever sets here) -- see CompileCache's doc for why sharing a compile across
// callers with different moduleCfg values is safe.
//
// With no cache configured (cfg == nil or cfg.compileCache == nil), this is
// exactly r.InstantiateWithConfig(ctx, coreBytes, moduleCfg) -- byte-for-byte
// the behavior every call site here had before CompileCache existed.
func instantiateCoreModule(ctx context.Context, r wazy.Runtime, cfg *config, coreBytes []byte, moduleCfg wazy.ModuleConfig) (api.Module, error) {
	if cfg == nil || cfg.compileCache == nil {
		return r.InstantiateWithConfig(ctx, coreBytes, moduleCfg)
	}
	compiled, err := cfg.compileCache.getOrCompile(ctx, r, coreBytes)
	if err != nil {
		return nil, err
	}
	return r.InstantiateModule(ctx, compiled, moduleCfg)
}
