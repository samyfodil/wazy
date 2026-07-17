package instance

import (
	"bytes"
	"context"
	"sync"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
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

	// decMu guards byComp -- a separate lock so a decode cache hit never
	// contends with a concurrent compile (byKey) lookup.
	decMu  sync.Mutex
	byComp map[string]*binary.Component

	// abiMu guards byABI -- per-export ABI metadata (flatten/type-resolution),
	// keyed by the shared *binary.Component pointer (stable across
	// instantiations because byComp returns the same pointer) and the export's
	// component func index.
	abiMu sync.Mutex
	byABI map[*binary.Component]map[uint32]*boundExportABI

	// planMu guards byPlan -- the graph import-discovery result (decoded import
	// signatures + per-core-module rewritten bytes), keyed by the shared
	// *binary.Component pointer. Pure over the immutable component, so it is
	// computed once and reused across instantiations (see graphPlanFor).
	planMu sync.Mutex
	byPlan map[*binary.Component]*graphPlan
}

// graphPlan is the cached, per-component output of the graph import-discovery
// pass: the core-level import signatures every lowered import needs, plus each
// embedded core module's bytes after the empty-import-name rewrite (identical
// every instantiation, so the main instantiation loop reuses them instead of
// re-slicing and re-rewriting). Immutable after graphPlanFor stores it.
type graphPlan struct {
	neededTypes map[string]map[string]coreFuncSig
	rewritten   [][]byte // indexed by core module index; ready to compile/instantiate
}

// NewCompileCache returns an empty CompileCache ready to pass to
// WithCompileCache. See CompileCache's doc for its ownership/lifetime and
// Runtime-pairing rules.
func NewCompileCache() *CompileCache {
	return &CompileCache{
		byKey:  make(map[string]wazy.CompiledModule),
		byComp: make(map[string]*binary.Component),
		byABI:  make(map[*binary.Component]map[uint32]*boundExportABI),
		byPlan: make(map[*binary.Component]*graphPlan),
	}
}

// abiFor returns the cached per-export ABI metadata for (comp, funcIdx),
// computing it via compute() on a miss and storing it for future
// instantiations. The metadata is a pure function of the immutable component,
// so one cached *boundExportABI is safely shared (read-only during invoke) by
// every Instance built from comp. Keyed by the comp POINTER, which is stable
// across instantiations only because getOrDecode hands back the same cached
// *binary.Component -- so abiFor is meaningful only alongside the decode cache
// (finalizeBoundExport passes a nil cache otherwise). compute() runs outside
// the lock; a rare miss race recomputes (pure, harmless) and the first stored
// wins.
func (c *CompileCache) abiFor(comp *binary.Component, funcIdx uint32, compute func() *boundExportABI) *boundExportABI {
	c.abiMu.Lock()
	if m := c.byABI[comp]; m != nil {
		if a, ok := m[funcIdx]; ok {
			c.abiMu.Unlock()
			return a
		}
	}
	c.abiMu.Unlock()

	a := compute()

	c.abiMu.Lock()
	defer c.abiMu.Unlock()
	m := c.byABI[comp]
	if m == nil {
		m = make(map[uint32]*boundExportABI)
		c.byABI[comp] = m
	}
	if existing, ok := m[funcIdx]; ok {
		return existing // lost a race -- both are equivalent (compute is pure)
	}
	m[funcIdx] = a
	return a
}

// graphPlanFor returns the cached graphPlan for comp, computing it once on a
// miss via compute. compute may fail (decode/rewrite of a malformed embedded
// core module); a failure is returned to the caller and NOT cached (it is
// deterministic and would recur, but the instantiation fails anyway). Mirrors
// abiFor's lock/recheck-on-store shape; a miss race computes at most twice and
// both results are equivalent (compute is pure over the immutable component).
func (c *CompileCache) graphPlanFor(comp *binary.Component, compute func() (*graphPlan, error)) (*graphPlan, error) {
	c.planMu.Lock()
	if p, ok := c.byPlan[comp]; ok {
		c.planMu.Unlock()
		return p, nil
	}
	c.planMu.Unlock()

	p, err := compute()
	if err != nil {
		return nil, err
	}

	c.planMu.Lock()
	defer c.planMu.Unlock()
	if existing, ok := c.byPlan[comp]; ok {
		return existing, nil // lost a race -- both are equivalent (compute is pure)
	}
	c.byPlan[comp] = p
	return p, nil
}

// getOrDecode returns the decoded *binary.Component for componentBytes,
// decoding on a miss and caching it for future Instantiate calls of the same
// component. The decoded Component is immutable after decode (ResolveType and
// friends are pure reads, and instantiation never writes comp.* fields), so
// one cached *binary.Component is safely shared -- and read concurrently -- by
// every Instance built from it, exactly like a cached CompiledModule. This
// skips re-parsing the component binary (~40% of a cached instantiation's
// allocations) on every call.
func (c *CompileCache) getOrDecode(componentBytes []byte) (*binary.Component, error) {
	// The hit path uses the map[string(byteslice)] idiom, which the Go compiler
	// lowers to a no-copy lookup -- so a cache hit allocates nothing (crucial:
	// componentBytes can be tens of KB). The full string key is materialized
	// only on the store (miss) path.
	c.decMu.Lock()
	if comp, ok := c.byComp[string(componentBytes)]; ok {
		c.decMu.Unlock()
		return comp, nil
	}
	c.decMu.Unlock()

	comp, err := binary.Decode(bytes.NewReader(componentBytes))
	if err != nil {
		return nil, err
	}

	key := string(componentBytes) // copy into the map key; safe if the caller reuses the backing array
	c.decMu.Lock()
	defer c.decMu.Unlock()
	if existing, ok := c.byComp[key]; ok {
		return existing, nil // lost a decode race -- reuse the winner (decode is pure, both are equivalent)
	}
	c.byComp[key] = comp
	return comp, nil
}

// getOrCompile returns the CompiledModule for coreBytes, compiling via r on
// a miss and storing the result for future callers. See CompileCache's doc.
func (c *CompileCache) getOrCompile(ctx context.Context, r wazy.Runtime, coreBytes []byte) (wazy.CompiledModule, error) {
	// Look up with map[string(coreBytes)] -- the compiler elides the []byte->string
	// copy for a map index expression, so a cache HIT (the steady state) allocates
	// nothing. The key is only materialized on the miss/store path below, exactly
	// as getOrDecode does. (Previously this copied the whole module bytes on every
	// hit, ~250 KB/op on the real_hello graph.)
	c.mu.Lock()
	if cm, ok := c.byKey[string(coreBytes)]; ok {
		c.mu.Unlock()
		return cm, nil
	}
	c.mu.Unlock()

	cm, err := r.CompileModule(ctx, coreBytes)
	if err != nil {
		return nil, err
	}

	key := string(coreBytes) // copy into the map key; safe if the caller reuses the backing array
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
	return instantiateCoreModuleCacheable(ctx, r, cfg, coreBytes, moduleCfg, true)
}

// instantiateCoreModuleCacheable is instantiateCoreModule with an explicit
// cacheable flag. Pass false when coreBytes carry a per-instantiation-unique
// name (an empty-import rewrite -- see rewriteEmptyImportModuleName's changed
// return): such bytes are never reused, so caching them only grows the cache.
func instantiateCoreModuleCacheable(ctx context.Context, r wazy.Runtime, cfg *config, coreBytes []byte, moduleCfg wazy.ModuleConfig, cacheable bool) (api.Module, error) {
	if !cacheable || cfg == nil || cfg.compileCache == nil {
		return r.InstantiateWithConfig(ctx, coreBytes, moduleCfg)
	}
	compiled, err := cfg.compileCache.getOrCompile(ctx, r, coreBytes)
	if err != nil {
		return nil, err
	}
	return r.InstantiateModule(ctx, compiled, moduleCfg)
}
