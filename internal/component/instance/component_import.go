package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// WithComponentImport supplies componentBytes, a component binary, for the
// component import name: `(import "name" (component ...))`.
//
// A component import is a component DEFINITION, not an instance: the
// importing component instantiates it itself, as often as its instance
// section says, with arguments of its own choosing -- exactly what it does
// with a component nested in its own binary. That is why this takes the
// component's bytes, the same input Instantiate takes, rather than a live
// Instance; with WithCompileCache, its decode and the compilation of its core
// modules are cached exactly as Instantiate's own are.
//
// Before anything is instantiated, the supplied component's type -- its
// imports and exports -- is checked against the type the import declares,
// with the Component Model's subtyping rules: it may import less and export
// more than declared, and what it does import and export must match. A
// mismatch fails Instantiate with the reason.
//
// A name the component does not import as a component is ignored, as
// WithImport ignores an interface the component does not import.
func WithComponentImport(name string, componentBytes []byte) Option {
	return func(c *config) {
		if c.componentImports == nil {
			c.componentImports = map[string][]byte{}
		}
		c.componentImports[name] = componentBytes
	}
}

// ctCheckKey names one type check whose success a CompileCache remembers:
// a component's own (supplied == nil), or a supplied component against the
// component import name of comp.
type ctCheckKey struct {
	comp, supplied *binary.Component
	name           string
}

// typeCheck checks comp's instantiations -- each one's arguments against the
// instantiated component's imports, for every component nested in it too --
// and supplies its component imports from cfg.componentImports, each checked
// against its declared type.
//
// It runs before any import is resolved, because a mismatch is a property of
// the component alone: a component instantiating its imported component with
// an argument of the wrong type is refused for that, whether or not anything
// was supplied for the import. A CompileCache remembers each check that
// passed, so a component instantiated again through one is not checked again.
func typeCheck(comp *binary.Component, cfg *config) error {
	if !comp.Decoded { // a hand-built component (tests) has no index spaces
		return nil
	}
	var env *ctEnv
	defer func() {
		if env != nil {
			env.release()
		}
	}()
	if !cfg.typeChecked(ctCheckKey{comp: comp}) {
		env = newCTEnv(comp, nil)
		if err := env.check(); err != nil {
			return fmt.Errorf("component/instance: %w", err)
		}
		cfg.rememberTypeChecked(ctCheckKey{comp: comp})
	}
	for i, im := range comp.Imports {
		if im.ExternType != ctSortComponent {
			continue
		}
		src, ok := cfg.componentImports[im.Name]
		if !ok {
			continue // instantiateGraph's import gate says what is missing
		}
		supplied, err := decodeSupplied(cfg, src)
		if err != nil {
			return fmt.Errorf("component/instance: component import %q: decode the supplied component: %w", im.Name, err)
		}
		key := ctCheckKey{comp: comp, supplied: supplied, name: im.Name}
		if !cfg.typeChecked(key) {
			if env == nil {
				env = newCTEnv(comp, nil)
			}
			if err := checkSupplied(env, uint32(i), supplied); err != nil {
				return fmt.Errorf("component/instance: component import %q: %w", im.Name, err)
			}
			cfg.rememberTypeChecked(key)
		}
		if cfg.scope.supplied == nil {
			cfg.scope.supplied = map[string]*binary.Component{}
		}
		cfg.scope.supplied[im.Name] = supplied
	}
	return nil
}

// checkSupplied checks that supplied, given for comp's component import i
// (env is comp's), has a subtype of the type the import declares.
func checkSupplied(env *ctEnv, i uint32, supplied *binary.Component) error {
	declared, err := env.importAt(i)
	if err != nil {
		return err
	}
	senv := newCTEnv(supplied, nil)
	defer senv.release()
	if err := senv.check(); err != nil { // which computes senv.self
		return fmt.Errorf("the supplied component is invalid: %w", err)
	}
	if err := ctComponentSub(senv.self, declared.comp); err != nil {
		return fmt.Errorf("the supplied component does not match the declared type: %w", err)
	}
	return nil
}

func decodeSupplied(cfg *config, src []byte) (*binary.Component, error) {
	if cfg.compileCache != nil {
		return cfg.compileCache.getOrDecode(src)
	}
	return binary.DecodeBytes(src)
}

func (cfg *config) typeChecked(k ctCheckKey) bool {
	c := cfg.compileCache
	if c == nil {
		return false
	}
	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	return c.checked[k]
}

func (cfg *config) rememberTypeChecked(k ctCheckKey) {
	c := cfg.compileCache
	if c == nil {
		return
	}
	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	if c.checked == nil {
		c.checked = map[ctCheckKey]bool{}
	}
	c.checked[k] = true
}
