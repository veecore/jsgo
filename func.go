package jsgo

import (
	"fmt"
	"reflect"
	"sync"
	"syscall/js"
)

// FuncOfAny wraps Go functions into JS functions with automatic type conversion.
// Supported variadic functions:
//
// Example:
//
//	jsFunc := FuncOfAny(func(x int) int { return x*2 })
//	js.Global().Set("double", jsFunc)
//	defer jsFunc.Release()
//
// # Critical Caveats
//
//  1. Argument modification doesn't affect JS side:
//     func mutate(x *int) { *x++ } // JS side won't see changes
//
// 2. Returned values are copies
// 3. Functions retain Go context - ensure proper cleanup with Release()
func FuncOfAny(f any) js.Func {
	rVal := reflect.ValueOf(f)
	if rVal.Kind() != reflect.Func {
		panic("f is not a function")
	}
	fa, err := newInnerFuncTypeEncoder(rVal.Type())(rVal)
	if err != nil {
		panic(err)
	}
	return js.FuncOf(fa)
}

func FuncOf(f func(this js.Value, jsArgs []js.Value) interface{}) js.Func {
	return js.FuncOf(func(this js.Value, jsArgs []js.Value) interface{} {
		result := f(this, jsArgs)
		if err, ok := result.(error); ok {
			return Throw(err)
		}

		return handleNonErrorSingleReturnA(result)
	})
}

// lazyFunc doesn't reuse "the actual" allocated value.
// Memory use would reduce greatly if it did
// but there seems to be no way to do that without
// cooperation between us and the caller.
type lazyFunc struct {
	args      sync.Pool
	_freeArgs func(args []reflect.Value)
}

func newLazyFunc(f reflect.Type) *lazyFunc {
	numIns := f.NumIn()

	var l lazyFunc

	// once evaluates the args types lazily
	onceArgType := sync.OnceValue(func() []reflect.Type {
		ts := make([]reflect.Type, numIns)
		for i := 0; i < numIns; i++ {
			ts[i] = f.In(i)
		}
		return ts
	})

	// TODO: We can't create this for everyone...
	l.args = sync.Pool{
		New: func() any {
			argsTypes := onceArgType()
			argsValues := make([]reflect.Value, len(argsTypes))
			for i, argType := range argsTypes {
				argsValues[i] = reflect.New(argType).Elem()
			}
			return argsValues
		},
	}

	// SAFETY: We're not deallocating here but decreasing
	// the reference count. So if the value is retained,
	// no use-after-free should occur. That's the GC's work
	l._freeArgs = func(args []reflect.Value) {
		for i := 0; i < len(args); i++ {
			// detect slices and preserve cap? or let it go or reduce to a threshold
			args[i].SetZero()
		}
		l.args.Put(args)
	}

	// return pointer to avoid copy issue
	return &l
}

func (l *lazyFunc) newArgs() []reflect.Value {
	return l.args.Get().([]reflect.Value)
}

func (l *lazyFunc) freeArgs(args []reflect.Value) {
	l._freeArgs(args)
}

func methodAny(rVal reflect.Value) (func(js.Value, []js.Value) any, error) {
	return funcAnyReturnHandler(rVal.Type(), isMethod, prepareFunctionHandler)(rVal)
}

func newInnerFuncTypeEncoder(t reflect.Type) funcInnerEncoderFunc {
	return funcAnyReturnHandler(t, isFunc, prepareFunctionHandler)
}

var isFunc = !isMethod
var isMethod = true

func evaluateReturn(f reflect.Type) []reflect.Type {
	results := make([]reflect.Type, f.NumOut())
	for i := 0; i < f.NumOut(); i++ {
		results[i] = f.Out(i)
	}
	return results
}

func funcAnyReturnHandler(t reflect.Type, isMethod bool, handlerPrepare func(results []reflect.Type) func(this js.Value, out []reflect.Value) js.Value) funcInnerEncoderFunc {
	f := newLazyFunc(t)
	handler := handlerPrepare(evaluateReturn(t))

	isVariadic := t.IsVariadic() // So it won't keep referencing t

	expectedNormalArgs := t.NumIn()
	if isVariadic {
		expectedNormalArgs--
	}
	if isMethod {
		// the receiver is now part of the arguments but we don't see it as one since it's in this
		expectedNormalArgs--
	}

	return funcInnerEncoderFunc(func(rVal reflect.Value) (func(js.Value, []js.Value) any, error) {
		var call func(in []reflect.Value) []reflect.Value
		if isVariadic {
			call = rVal.CallSlice
		} else {
			call = rVal.Call
		}

		return func(this js.Value, jsArgs []js.Value) any {
			if len(jsArgs) < expectedNormalArgs {
				return Throw(fmt.Errorf("too few arguments: expected at least %d", expectedNormalArgs))
			}

			if !isVariadic && len(jsArgs) != expectedNormalArgs {
				return Throw(fmt.Errorf("too many arguments: expected %d", expectedNormalArgs))
			}

			args := f.newArgs()
			defer f.freeArgs(args)
			callI := 0 // if it's method, we have the first as receiver
			if isMethod {
				callI = 1
				if err := unmarshalRval(this, args[0]); err != nil {
					return Throw(fmt.Errorf("unmarshalling method receiver: %w", err))
				}
			}
			for i := 0; i < expectedNormalArgs; i++ {
				if err := unmarshalRval(jsArgs[i], args[i+callI]); err != nil {
					return Throw(fmt.Errorf("argument %d: %w", i, err))
				}
			}

			if variadicArgs := jsArgs[expectedNormalArgs:]; len(variadicArgs) > 0 {
				variadic := args[expectedNormalArgs]
				// Even if lazyFunc joins the dark force, we shouldn't
				variadic.Set(reflect.MakeSlice(variadic.Type(), len(variadicArgs), len(variadicArgs)))
				for i, variadicArg := range variadicArgs {
					if err := unmarshalRval(variadicArg, variadic.Index(i)); err != nil {
						return Throw(fmt.Errorf("argument %d: %w", i, err))
					}
				}
			}

			// Call the Go function
			// Capture the argument's state here and reflect modification
			// on the js values for side-effect functions. But wouldn't this be deceptive?
			// making it seem like the memory is really shared when we marshal back and forth
			var results = call(args)
			return handler(this, results)
		}, nil
	})
}

func prepareFunctionHandler(results []reflect.Type) func(this js.Value, out []reflect.Value) js.Value {
	switch len(results) {
	case 0:
		return func(this js.Value, out []reflect.Value) js.Value { return js.Undefined() }
	case 1:
		return prepareHandleSingleReturn(results[0])
	default:
		return handleMultipleReturnsfunc
	}
}

func prepareHandleSingleReturn(results reflect.Type) func(this js.Value, out []reflect.Value) js.Value {
	if results.Implements(errorType) {
		return handleError
	}

	return handleNonErrorSingleReturn
}

// Special case for error returns
func handleError(this js.Value, out []reflect.Value) js.Value {
	if err := out[0].Interface().(error); err != nil {
		return Throw(err)
	}
	return js.Null()
}

func handleNonErrorSingleReturn(_ js.Value, out []reflect.Value) js.Value {
	jsVal, err := marshalRVal(out[0])
	if err != nil {
		return Throw(fmt.Errorf("failed to marshal return value: %w", err))
	}
	return jsVal
}

func handleNonErrorSingleReturnA(a any) js.Value {
	jsVal, err := Marshal(a)
	if err != nil {
		return Throw(fmt.Errorf("failed to marshal return value: %w", err))
	}
	return jsVal
}

func handleMultipleReturnsfunc(_ js.Value, results []reflect.Value) js.Value {
	// Check if last value is an error
	lastResult := results[len(results)-1]
	if lastResult.Type().Implements(errorType) {
		if !lastResult.IsNil() {
			// Handle partial result here?
			return Throw(lastResult.Interface().(error))
		}
		results = results[:len(results)-1]
	}
	// Marshal all return values into JS array
	jsResults := MakeArrayNoBuffer(len(results))
	for i, result := range results {
		jsVal, err := marshalRVal(result)
		if err != nil {
			return Throw(fmt.Errorf("failed to marshal return value %d: %w", i, err))
		}
		jsResults.SetIndex(i, jsVal)
	}

	return jsResults
}
