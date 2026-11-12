package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file checks the canonical options of a canon definition, as
// wasmparser's check_options, check_lower and ComponentFuncType::lower do:
// each option given at most once, the options that make sense together, and
// the memory and realloc a func's lowering needs. What needs a core func's
// type -- a realloc's or post-return's signature, and a lifted func's -- is
// checked when the export binds (checkLiftSignature), where the module that
// defines the func exists and its type costs nothing to read.

// Canonical option kinds (Binary.md, canonopt).
const (
	ctOptUTF8        = 0x00
	ctOptUTF16       = 0x01
	ctOptLatin1UTF16 = 0x02
	ctOptMemory      = 0x03
	ctOptRealloc     = 0x04
	ctOptPostReturn  = 0x05
	ctOptAsync       = 0x06
	ctOptCallback    = 0x07
)

// The most core values a func's params and results flatten to before they
// pass through memory instead (CanonicalABI.md, flatten_functype).
const (
	ctMaxFlatParams      = 16
	ctMaxFlatAsyncParams = 4
	ctMaxFlatResults     = 1
)

// ctOptions are a canon's canonical options, once checked.
type ctOptions struct {
	memory, realloc, postReturn, async, callback bool
}

func (o ctOptions) requireMemory() error {
	if !o.memory {
		return fmt.Errorf("canonical option `memory` is required")
	}
	return nil
}

func (o ctOptions) requireRealloc() error {
	if err := o.requireMemory(); err != nil {
		return err
	}
	if !o.realloc {
		return fmt.Errorf("canonical option `realloc` is required")
	}
	return nil
}

func (o ctOptions) requireMemoryIf(when bool) error {
	if when {
		return o.requireMemory()
	}
	return nil
}

func (o ctOptions) requireReallocIf(when bool) error {
	if when {
		return o.requireRealloc()
	}
	return nil
}

func (o ctOptions) checkLower() error {
	if o.postReturn {
		return fmt.Errorf("canonical option `post-return` cannot be specified for lowerings")
	}
	if o.callback {
		return fmt.Errorf("canonical option `callback` cannot be specified for lowerings")
	}
	return nil
}

func (o ctOptions) requireSync(where string) error {
	if o.async {
		return fmt.Errorf("cannot specify `async` option on `%s`", where)
	}
	return nil
}

// checkCanonOptions checks cn's canonical options, for the canons that take
// them.
func (e *ctEnv) checkCanonOptions(cn binary.Canon) error {
	switch cn.Kind {
	case binary.CanonKindLift, binary.CanonKindLower, binary.CanonKindTaskReturn,
		binary.CanonKindStreamRead, binary.CanonKindStreamWrite,
		binary.CanonKindFutureRead, binary.CanonKindFutureWrite,
		binary.CanonKindErrorContextNew, binary.CanonKindErrorContextDebugMessage:
	default:
		return nil
	}
	o, err := e.options(cn.Opts)
	if err != nil {
		return err
	}
	switch cn.Kind {
	case binary.CanonKindLift:
		t, err := e.typeAt(cn.TypeIdx)
		if err != nil {
			return err
		}
		if t.fn == nil {
			return fmt.Errorf("type index %d is not a function type", cn.TypeIdx)
		}
		return o.lower(t.fn, false)
	case binary.CanonKindLower:
		if err := o.checkLower(); err != nil {
			return err
		}
		f, err := e.funcAt(cn.FuncIdx)
		if err != nil {
			return err
		}
		return o.lower(f, true)
	case binary.CanonKindTaskReturn:
		if o.realloc {
			return fmt.Errorf("cannot specify `realloc` option on `task.return`")
		}
		if o.postReturn {
			return fmt.Errorf("cannot specify `post-return` option on `task.return`")
		}
		if err := o.checkLower(); err != nil {
			return err
		}
		if err := o.requireSync("task.return"); err != nil {
			return err
		}
		f := ctNewFunc(false, 0, 0)
		r := cn.TaskReturnResult
		if r.Unnamed != nil {
			v, err := ctValRef(e, *r.Unnamed)
			if err != nil {
				return err
			}
			f.params = []ctField{{name: "v", t: v}}
		}
		for _, n := range r.Named {
			v, err := ctValRef(e, n.Type)
			if err != nil {
				return err
			}
			f.params = append(f.params, ctField{name: n.Name, t: v})
		}
		return o.lower(f, true)
	case binary.CanonKindStreamRead, binary.CanonKindStreamWrite,
		binary.CanonKindFutureRead, binary.CanonKindFutureWrite:
		t, err := e.typeAt(cn.TypeIdx)
		if err != nil {
			return err
		}
		var elem *ctVal
		if t.val != nil && len(t.val.elems) > 0 {
			elem = t.val.elems[0]
		}
		if err := o.requireMemoryIf(elem != nil); err != nil {
			return err
		}
		if cn.Kind == binary.CanonKindStreamRead || cn.Kind == binary.CanonKindFutureRead {
			if err := o.requireReallocIf(elem != nil && ctContainsPtr(elem)); err != nil {
				return err
			}
		}
		return o.checkLower()
	case binary.CanonKindErrorContextNew:
		if err := o.requireMemory(); err != nil {
			return err
		}
		if err := o.requireSync("error-context.new"); err != nil {
			return err
		}
		return o.checkLower()
	default: // binary.CanonKindErrorContextDebugMessage
		if err := o.requireRealloc(); err != nil {
			return err
		}
		if err := o.requireSync("error-context.debug-message"); err != nil {
			return err
		}
		return o.checkLower()
	}
}

// options checks a canon's options, each given at most once and the ones
// that need another given with it.
func (e *ctEnv) options(opts []binary.CanonOpt) (ctOptions, error) {
	var o ctOptions
	encoding := -1
	for _, op := range opts {
		switch op.Kind {
		case ctOptUTF8, ctOptUTF16, ctOptLatin1UTF16:
			if encoding >= 0 {
				return o, fmt.Errorf("canonical encoding option `%s` conflicts with option `%s`", ctEncodingName(byte(encoding)), ctEncodingName(op.Kind))
			}
			encoding = int(op.Kind)
		case ctOptMemory:
			if o.memory {
				return o, fmt.Errorf("canonical option `memory` is specified more than once")
			}
			if n := e.coreMemoryCount(); op.Idx >= n {
				return o, fmt.Errorf("unknown memory %d: memory index out of bounds", op.Idx)
			}
			o.memory = true
		case ctOptRealloc:
			if o.realloc {
				return o, fmt.Errorf("canonical option `realloc` is specified more than once")
			}
			o.realloc = true
		case ctOptPostReturn:
			if o.postReturn {
				return o, fmt.Errorf("canonical option `post-return` is specified more than once")
			}
			o.postReturn = true
		case ctOptAsync:
			if o.async {
				return o, fmt.Errorf("canonical option `async` is specified more than once")
			}
			o.async = true
		case ctOptCallback:
			if o.callback {
				return o, fmt.Errorf("canonical option `callback` is specified more than once")
			}
			o.callback = true
		}
	}
	switch {
	case !o.async && o.callback:
		return o, fmt.Errorf("cannot specify callback without async")
	case o.async && o.postReturn:
		return o, fmt.Errorf("cannot specify post-return function in async")
	case o.realloc && !o.memory:
		return o, fmt.Errorf("canonical option `realloc` requires `memory` to also be specified")
	}
	return o, nil
}

// lower is ComponentFuncType::lower's checks of the memory and realloc a
// func's lowering (lower) or lifting (!lower) needs.
func (o ctOptions) lower(f *ctFunc, lower bool) error {
	maxParams := ctMaxFlatParams
	if lower && o.async {
		maxParams = ctMaxFlatAsyncParams
	}
	flat := 0
	for _, p := range f.params {
		var err error
		if lower {
			err = o.requireMemoryIf(ctContainsPtr(p.t))
		} else {
			err = o.requireReallocIf(ctContainsPtr(p.t))
		}
		if err != nil {
			return err
		}
		if flat += ctFlatCount(p.t); flat > maxParams {
			if err := o.requireMemory(); err != nil {
				return err
			}
			if !lower {
				if err := o.requireRealloc(); err != nil {
					return err
				}
			}
			break
		}
	}
	if len(f.results) == 0 {
		return nil
	}
	ptr, count := false, 0
	for _, r := range f.results {
		ptr = ptr || ctContainsPtr(r.t)
		count += ctFlatCount(r.t)
	}
	if err := o.requireReallocIf(lower && ptr); err != nil {
		return err
	}
	switch {
	case !o.async:
		return o.requireMemoryIf(count > ctMaxFlatResults)
	case lower:
		return o.requireMemory()
	default:
		// An async lift returns through task.return, whose params are
		// what overflows.
		return o.requireMemoryIf(count > ctMaxFlatParams || ptr)
	}
}

// ctEncodingName is wasmparser's name for a string-encoding option.
func ctEncodingName(kind byte) string {
	return [...]string{ctOptUTF8: "utf8", ctOptUTF16: "utf16", ctOptLatin1UTF16: "latin1-utf16"}[kind]
}

// coreMemoryCount is the size of the core memory index space: the core
// export aliases of memories.
func (e *ctEnv) coreMemoryCount() uint32 {
	n := uint32(0)
	for _, al := range e.comp.Aliases {
		if al.Sort == 0x00 && al.CoreSort == 0x02 && al.TargetKind == 0x01 {
			n++
		}
	}
	return n
}

// ctContainsPtr reports whether v holds, at any depth, a string or list:
// something lowered as a pointer into memory.
func ctContainsPtr(v *ctVal) bool {
	switch v.kind {
	case ctvPrim:
		return v.prim == "string"
	case ctvList, ctvMap:
		return true
	case ctvRecord, ctvVariant, ctvTuple, ctvFixedList, ctvOption, ctvResult:
		for _, el := range v.elems {
			if el != nil && ctContainsPtr(el) {
				return true
			}
		}
	}
	return false
}

// ctFlatCount is how many core values v flattens to.
func ctFlatCount(v *ctVal) int {
	switch v.kind {
	case ctvPrim:
		if v.prim == "string" {
			return 2
		}
		return 1
	case ctvList, ctvMap:
		return 2
	case ctvFixedList:
		return int(v.length) * ctFlatCount(v.elems[0])
	case ctvRecord, ctvTuple:
		n := 0
		for _, el := range v.elems {
			n += ctFlatCount(el)
		}
		return n
	case ctvVariant, ctvOption, ctvResult:
		n := 0
		for _, el := range v.elems {
			if el != nil {
				n = max(n, ctFlatCount(el))
			}
		}
		return 1 + n
	case ctvFlags:
		return (len(v.labels) + 31) / 32
	default: // enum, own, borrow, stream, future
		return 1
	}
}
