package jsgo

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"syscall/js"
)

// Marshaler is the interface implemented by types that can convert themselves
// to JavaScript values. Use for custom serialization logic.
//
// Example:
//
//	type CustomID string
//	func (id CustomID) MarshalJS() (js.Value, error) {
//	    return js.ValueOf("ID:"+string(id)), nil
//	}
type Marshaler interface {
	MarshalJS() (js.Value, error)
}

// MarshalerInto allows certain types(types have jsgo.TypeJSParallel[T]() == js.TypeObject)
// to marshal into existing JS values.
// Implementing this interface provides both MarshalJS and MarshalIntoJS capabilities.
//
// Example:
//
//	type MySophisticatedType struct {
//			myPrivateField int
//	}
//
//	func (m MySophisticatedType) MarshalIntoJS(v js.Value) error {
//			v.Set("exposedField", m.myPrivateField)
//			return nil
//	}
type MarshalerInto interface {
	MarshalIntoJS(js.Value) error
}

// Marshal converts Go values to JavaScript values with special handling for:
// - time.Time → JS Date
// - []byte → Uint8Array
// - big.Int → JS BigInt
// - Functions → JS callbacks
//
// Example:
//
//	type User struct {
//	    Name string `jsgo:"username"`
//	}
//	u := &User{Name: "Alice"}
//	jsVal, err := Marshal(u) // Returns JS object {username: "Alice"}
//
// Gotchas:
// - Cyclic structures return error
// - Channels are not supported
func Marshal(goVal any) (js.Value, error) {
	ctx := marshalContextPool.Get().(*contextM)
	defer func() {
		ctx.ptrLevel = 0
		clear(ctx.seen)
		marshalContextPool.Put(ctx)
	}()
	rVal := reflect.ValueOf(goVal)
	v, err := getTypeEncoder(rVal.Type())(ctx, rVal)
	if err != nil {
		return js.Value{}, fmt.Errorf("jsgo: %w", err)
	}
	return v, nil
}

// MarshalInto is like Marshal buf jsVal has to be a reference type (Object)

// MarshalInto marshals a Go value into an existing JavaScript reference type
// (Object). More efficient than Marshal for repeated operations
// as it reuses JS containers instead of creating new ones.
//
// Parameters:
//   - jsVal: Must be a JS reference type (Object, Array, TypedArray)
//   - goVal: Go value to marshal (struct, map, slice, etc)
//
// Returns:
//   - error if marshaling fails (cyclic ref, unsupported type, etc)
//
// Example:
//
//	jsObj := js.Global().Get("Object").New()
//	err := MarshalInto(jsObj, myStruct)
//	jsObj.Call("someMethod")
//
// Notes:
//   - Prefer for hot paths where JS object reuse matters
//   - Does not clear existing properties
func MarshalInto(jsVal js.Value, goVal any) error {
	ctx := marshalContextPool.Get().(*contextM)
	defer func() {
		ctx.ptrLevel = 0
		clear(ctx.seen)
		marshalContextPool.Put(ctx)
	}()
	rVal := reflect.ValueOf(goVal)
	if err := getTypeEncoderInto(rVal.Type())(ctx, jsVal, rVal); err != nil {
		return fmt.Errorf("jsgo: %w", err)
	}
	return nil
}

var marshalContextPool = sync.Pool{
	New: func() any {
		return &contextM{
			ptrLevel: 0,
			seen:     make(map[uintptr]struct{}),
		}
	},
}

// NOTE: We don't store pointer but the base type. (so as to avoid storing all variations)
// NOTE: We don't store specific marhaler and marshalerInto
// NOTE: Since nothing helps us enforce the kind of object that should be filled in
// a field, if we get and don't find anything, we just create according to discretion
// based on the go type(array, uint8array, or object)
// if we decide to enforce type on the js side (e.g by using struct tags to tell the
// js equivalent of the type), we will also have to deal with handling the arguments to
// pass to the constructors of these types. Soo...

var encoderFuncCache sync.Map // map[reflect.Type]{ encoderFunc || encodeIntoFunc || funcInnerEncoderFunc}

type contextM struct {
	ptrLevel uint
	seen     map[uintptr]struct{}
}

type encoderFunc func(*contextM, reflect.Value) (js.Value, error)
type encodeIntoFunc func(*contextM, js.Value, reflect.Value) error
type funcInnerEncoderFunc func(rVal reflect.Value) (func(js.Value, []js.Value) any, error)

func (ei encodeIntoFunc) intoEncode(t reflect.Type) encoderFunc {
	create := dynamicObjectTypeCreator(t)
	defaultJsValue := defaultJsValueFor(t)
	return encoderFunc(func(ctx *contextM, rVal reflect.Value) (js.Value, error) {
		instance := create(rVal)
		if err := ei(ctx, instance, rVal); err != nil {
			return defaultJsValue, err
		}
		return instance, nil
	})
}

func (f funcInnerEncoderFunc) intoEncode() encoderFunc {
	return encoderFunc(func(ctx *contextM, rVal reflect.Value) (js.Value, error) {
		inner, err := f(rVal)
		if err != nil {
			return js.Null(), err
		}
		// Find a way to pass this up
		return js.FuncOf(inner).Value, nil
	})
}

var marshalerType = reflect.TypeFor[Marshaler]()
var marshalerIntoType = reflect.TypeFor[MarshalerInto]()

func getTypeEncoder(t reflect.Type) (enc encoderFunc) {
	ptr_depth := 0
	defer func() {
		if ptr_depth > 0 {
			enc = indirectPointerEnc(enc, ptr_depth)
		}
	}()
	for {
		cached, ok := encoderFuncCache.Load(t)
		if ok {
			// we're the entry point for both of us
			switch enci := cached.(type) {
			case encodeIntoFunc:
				return enci.intoEncode(t)
			case encoderFunc:
				return enci
			case funcInnerEncoderFunc:
				return enci.intoEncode()
			default:
				panic(fmt.Sprintf("%T", enc))
			}
			return
		}

		// Handle js.Value directly without processing
		if t == jsValueType {
			return jsValueEnc
		}
		if t == jsFuncType {
			return jsFuncEnc
		}

		if t.Implements(marshalerType) {
			return marshalerEncoder
		}

		// We could have a valid marshalerInto here which we could
		// convert to a encoderFunc
		// we use typeJSParallelNoIndirect so it doesn't follow the pointer
		if t.Implements(marshalerIntoType) && typeJSParallelNoIndirect(t) == js.TypeObject {
			return encodeIntoFunc(marshalerIntoEncoder).intoEncode(t)
		}

		if marshaler, ok := isCanonMarshal(t); ok {
			return marshaler
		}

		switch t.Kind() {
		case reflect.Func:
			// We cache the inner func so we can create this as needed
			// this wouldn't be possible otherwise
			return getFuncTypeEncoder(t)
		case reflect.Pointer:
			ptr_depth++
			t = t.Elem()
			continue
		case reflect.Interface:
			return interfaceEncoder
		}

		// not our work...
		if jsT := typeJSParallel(t); jsT == js.TypeObject {
			return getTypeEncoderInto(t).intoEncode(t)
		}

		// To prevent redundant work during race
		// we fill it with something empty to
		// signify we're working on it so 2 goroutines don't work on
		// the same type at once
		var (
			wg sync.WaitGroup
		)
		wg.Add(1)
		fi, loaded := encoderFuncCache.LoadOrStore(t, encoderFunc(func(ctx *contextM, r reflect.Value) (js.Value, error) {
			// if r := recover(); r != nil {
			//    encoderFuncCache.Delete(t)
			//    wg.Done()
			//    panic(r)
			// }
			wg.Wait()
			return enc(ctx, r)
		}))
		if loaded {
			return fi.(encoderFunc)
		}

		// Compute the real encoder and replace the indirect func with it.
		enc = newPrimitiveTypeEncoder(t)
		// This is the default behaviour in json so.
		if reflect.PointerTo(t).Implements(marshalerType) {
			oldEnc := enc
			enc = encoderFunc(func(ctx *contextM, rVal reflect.Value) (js.Value, error) {
				if rVal.CanAddr() {
					return marshalerEncoder(ctx, rVal.Addr())
				}
				return oldEnc(ctx, rVal)
			})
		}
		wg.Done()
		encoderFuncCache.Store(t, enc)
		return
	}
}

func getTypeEncoderInto(t reflect.Type) (encInto encodeIntoFunc) {
	ptr_depth := 0
	defer func() {
		if ptr_depth > 0 {
			encInto = indirectPointerEncInto(encInto, ptr_depth)
		}
	}()
	for {
		cached, ok := encoderFuncCache.Load(t)
		if ok {
			encInto = cached.(encodeIntoFunc)
			return
		}

		// Handle js.Value directly without processing
		if t == jsValueType {
			encInto = jsValueEncInto
			return
		}

		// We don't regard inappropriate implementations
		if t.Implements(marshalerIntoType) && typeJSParallel(t) == js.TypeObject {
			encInto = marshalerIntoEncoder
			return
		}

		if marshalerInto, ok := isCanonMarshalInto(t); ok {
			encInto = marshalerInto
			return
		}

		switch t.Kind() {
		case reflect.Pointer:
			ptr_depth++
			t = t.Elem()
			continue
		case reflect.Interface:
			encInto = interfaceEncoderInto
			return
		}

		// Now is time to conclude
		if jsT := typeJSParallel(t); jsT != js.TypeObject {
			return nonObjectMarshalInto
		}

		// To deal with recursive types, populate the map with an
		// indirect func before we build it. This type waits on the
		// real func (f) to be ready and then calls it. This indirect
		// func is only used for recursive types.
		var (
			wg sync.WaitGroup
		)
		wg.Add(1)
		fi, loaded := encoderFuncCache.LoadOrStore(t, encodeIntoFunc(func(ctx *contextM, jsVal js.Value, r reflect.Value) error {
			wg.Wait()
			return encInto(ctx, jsVal, r)
		}))
		if loaded {
			encInto = fi.(encodeIntoFunc)
			return
		}

		// Compute the real encoder and replace the indirect func with it.
		encInto = newReferenceTypeEncoder(t)
		// This is the default behaviour in json so.
		if reflect.PointerTo(t).Implements(marshalerIntoType) {
			oldEncInto := encInto
			encInto = encodeIntoFunc(func(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
				if rVal.CanAddr() {
					return marshalerIntoEncoder(ctx, jsVal, rVal.Addr())
				}
				return oldEncInto(ctx, jsVal, rVal)
			})
		}
		wg.Done()
		encoderFuncCache.Store(t, encInto)
		return
	}
}
func marshalerEncoder(_ *contextM, rVal reflect.Value) (js.Value, error) {
	// They might have a special handling for nil
	if rVal.Kind() == reflect.Pointer && rVal.IsNil() {
		return defaultJsValueFor(rVal.Type()), nil
	}
	return rVal.Interface().(Marshaler).MarshalJS()
}

func jsValueEnc(ctx *contextM, rVal reflect.Value) (js.Value, error) {
	return rVal.Interface().(js.Value), nil
}

func jsValueEncInto(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
	if jsVal.Type() != js.TypeObject {
		return fmt.Errorf("cannot MarshalInto non-reference js type: %v", jsVal.Type())
	}
	return CopyObject(jsVal, rVal.Interface().(js.Value))
}

func jsFuncEnc(ctx *contextM, rVal reflect.Value) (js.Value, error) {
	return rVal.Interface().(js.Func).Value, nil
}

func marshalerIntoEncoder(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
	// They might have a special handling for nil
	if rVal.Kind() == reflect.Pointer && rVal.IsNil() {
		return makeNull(jsVal)
	}
	return rVal.Interface().(MarshalerInto).MarshalIntoJS(jsVal)
}

// copied from json and modified
func getInnerFuncTypeEncoder(t reflect.Type) funcInnerEncoderFunc {
	if cached, ok := encoderFuncCache.Load(t); ok {
		return cached.(funcInnerEncoderFunc)
	}

	// To deal with recursive types, populate the map with an
	// indirect func before we build it. This type waits on the
	// real func (f) to be ready and then calls it. This indirect
	// func is only used for recursive types.
	var (
		wg sync.WaitGroup
		f  funcInnerEncoderFunc
	)
	wg.Add(1)
	fi, loaded := encoderFuncCache.LoadOrStore(t, funcInnerEncoderFunc(func(r reflect.Value) (func(js.Value, []js.Value) any, error) {
		wg.Wait()
		return f(r)
	}))
	if loaded {
		return fi.(funcInnerEncoderFunc)
	}

	// Compute the real encoder and replace the indirect func with it.
	f = newInnerFuncTypeEncoder(t)
	wg.Done()
	encoderFuncCache.Store(t, f)
	return f
}

func getFuncTypeEncoder(t reflect.Type) encoderFunc {
	innerEnc := getInnerFuncTypeEncoder(t)
	return innerEnc.intoEncode()
}

func newPrimitiveTypeEncoder(t reflect.Type) encoderFunc {
	switch t.Kind() {
	case reflect.String:
		return stringEncoder
	case reflect.Bool:
		return boolEncoder
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return intEncoder
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uintptr:
		return uintEncoder
	case reflect.Uint64:
		return uint64Encoder
	case reflect.Float32, reflect.Float64:
		return floatEncoder
	default:
		return unsupportedTypeEncoder
	}
}

func newReferenceTypeEncoder(t reflect.Type) encodeIntoFunc {
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return prepareSliceEncoderInto(t)
	case reflect.Map:
		return prepareMapEncoderInto(t)
	case reflect.Struct:
		return prepareStructEncoderInto(t)
	default:
		// NOTE: Since we're not caching pointers and interfaces, we won't handle here
		// so it doesn't get cached. get will evaluate and return as needed
		return nonObjectMarshalInto
	}
}

func unsupportedTypeEncoderInto(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
	return fmt.Errorf("cannot MarshalInto js value from non \"Object\" go type: %v", rVal.Type())
}

func unsupportedTypeEncoder(_ *contextM, rVal reflect.Value) (js.Value, error) {
	return js.Undefined(), fmt.Errorf("unsupported type %s", rVal.Type())
}

func nonObjectMarshalInto(_ *contextM, _ js.Value, rVal reflect.Value) error {
	return fmt.Errorf("cannot MarshalInto js value from non \"Object\" go type: %v", rVal.Type())
}

func interfaceEncoder(ctx *contextM, rVal reflect.Value) (js.Value, error) {
	return getTypeEncoder(rVal.Elem().Type())(ctx, rVal)
}

func interfaceEncoderInto(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
	if jsT := typeJSParallel(rVal.Elem().Type()); jsT == js.TypeObject {
		return getTypeEncoderInto(rVal.Elem().Type())(ctx, jsVal, rVal.Elem())
	}

	return fmt.Errorf("cannot MarshalInto js value from non \"Object\" go type: %v", rVal.Type())
}

func prepareStructEncoderInto(t reflect.Type) encodeIntoFunc {
	fields := getCachedFields(t)
	fieldsEnc := make(map[*field]struct {
		enc           any
		getOrSetField getterOrSetter
	}, len(fields.normalFields)) // to encoderFunc or encoderFuncInto
	for _, f := range fields.normalFields {
		if fieldJsT := typeJSParallel(f._type); fieldJsT == js.TypeObject {
			fieldsEnc[f] = struct {
				enc           any
				getOrSetField getterOrSetter
			}{
				getTypeEncoderInto(f._type), dynamicObjectTypeGetterSetter(f._type),
			}
			continue
		}
		fieldsEnc[f] = struct {
			enc           any
			getOrSetField getterOrSetter
		}{
			getTypeEncoder(f._type), nil,
		}
	}
	embeddedJsValueField := fields.embeddedJsValueField
	return encodeIntoFunc(func(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
		if jsVal.Type() != js.TypeObject {
			return fmt.Errorf("cannot MarshalInto non-reference js type: %v", jsVal.Type())
		}
		// inherited fields have lower precedence
		// Check that it's an object
		if embeddedJsValueField != nil {
			var embeddedJsValue js.Value
			if embeddedJsValueField._type.Kind() == reflect.Pointer {
				embeddedJsValuePtr := rVal.
					Field(embeddedJsValueField.index[0]).Interface().(*js.Value)
				if embeddedJsValuePtr != nil {
					embeddedJsValue = *embeddedJsValuePtr
				}
			} else {
				embeddedJsValue = rVal.
					Field(embeddedJsValueField.index[0]).Interface().(js.Value)
			}

			if embeddedJsValue.Type() == js.TypeObject {
				if err := CopyObject(jsVal, embeddedJsValue); err != nil {
					return err
				}
			}
		}
		for field, encFuncGetOrSet := range fieldsEnc {
			// We're using the Err variant because we don't want panic
			// on nil pointer, we can just omit if omitempty or set the zero value
			fieldValue, err := rVal.FieldByIndexErr(field.index)
			if err != nil {
				if field.tag.omitempty {
					continue
				}
				fieldValue = reflect.Zero(field._type)
			}

			if fieldValue.IsZero() && field.tag.omitempty {
				continue
			}

			var jsFieldValue js.Value

			switch enc := encFuncGetOrSet.enc.(type) {
			case encoderFunc:
				jsFieldValue, err = enc(ctx, fieldValue)
				if err == nil {
					jsVal.Set(field.tag.name, jsFieldValue)
				}
			case encodeIntoFunc:
				jsFieldValue, _ = encFuncGetOrSet.getOrSetField(jsVal, field.tag.name, fieldValue)
				err = enc(ctx, jsFieldValue, fieldValue)
			default:
				panic(fmt.Sprintf("unknown encode got: %T, %v", enc, enc))
			}
			if err != nil {
				return fmt.Errorf("%s: %w", field.tag.name, err)
			}
		}
		return nil
	})
}

var textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()

func prepareMapEncoderInto(t reflect.Type) encodeIntoFunc {
	var keyEnc func(reflect.Value) (string, error)

	// TODO: We can't set bigInt as js key. So how do we handle big number
	switch t.Key().Kind() {
	case reflect.String:
		keyEnc = stringKeyEnc
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		keyEnc = intKeyEnc
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		keyEnc = uintKeyEnc
	default:
		if t.Key().Implements(textMarshalerType) {
			keyEnc = textMlKeyEnc
		}
		keyEnc = anyKeyEnc // We don't error because we haven't seen all keys yet
	}

	if jsT := typeJSParallel(t.Elem()); jsT == js.TypeObject {
		valueEncInto := getTypeEncoderInto(t.Elem())
		getOrSetKey := dynamicObjectTypeGetterSetter(t.Elem())
		return encodeIntoFunc(func(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
			if jsVal.Type() != js.TypeObject {
				return fmt.Errorf("cannot MarshalInto non-reference js type: %v", jsVal.Type())
			}
			if rVal.IsNil() {
				return makeNull(jsVal)
			}
			if ctx.ptrLevel++; ctx.ptrLevel > startDetectingCyclesAfter {
				ptr := rVal.Pointer()
				if ptr == 0 {
					return makeNull(jsVal)
				}
				if _, exists := ctx.seen[ptr]; exists {
					return fmt.Errorf("cyclic reference detected via %s", rVal.Type())
				}
				ctx.seen[ptr] = struct{}{}
				defer delete(ctx.seen, ptr)
			}

			iter := rVal.MapRange()
			for iter.Next() {
				key := iter.Key()
				value := iter.Value()

				// Fast path for string keys
				strKey, err := keyEnc(key)
				if err != nil {
					return err
				}

				jsValue, _ := getOrSetKey(jsVal, strKey, value)
				if err := valueEncInto(ctx, jsValue, value); err != nil {
					return fmt.Errorf("map[%s]: %w", strKey, err)
				}
			}
			return nil
		})
	}
	valueEnc := getTypeEncoder(t.Elem())
	return encodeIntoFunc(func(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
		if jsVal.Type() != js.TypeObject {
			return fmt.Errorf("cannot MarshalInto non-reference js type: %v", jsVal.Type())
		}
		iter := rVal.MapRange()

		for iter.Next() {
			key := iter.Key()
			value := iter.Value()

			// Fast path for string keys
			strKey, err := keyEnc(key)
			if err != nil {
				return err
			}

			jsValue, err := valueEnc(ctx, value)
			if err != nil {
				return fmt.Errorf("map[%s]: %w", strKey, err)
			}

			// Direct property setting
			jsVal.Set(strKey, jsValue)
		}
		return nil
	})
}

func prepareSliceEncoderInto(t reflect.Type) encodeIntoFunc {
	if t.Elem().Kind() == reflect.Uint8 {
		return bytesEncoderInto
	}
	arrayEncInto := prepareArrayEncoderInto(t)

	return encodeIntoFunc(func(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
		if jsVal.Type() != js.TypeObject {
			return fmt.Errorf("cannot MarshalInto non-reference js type: %v", jsVal.Type())
		}

		if ctx.ptrLevel++; ctx.ptrLevel > startDetectingCyclesAfter {
			ptr := rVal.Pointer()
			if ptr == 0 {
				return makeNull(jsVal)
			}
			if _, exists := ctx.seen[ptr]; exists {
				return fmt.Errorf("cyclic reference detected via %s", rVal.Type())
			}
			ctx.seen[ptr] = struct{}{}
			defer delete(ctx.seen, ptr)
		}

		return arrayEncInto(ctx, jsVal, rVal)
	})

}

func prepareArrayEncoderInto(t reflect.Type) encodeIntoFunc {
	// Maybe we should consider arrays of bytes bytes? We shouldn't? That's what it's called in js
	if jsT := typeJSParallel(t.Elem()); jsT == js.TypeObject {
		elemEncInto := getTypeEncoderInto(t.Elem())
		getOrSetIndex := dynamicObjectTypeGetterSetter(t.Elem())
		return encodeIntoFunc(func(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
			if jsVal.Type() != js.TypeObject {
				return fmt.Errorf("cannot MarshalInto non-reference js type: %v", jsVal.Type())
			}

			length := rVal.Len()
			for i := 0; i < length; i++ {
				goIndexValue := rVal.Index(i)
				item, _ := getOrSetIndex(jsVal, i, goIndexValue)
				if err := elemEncInto(ctx, item, goIndexValue); err != nil {
					return fmt.Errorf("array[%d]: %w", i, err)
				}
			}
			return nil
		})
	}
	elemEnc := getTypeEncoder(t.Elem())
	return encodeIntoFunc(func(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
		if jsVal.Type() != js.TypeObject {
			return fmt.Errorf("cannot MarshalInto non-reference js type: %v", jsVal.Type())
		}
		length := rVal.Len()
		for i := 0; i < length; i++ {
			item, err := elemEnc(ctx, rVal.Index(i))
			if err != nil {
				return fmt.Errorf("array[%d]: %w", i, err)
			}
			// Only if we could reduce syscall here and other composites
			jsVal.SetIndex(i, item)
		}
		return nil
	})
}

func bytesEncoderInto(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
	b := rVal.Bytes()
	i := js.CopyBytesToJS(jsVal, b)
	if i != len(b) {
		panic(fmt.Sprintf("incomplete bytes copy. %d of %d", i, len(b)))
	}
	return nil
}
func stringEncoder(_ *contextM, rVal reflect.Value) (js.Value, error) {
	return js.ValueOf(rVal.String()), nil
}
func boolEncoder(_ *contextM, rVal reflect.Value) (js.Value, error) {
	return js.ValueOf(rVal.Bool()), nil
}

// consider getting these beforehand
// Number.MAX_SAFE_INTEGER
// Number.MIN_SAFE_INTEGER
// Number.MAX_VALUE
// Number.MIN_VALUE
// Number.NEGATIVE_INFINITY
// Number.POSITIVE_INFINITY
// intEncoder handles large int64 values that might need BigInt
func intEncoder(_ *contextM, rVal reflect.Value) (js.Value, error) {
	i := rVal.Int()
	if i > 1<<53-1 || i < -(1<<53) {
		if jsBigInt.Truthy() {
			return jsBigInt.Call("apply", nil, fmt.Sprintf("%d", i)), nil
		}
		return js.ValueOf(0), fmt.Errorf("value %d exceeds safe integer range and BigInt is unavailable", i)
	}
	return js.ValueOf(i), nil
}

// uint64Encoder handles potential precision loss
func uint64Encoder(_ *contextM, rVal reflect.Value) (js.Value, error) {
	u := rVal.Uint()
	if u > 1<<53-1 {
		if jsBigInt.Truthy() {
			return jsBigInt.Call("apply", nil, fmt.Sprintf("%d", u)), nil
		}
		return js.ValueOf(0), fmt.Errorf("value %d exceeds safe integer range and BigInt is unavailable", u)
	}
	return js.ValueOf(float64(u)), nil
}
func uintEncoder(_ *contextM, rVal reflect.Value) (js.Value, error) {
	return js.ValueOf(float64(rVal.Uint())), nil
}
func floatEncoder(_ *contextM, rVal reflect.Value) (js.Value, error) {
	return js.ValueOf(rVal.Float()), nil
}

func indirectPointerEnc(f encoderFunc, ptr_depth int) encoderFunc {
	return encoderFunc(func(ctx *contextM, rVal reflect.Value) (js.Value, error) {
		for i := 0; i < ptr_depth; i++ {
			if ctx.ptrLevel++; ctx.ptrLevel > startDetectingCyclesAfter {
				ptr := rVal.Pointer()
				if ptr == 0 {
					return js.Null(), nil
				}
				if _, exists := ctx.seen[ptr]; exists {
					return js.Null(), fmt.Errorf("cyclic reference detected via %s", rVal.Type())
				}
				ctx.seen[ptr] = struct{}{}
				defer delete(ctx.seen, ptr)
			}
			rVal = rVal.Elem()
		}
		// We may put rVal.IsNil in the loop and ask everytime and fail or use this and fail once
		if !rVal.IsValid() {
			return js.Null(), nil
		}
		return f(ctx, rVal)
	})
}

func indirectPointerEncInto(f encodeIntoFunc, ptr_depth int) encodeIntoFunc {
	return encodeIntoFunc(func(ctx *contextM, jsVal js.Value, rVal reflect.Value) error {
		for i := 0; i < ptr_depth; i++ {
			if ctx.ptrLevel++; ctx.ptrLevel > startDetectingCyclesAfter {
				ptr := rVal.Pointer()
				if ptr == 0 {
					return makeNull(jsVal)
				}
				if _, exists := ctx.seen[ptr]; exists {
					return fmt.Errorf("cyclic reference detected via %s", rVal.Type())
				}
				ctx.seen[ptr] = struct{}{}
				defer delete(ctx.seen, ptr)
			}
			rVal = rVal.Elem()
		}
		// We may put rVal.IsNil in the loop and ask everytime and fail or use this and fail once
		if !rVal.IsValid() {
			return makeNull(jsVal)
		}
		return f(ctx, jsVal, rVal)
	})
}

var stringType = reflect.TypeFor[string]()

func stringKeyEnc(key reflect.Value) (string, error) {
	return key.String(), nil
}

// TODO: Handle overflow
func intKeyEnc(key reflect.Value) (string, error) {
	return strconv.FormatInt(key.Int(), 10), nil
}
func uintKeyEnc(key reflect.Value) (string, error) {
	return strconv.FormatUint(key.Uint(), 10), nil
}
func textMlKeyEnc(key reflect.Value) (string, error) {
	tm := key.Interface().(encoding.TextMarshaler)
	if key.Kind() == reflect.Pointer && key.IsNil() {
		return "", nil
	}
	utf8, err := tm.MarshalText()
	return string(utf8), err
}
func anyKeyEnc(key reflect.Value) (string, error) {
	switch key.Kind() {
	case reflect.String:
		return stringKeyEnc(key)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return intKeyEnc(key)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return uintKeyEnc(key)
	default:
		if key.Type().Implements(textMarshalerType) {
			return textMlKeyEnc(key)
		}
	}
	return "", fmt.Errorf("map key of type %s cannot be converted to string", key.Type())
}

// Because Into guys are strict
func dynamicObjectTypeCreator(t reflect.Type) func(reflect.Value) js.Value {
	switch t.Kind() {
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return bytesCreator
		}
		fallthrough
	case reflect.Array:
		return arrayCreator
	default:
		return objectCreator
	}
}

func bytesCreator(rVal reflect.Value) js.Value {
	return MakeBytesNoBuffer(rVal.Len())
}

func arrayCreator(rVal reflect.Value) js.Value {
	return MakeArrayNoBuffer(rVal.Len())
}

func objectCreator(_ reflect.Value) js.Value {
	return NewObject()
}

type valueType int

// NOTE: This must match the getOrSetFunc body values
const (
	typeObject     valueType = iota // Plain JS object: {}
	typeArray                       // JS Array: new Array()
	typeUint8Array                  // JS Uint8Array: new Uint8Array(length)
	typeDate                        // JS Date: new Date() // We're getting to edit so no need to set the time
)

type getterOrSetter func(obj js.Value, fieldOrIndex any, target reflect.Value) (js.Value, bool)

func dynamicObjectTypeGetterSetter(expectedType reflect.Type) getterOrSetter {
	if expectedType == timeType {
		return timeGetterSetter
	}
	switch expectedType.Kind() {
	case reflect.Slice:
		if expectedType.Elem().Kind() == reflect.Uint8 {
			return bytesGetterSetter
		}
		fallthrough
	case reflect.Array:
		return arrayGetterSetter
	default:
		return objectGetterSetter
	}
}

func bytesGetterSetter(v js.Value, fieldOrIndex any, target reflect.Value) (js.Value, bool) {
	return getOrSet(v, fieldOrIndex, target.Len(), typeUint8Array)
}

func arrayGetterSetter(v js.Value, fieldOrIndex any, target reflect.Value) (js.Value, bool) {
	return getOrSet(v, fieldOrIndex, target.Len(), typeArray)
}

func objectGetterSetter(v js.Value, fieldOrIndex any, _ reflect.Value) (js.Value, bool) {
	return getOrSet(v, fieldOrIndex, 0, typeObject)
}

func timeGetterSetter(v js.Value, fieldOrIndex any, _ reflect.Value) (js.Value, bool) {
	return getOrSet(v, fieldOrIndex, 0, typeDate)
}

var (
	getOrSetFunc js.Value
)

func init() {
	getOrSetFunc = compileJSFunction([]string{"obj", "fieldOrIndex", "type", "len"}, `
		if (Object.prototype.hasOwnProperty.call(obj, fieldOrIndex)) {
			return [obj[fieldOrIndex], true];
		}
		var newVal;
		switch(type) {
			case 0: newVal = {}; break;
			case 1: newVal = new Array(len); break;
			case 2: newVal = new Uint8Array(len); break;
			case 3: newVal = new Date(); break;
		}
		obj[fieldOrIndex] = newVal;
		return [newVal, false];
	`)
}

func compileJSFunction(args []string, body string) js.Value {
	argss := make([]any, 0, len(args)+1)
	for _, arg := range args {
		argss = append(argss, any(arg))
	}
	argss = append(argss, any(body))

	return js.Global().Get("Function").New(argss...)
}

// getOrSet checks/sets a field with ONE syscall. Returns (value, existed).
func getOrSet(v js.Value, fieldOrIndex any, len int, typ valueType) (js.Value, bool) {
	result := getOrSetFunc.Invoke(v, fieldOrIndex, int(typ), len)
	return result.Index(0), result.Index(1).Bool()
}

func marshalRVal(rVal reflect.Value) (js.Value, error) {
	ctx := marshalContextPool.Get().(*contextM)
	defer func() {
		ctx.ptrLevel = 0
		clear(ctx.seen)
		marshalContextPool.Put(ctx)
	}()
	v, err := getTypeEncoder(rVal.Type())(ctx, rVal)
	if err != nil {
		return js.Value{}, fmt.Errorf("jsgo: %w", err)
	}
	return v, nil
}

func marshalIntoRVal(jsVal js.Value, rVal reflect.Value) error {
	ctx := marshalContextPool.Get().(*contextM)
	defer func() {
		ctx.ptrLevel = 0
		clear(ctx.seen)
		marshalContextPool.Put(ctx)
	}()
	if err := getTypeEncoderInto(rVal.Type())(ctx, jsVal, rVal); err != nil {
		return fmt.Errorf("jsgo: %w", err)
	}
	return nil
}
