package jsgo

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"syscall/js"
)

// TypeFor creates a Go type as a JavaScript class with proper inheritance support.
//
// Parameters:
//   - T: Go type to export (must be struct, array types, or map)
//   - constructor: Either:
//   - nil for default constructor
//   - Constructor function (func(...) (T, [error])) // optional error
//   - ConstructorConfig for advanced control
//
// Returns:
//   - error: Initialization error if any
//   - release: Cleanup function to free JS resources
//   - class: type as a js.Value
//
// Features:
// - Inheritance via embedded struct tags: `jsgo:",extends=ParentClass"`
// - Custom super constructor arguments via ConstructorConfig.SuperArgs
// - Automatic resource management
// - Method export for exported Go methods
//
// Example:
//
//	type MyClass struct {
//	    BaseClass `jsgo:",extends=BaseJSClass"`  // can be js.Value or formerly exported types
//	}
//
//	func NewMyClass(arg string) (*MyClass, error) { ... }
//
//	// Export with custom super args
//	cfg := ConstructorConfig{
//	    Fn: NewMyClass,
//	    SuperArgs: func(args []js.Value) []js.Value {
//	        return []js.Value{js.ValueOf("processed")}
//	    },
//	}
//	class, release, err := TypeFor[MyClass](cfg)
func TypeFor[T any](constructor any) (Class, error) {
	return typeFor(reflect.TypeFor[T](), constructor)
}

type Class struct {
	Constructor js.Func
	Methods     map[string]js.Func
}

func (c Class) Release() {
	for _, m := range c.Methods {
		m.Release()
	}
	c.Constructor.Release()
}

// ConstructorConfig defines configuration options for class constructors
type ConstructorConfig struct {
	// Constructor function that returns an instance of the type
	Fn interface{}

	// SuperArgs transforms arguments before passing to parent constructor.
	// If nil, all arguments are passed through.
	SuperArgs func([]js.Value) []js.Value
}

func ExportGoType[T any](constructor any) (release func(), err error) {
	_type := reflect.TypeFor[T]()
	return exportGoType(_type, constructor, _type.Name())
}

func ExportGoTypeWithName[T any](constructor any, name string) (release func(), err error) {
	return exportGoType(reflect.TypeFor[T](), constructor, name)
}

func exportGoType(_type reflect.Type, constructor any, name string) (release func(), err error) {
	// Validate class name
	if name == "" {
		return nil, fmt.Errorf("type name cannot be empty")
	}
	class, err := typeFor(_type, constructor)
	if err != nil {
		return nil, err
	}

	class.Constructor.Set("name", name)
	// Register global class
	js.Global().Set(name, class.Constructor)
	return func() {
		class.Release()
		js.Global().Delete(name)
	}, nil
}

func typeFor(_type reflect.Type, constructor any) (class Class, err error) {
	// Cleanup on error
	defer func() {
		if err != nil {
			class.Release()
		}
	}()

	// Validate type
	if !canBeClass(_type) {
		return Class{}, fmt.Errorf("type %v can't be a js class; expected one of %v",
			_type, "slice, array, map, or struct")
	}

	// Process constructor configuration
	var superArgsFunc func([]js.Value) []js.Value
	var cons reflect.Value

	switch cfg := constructor.(type) {
	case ConstructorConfig:
		cons = reflect.ValueOf(cfg.Fn)
		superArgsFunc = cfg.SuperArgs
	default:
		cons = reflect.ValueOf(constructor)
	}
	// Find parent class through embedded fields
	var parentCtor js.Value
	if extendsTag := getExtendsTag(_type); extendsTag != "" {
		parentCtor = js.Global().Get(extendsTag)
		if !parentCtor.Truthy() {
			return Class{}, fmt.Errorf("parent class %q not found", extendsTag)
		}
	}

	// Configure constructor wrapper
	var jsCtorFunc func(js.Value, []js.Value) any

	if constructor != nil {
		// Validate constructor signature
		if cons.Kind() != reflect.Func {
			return Class{}, fmt.Errorf("constructor is not a function")
		}

		conSignature := cons.Type()
		if conSignature.NumOut() < 1 || conSignature.NumOut() > 2 {
			return Class{}, fmt.Errorf("constructor must return (T) or (T, error)")
		}

		if !isOrPtr(conSignature.Out(0), _type, 2) {
			return Class{}, fmt.Errorf("constructor first return must be %v, got %v",
				_type, conSignature.Out(0))
		}

		if conSignature.NumOut() == 2 && conSignature.Out(1) != errorType {
			return Class{}, fmt.Errorf("second return must be error, got %v",
				conSignature.Out(1))
		}

		// Create JS-accessible constructor
		var err error
		jsCtorFunc, err = funcAnyReturnHandler(cons.Type(), isFunc, prepareConstructorHandler)(cons)
		if err != nil {
			return Class{}, fmt.Errorf("constructor handler failed: %w", err)
		}
	} else {
		// Default constructor
		jsCtorFunc = func(this js.Value, args []js.Value) any {
			return this
		}
	}

	// Wrap constructor with inheritance logic
	if parentCtor.Truthy() {
		originalCtor := jsCtorFunc
		jsCtorFunc = func(this js.Value, args []js.Value) any {
			// Call parent constructor with processed args
			superArgs := args
			if superArgsFunc != nil {
				superArgs = superArgsFunc(args)
			}

			argsArray := make([]any, len(superArgs)) // Find ways to avoid this new alloc
			// annoying type system
			for i, arg := range superArgs {
				argsArray[i] = arg
			}
			baseSelf := parentCtor.New(argsArray...)
			return originalCtor(baseSelf, args)
		}
	}

	var jsCtorProto js.Value // We need it before it is
	jsCtorFuncOld := jsCtorFunc
	jsCtorFunc = func(this js.Value, args []js.Value) any {
		if err := enforceClass(this, jsCtorProto); err.Truthy() {
			return err
		}
		return jsCtorFuncOld(this, args)
	}

	// Create JS constructor function
	jsCtor := js.FuncOf(jsCtorFunc)
	class.Constructor = jsCtor
	class.Methods = make(map[string]js.Func)

	// Set up prototype chain
	if parentCtor.Truthy() {
		// Create proper prototype chain
		parentProto := parentCtor.Get("prototype")
		childProto := jsObject.Call("create", parentProto)
		childProto.Set("constructor", jsCtor)
		jsCtor.Set("prototype", childProto)
	}

	jsCtorProto = jsCtor.Get("prototype")

	// Export methods
	for i := 0; i < _type.NumMethod(); i++ {
		method := _type.Method(i)
		if !method.IsExported() {
			continue
		}

		// Convert Go method to JS function
		m, err := methodAny(method.Func)
		if err != nil {
			return Class{}, fmt.Errorf("method %q: %w", method.Name, err)
		}

		jsMethod := js.FuncOf(m)
		jsCtorProto.Set(method.Name, jsMethod)
		class.Methods[method.Name] = jsMethod
	}

	return
}

var errClassNewConstruct = errors.New("Cannot call a class constructor without new")

func enforceClass(this, targetClassProto js.Value) (err js.Value) {
	if !PrototypeOf(this).Equal(targetClassProto) {
		return Throw(errClassNewConstruct)
	}
	return js.Null()
}

func prepareConstructorHandler(results []reflect.Type) func(this js.Value, out []reflect.Value) js.Value {
	switch len(results) {
	case 1:
		return handleTConstructor
	case 2:
		return handleTEConstructor
	default:
		panic(fmt.Sprintf("invalid constructor returns %d", len(results)))
	}
}

func handleTConstructor(this js.Value, results []reflect.Value) js.Value {
	if err := marshalIntoRVal(this, results[0]); err != nil {
		return Throw(fmt.Errorf("failed to marshal return value: %w", err))
	}
	return this
}

func handleTEConstructor(this js.Value, results []reflect.Value) js.Value {
	if !results[1].IsNil() {
		if err := results[1].Interface().(error); err != nil {
			return Throw(err)
		}
	}

	return handleTConstructor(this, results)
}

func canBeClass(t reflect.Type) bool {
	for {
		switch t.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map, reflect.Struct:
			return !containsChan(t)
		case reflect.Pointer:
			t = t.Elem()
		default:
			return false
		}
	}
}

func containsChan(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Chan:
		return true
	case reflect.Slice, reflect.Array:
		return containsChan(t.Elem())
	case reflect.Map:
		return containsChan(t.Key()) || containsChan(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if containsChan(t.Field(i).Type) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func isOrPtr(t, to reflect.Type, depth int) bool {
	if depth == 0 {
		return false
	}
	if t.Kind() == reflect.Pointer {
		return isOrPtr(t.Elem(), to, depth-1)
	}
	return t == to
}

// New creates JavaScript objects from their global constructor names.
// Equivalent to `new ConstructorName(...args)` in JavaScript.
//
// Example:
//
//	array := jsgo.New("Array", 10) // Creates new Array(10)
func New(name string, constructor ...any) js.Value {
	return Type(name).New(constructor...)
}

func Type(name string) js.Value {
	return js.Global().Get(name)
}

var (
	uint8Array           = Type("Uint8Array")
	jsArray              = Type("Array")
	jsArrayBuffer        = Type("ArrayBuffer")
	jsObject             = Type("Object")
	jsDate               = Type("Date")
	jsBigInt             = Type("BigInt")
	arrayProto           = jsArray.Get("prototype")
	objectPrototypeOf    = jsObject.Get("getPrototypeOf")
	objectSetPrototypeOf = jsObject.Get("setPrototypeOf")
	ObjectKeysFn         = jsObject.Get("keys")
)

func NewObject() js.Value {
	return jsObject.New()
}

// IsArray checks if a JS value is an array
// Equivalent to Array.isArray() in JavaScript
func IsArray(v js.Value) bool {
	// return v.InstanceOf(jsArray) || v.InstanceOf(uint8Array)
	return jsArray.Call("isArray", v).Bool()
}

func MakeArray(len int, cap int) js.Value {
	if cap < len {
		panic("cap < len")
	}
	// Create underlying storage with capacity
	buf := jsArrayBuffer.New(cap)
	// Create view with specified length
	return jsArray.New(buf, 0, len)
}

func MakeArrayNoBuffer(len int) js.Value {
	return jsArray.New(len)
}

// MakeBytes creates a byte buffer with both length and capacity
// Underlying storage uses ArrayBuffer for optimal memory layout
//
// Example:
//
//	buf := MakeBytes(1024, 4096) // 1KB view, 4KB capacity
func MakeBytes(len int, cap int) js.Value {
	if cap < len {
		panic("cap < len")
	}
	// Create underlying storage with capacity
	buf := jsArrayBuffer.New(cap)
	// Create view with specified length
	return uint8Array.New(buf, 0, len)
}

func MakeBytesNoBuffer(len int) js.Value {
	return uint8Array.New(len)
}

func BytesN(b []byte) (js.Value, int) {
	dst := uint8Array.New(len(b))
	return dst, js.CopyBytesToJS(dst, b)
}

// Bytes efficiently converts Go byte slices to JavaScript Uint8Array.
// Panics if the copy is incomplete to prevent subtle data bugs.
//
// For partial copies, use BytesN and check the returned count.
func Bytes(b []byte) js.Value {
	dst, n := BytesN(b)
	if n != len(b) {
		panic(fmt.Sprintf("incomplete bytes copy. %d of %d", n, len(b)))
	}
	return dst
}

// objectKeysGoField returns the enumerable own properties of a JS object.
// More efficient than manual iteration in Go-WASM context.
// Note: Splitting by commas is unsafe in general but may be acceptable
// if keys are guaranteed not to contain commas (e.g., struct field names).
func objectKeysGoField(v js.Value) []string {
	keysObj := ObjectKeysFn.Invoke(v)        // 1 syscall
	str := keysObj.Call("toString").String() // 2 syscall
	// This is safe because go fields cannot have ","
	return strings.Split(str, ",")
}

func ObjectKeys(v js.Value) []string {
	fs := ObjectKeysFn.Invoke(v)
	keys := make([]string, fs.Length())
	for i := 0; i < len(keys); i++ { // len(keys) syscalls
		keys[i] = fs.Index(i).String()
	}
	return keys
}

var errorType = reflect.TypeFor[error]()

func IsObject(v js.Value) bool {
	return v.Type() == js.TypeObject || v.Type() == js.TypeFunction
}

func PrototypeOf(obj js.Value) js.Value {
	if obj.IsUndefined() || obj.IsNull() {
		return js.Undefined()
	}
	return objectPrototypeOf.Invoke(obj)
}

func CopyObject(dst, src js.Value) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// We're not checking js.ValueError because
			// we've called the right method
			if jerr, ok := r.(*js.Error); ok {
				err = jerr
			} else {
				panic(r)
			}
		}
	}()
	jsObject.Call("assign", dst, src)
	return nil
}

func CopyObjectWithMethods(dst, src js.Value) (err error) {
	panic("CopyObjectWithMethods unimplemented")
}

func TypeJSParallel[T any]() js.Type {
	return typeJSParallel(reflect.TypeFor[T]())
}

func typeJSParallel(t reflect.Type) js.Type {
	for {
		switch t.Kind() {
		case reflect.Array, reflect.Struct, reflect.Slice, reflect.Map:
			return js.TypeObject
		case reflect.Func:
			return js.TypeFunction
		case reflect.Bool:
			return js.TypeBoolean
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.Int, reflect.Int8, reflect.Int16,
			reflect.Int32, reflect.Int64, reflect.UnsafePointer:
			return js.TypeNumber
		case reflect.String:
			return js.TypeString
		case reflect.Pointer:
			t = t.Elem() // let's take a break from recursion
		case reflect.Interface:
			// is this the right way?
			return js.TypeUndefined
		default:
			return js.TypeUndefined
		}
	}
}

func typeJSParallelNoIndirect(t reflect.Type) js.Type {
	switch t.Kind() {
	case reflect.Array, reflect.Struct, reflect.Slice, reflect.Map:
		return js.TypeObject
	case reflect.Func:
		return js.TypeFunction
	case reflect.Bool:
		return js.TypeBoolean
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64, reflect.UnsafePointer:
		return js.TypeNumber
	case reflect.String:
		return js.TypeString
	case reflect.Interface:
		// is this the right way?
		return js.TypeUndefined
	default:
		return js.TypeUndefined
	}
}

func defaultJsValueFor(t reflect.Type) js.Value {
	switch t.Kind() {
	case reflect.Array, reflect.Struct, reflect.Slice, reflect.Map, reflect.Func:
		return js.Null()
	case reflect.Bool:
		return js.ValueOf(false)
	case reflect.String:
		return js.ValueOf("")
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64, reflect.UnsafePointer:
		return js.ValueOf(0)
	default:
		return js.Undefined()
	}
}

func makeNull(v js.Value) error {
	if v.IsUndefined() {
		return fmt.Errorf("cannot set prototype on undefined value")
	}

	if v.IsNull() {
		return nil
	}

	// Set prototype of v to null
	objectSetPrototypeOf.Invoke(v, js.Null())
	return nil
}

var (
	errorConstructor = Type("Error")
)

// Throw converts Go errors to JS exceptions. Use in function callbacks
// to propagate errors properly to JS.
//
// Example:
//
//	js.FuncOf(func(this js.Value, args []js.Value) any {
//	    result, err := riskyOperation()
//	    if err != nil {
//	        return Throw(err)
//	    }
//	    return result
//	})
func Throw(err error) js.Value {
	// goStack := debug.Stack()
	// message := fmt.Sprintf("%s\n\nGo Stack Trace:\n%s", err.Error(), string(goStack))
	return errorConstructor.New(err.Error())
}
