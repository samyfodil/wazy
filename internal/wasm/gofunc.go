package wasm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"

	"github.com/tetratelabs/wazero/api"
)

// gofunc_fast.go holds the generated zero-reflection adapters for common host
// function signatures. Regenerate it after changing the covered matrix:
//go:generate go run gen_gofunc_fast.go

type paramsKind byte

const (
	paramsKindNoContext paramsKind = iota
	paramsKindContext
	paramsKindContextModule
)

// Below are reflection code to get the interface type used to parse functions and set values.

var (
	moduleType    = reflect.TypeOf((*api.Module)(nil)).Elem()
	goContextType = reflect.TypeOf((*context.Context)(nil)).Elem()
	errorType     = reflect.TypeOf((*error)(nil)).Elem()
)

// compile-time check to ensure reflectGoModuleFunction implements
// api.GoModuleFunction.
var _ api.GoModuleFunction = (*reflectGoModuleFunction)(nil)

type reflectGoModuleFunction struct {
	fn                      *reflect.Value
	paramTypes              []reflect.Type
	paramKinds, resultKinds []reflect.Kind
	numIn                   int
	params, results         []ValueType
}

// Call implements the same method as documented on api.GoModuleFunction.
func (f *reflectGoModuleFunction) Call(ctx context.Context, mod api.Module, stack []uint64) {
	callGoFunc(ctx, mod, f.fn, f.numIn, f.paramTypes, f.paramKinds, f.resultKinds, stack)
}

// EqualTo is exposed for testing.
func (f *reflectGoModuleFunction) EqualTo(that interface{}) bool {
	if f2, ok := that.(*reflectGoModuleFunction); !ok {
		return false
	} else {
		// TODO compare reflect pointers
		return slices.Equal(f.params, f2.params) && slices.Equal(f.results, f2.results)
	}
}

// compile-time check to ensure reflectGoFunction implements api.GoFunction.
var _ api.GoFunction = (*reflectGoFunction)(nil)

type reflectGoFunction struct {
	fn                      *reflect.Value
	pk                      paramsKind
	paramTypes              []reflect.Type
	paramKinds, resultKinds []reflect.Kind
	numIn                   int
	params, results         []ValueType
}

// EqualTo is exposed for testing.
func (f *reflectGoFunction) EqualTo(that interface{}) bool {
	if f2, ok := that.(*reflectGoFunction); !ok {
		return false
	} else {
		// TODO compare reflect pointers
		return f.pk == f2.pk &&
			slices.Equal(f.params, f2.params) && slices.Equal(f.results, f2.results)
	}
}

// Call implements the same method as documented on api.GoFunction.
func (f *reflectGoFunction) Call(ctx context.Context, stack []uint64) {
	if f.pk == paramsKindNoContext {
		ctx = nil
	}
	callGoFunc(ctx, nil, f.fn, f.numIn, f.paramTypes, f.paramKinds, f.resultKinds, stack)
}

// callGoFunc executes the reflective function by converting params to Go
// types. The results of the function call are converted back to api.ValueType.
//
// numIn, paramTypes, paramKinds and resultKinds are precomputed at parse
// time (see parseGoReflectFunc) so that this per-call path never needs to
// call fn.Type(), tp.In(i) or tp.NumIn().
func callGoFunc(ctx context.Context, mod api.Module, fn *reflect.Value, numIn int, paramTypes []reflect.Type, paramKinds, resultKinds []reflect.Kind, stack []uint64) {
	var in []reflect.Value
	if numIn != 0 {
		in = make([]reflect.Value, numIn)

		i := 0
		if ctx != nil {
			in[0] = reflect.ValueOf(ctx)
			i++
		}
		if mod != nil {
			in[1] = reflect.ValueOf(mod)
			i++
		}

		for j := 0; i < numIn; i++ {
			val := reflect.New(paramTypes[j]).Elem()
			k := paramKinds[j]
			raw := stack[j]
			j++

			switch k {
			case reflect.Float32:
				val.SetFloat(float64(math.Float32frombits(uint32(raw))))
			case reflect.Float64:
				val.SetFloat(math.Float64frombits(raw))
			case reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				val.SetUint(raw)
			case reflect.Int32, reflect.Int64:
				val.SetInt(int64(raw))
			default:
				panic(fmt.Errorf("BUG: param[%d] has an invalid type: %v", i, k))
			}
			in[i] = val
		}
	}

	// Execute the host function and push back the call result onto the stack.
	for i, ret := range fn.Call(in) {
		switch resultKinds[i] {
		case reflect.Float32:
			stack[i] = uint64(math.Float32bits(float32(ret.Float())))
		case reflect.Float64:
			stack[i] = math.Float64bits(ret.Float())
		case reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			stack[i] = ret.Uint()
		case reflect.Int32, reflect.Int64:
			stack[i] = uint64(ret.Int())
		default:
			panic(fmt.Errorf("BUG: result[%d] has an invalid type: %v", i, resultKinds[i]))
		}
	}
}

// MustParseGoReflectFuncCode parses Code from the go function or panics.
//
// Exposing this simplifies FunctionDefinition of host functions in built-in host
// modules and tests.
func MustParseGoReflectFuncCode(fn interface{}) Code {
	_, _, code, err := parseGoReflectFunc(fn)
	if err != nil {
		panic(err)
	}
	return code
}

func parseGoReflectFunc(fn interface{}) (params, results []ValueType, code Code, err error) {
	fnV := reflect.ValueOf(fn)
	p := fnV.Type()

	if fnV.Kind() != reflect.Func {
		err = fmt.Errorf("kind != func: %s", fnV.Kind().String())
		return
	}

	pk, kindErr := kind(p)
	if kindErr != nil {
		err = kindErr
		return
	}

	pOffset := 0
	switch pk {
	case paramsKindNoContext:
	case paramsKindContext:
		pOffset = 1
	case paramsKindContextModule:
		pOffset = 2
	}

	pCount := p.NumIn() - pOffset
	var paramTypes []reflect.Type
	var paramKinds []reflect.Kind
	if pCount > 0 {
		params = make([]ValueType, pCount)
		paramTypes = make([]reflect.Type, pCount)
		paramKinds = make([]reflect.Kind, pCount)
	}
	for i := 0; i < len(params); i++ {
		pI := p.In(i + pOffset)
		if t, ok := getTypeOf(pI.Kind()); ok {
			params[i] = t
			paramTypes[i] = pI
			paramKinds[i] = pI.Kind()
			continue
		}

		// Now, we will definitely err, decide which message is best
		var arg0Type reflect.Type
		if hc := pI.Implements(moduleType); hc {
			arg0Type = moduleType
		} else if gc := pI.Implements(goContextType); gc {
			arg0Type = goContextType
		}

		if arg0Type != nil {
			err = fmt.Errorf("param[%d] is a %s, which may be defined only once as param[0]", i+pOffset, arg0Type)
		} else {
			err = fmt.Errorf("param[%d] is unsupported: %s", i+pOffset, pI.Kind())
		}
		return
	}

	rCount := p.NumOut()
	var resultKinds []reflect.Kind
	if rCount > 0 {
		results = make([]ValueType, rCount)
		resultKinds = make([]reflect.Kind, rCount)
	}
	for i := 0; i < len(results); i++ {
		rI := p.Out(i)
		if t, ok := getTypeOf(rI.Kind()); ok {
			results[i] = t
			resultKinds[i] = rI.Kind()
			continue
		}

		// Now, we will definitely err, decide which message is best
		if rI.Implements(errorType) {
			err = fmt.Errorf("result[%d] is an error, which is unsupported", i)
		} else {
			err = fmt.Errorf("result[%d] is unsupported: %s", i, rI.Kind())
		}
		return
	}

	code = Code{}
	if gf, ok := fastGoFunc(pk, fn); ok {
		// fn matches one of the common concrete signatures handled without
		// reflection at call time: use that instead of the general path.
		code.GoFunc = gf
	} else if pk == paramsKindContextModule {
		code.GoFunc = &reflectGoModuleFunction{
			fn: &fnV, numIn: p.NumIn(), paramTypes: paramTypes, paramKinds: paramKinds, resultKinds: resultKinds,
			params: params, results: results,
		}
	} else {
		code.GoFunc = &reflectGoFunction{
			pk: pk, fn: &fnV, numIn: p.NumIn(), paramTypes: paramTypes, paramKinds: paramKinds, resultKinds: resultKinds,
			params: params, results: results,
		}
	}
	return
}

func kind(p reflect.Type) (paramsKind, error) {
	pCount := p.NumIn()
	if pCount > 0 && p.In(0).Kind() == reflect.Interface {
		p0 := p.In(0)
		if p0.Implements(moduleType) {
			return 0, errors.New("invalid signature: api.Module parameter must be preceded by context.Context")
		} else if p0.Implements(goContextType) {
			if pCount >= 2 && p.In(1).Implements(moduleType) {
				return paramsKindContextModule, nil
			}
			return paramsKindContext, nil
		}
	}
	// Without context param allows portability with reflective runtimes.
	// This allows people to more easily port to wazero.
	return paramsKindNoContext, nil
}

func getTypeOf(kind reflect.Kind) (ValueType, bool) {
	switch kind {
	case reflect.Float64:
		return ValueTypeF64, true
	case reflect.Float32:
		return ValueTypeF32, true
	case reflect.Int32, reflect.Uint32:
		return ValueTypeI32, true
	case reflect.Int64, reflect.Uint64:
		return ValueTypeI64, true
	case reflect.Uintptr:
		return ValueTypeExternref, true
	default:
		return 0x00, false
	}
}
