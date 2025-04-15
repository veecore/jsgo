package jsgo

import (
	"encoding"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"sync"
	"syscall/js"
)

// Unmarshaler is the interface implemented by types that can decode themselves
// from JavaScript values. Use for custom validation/parsing.
//
// Example:
//
//	type Account struct{ Balance float64 }
//	func (a *Account) UnmarshalJS(v js.Value) error {
//	    if v.Type() != js.TypeObject {
//	        return errors.New("must be object")
//	    }
//	    a.Balance = v.Get("balance").Float()
//	    return nil
//	}
type Unmarshaler interface {
	UnmarshalJS(v js.Value) error
}

// Unmarshal populates Go values from JavaScript values with strict type checking.
//
//	similar to JSON unmarshaling.
//
// Requires a pointer to writable memory.
//
// Handles:
//
// - JS Date → time.Time (UTC)
// - JS BigInt → int64/uint64/big.Int
// - JS Array → Go slice/array
//
// Example:
//
//	var user struct{Name string `jsgo:"username"`}
//	err := Unmarshal(jsObject, &user)
//
// Gotchas:
// - Numeric overflow returns error
// - JS null converts to Go zero values
//
// Implement to encoding.TextUnmarshaler to support complex
// key types when unmarshaling JS objects to Go maps.
func Unmarshal(jsVal js.Value, goVal any) error {
	rVal := reflect.ValueOf(goVal)
	if rVal.Kind() != reflect.Pointer || rVal.IsZero() {
		return fmt.Errorf("jsgo: cannot unmarshal into non/nil pointer")
	}
	ctx := unmarshalContextPool.Get().(*contextU)
	defer func() {
		ctx.ptrLevel = 0
		clear(ctx.seen)
		unmarshalContextPool.Put(ctx)
	}()
	if err := unmarshal(ctx, jsVal, rVal); err != nil {
		return fmt.Errorf("jsgo: %w", err)
	}
	return nil
}

var jsValueType = reflect.TypeFor[js.Value]()
var jsValueTypePtr = reflect.TypeFor[*js.Value]()

func unmarshal(ctx *contextU, jsVal js.Value, goVal reflect.Value) error {
	un, goVal := indirect[Unmarshaler](goVal)
	if un != nil {
		return (*un).UnmarshalJS(jsVal)
	}

	if un, ok := isCanonUnmarshal(goVal.Type()); ok {
		return un(jsVal, goVal)
	}

	var jsValType = jsVal.Type()

	if jsValType != js.TypeNull && goVal.Kind() == reflect.Pointer {
		if goVal.IsNil() {
			nv := reflect.New(goVal.Type().Elem())
			goVal.Set(nv)
		}
		return unmarshal(ctx, jsVal, goVal.Elem())
	}

	if goVal.Type() == jsValueType {
		goVal.Set(reflect.ValueOf(jsVal))
		return nil
	}
	switch jsValType {
	case js.TypeNumber:
		return unmarshalNumber(jsVal, goVal)
	case js.TypeBoolean:
		switch goVal.Kind() {
		case reflect.Bool:
			goVal.SetBool(jsVal.Bool())
		case reflect.Interface:
			if goVal.NumMethod() == 0 {
				goVal.Set(reflect.ValueOf(jsVal.Bool()))
			}
		default:
			return errTypeMismatch(jsValType, goVal.Type())
		}
	case js.TypeString:
		switch goVal.Kind() {
		case reflect.String:
			goVal.SetString(jsVal.String())
		case reflect.Interface:
			if goVal.NumMethod() == 0 {
				goVal.Set(reflect.ValueOf(jsVal.String()))
			}
		default:
			return errTypeMismatch(jsValType, goVal.Type())
		}
	case js.TypeObject:
		if ctx.ptrLevel++; ctx.ptrLevel > startDetectingCyclesAfter {
			if ctx.isSeen(jsVal) {
				// should we panic so this msg stays alone
				return fmt.Errorf("encountered a cycle via %s", goVal.Type())
			}
			ctx.see(jsVal)
			defer ctx.unsee(jsVal)
		}
		if jsVal.InstanceOf(uint8Array) {
			if goVal.Type() != reflect.TypeOf([]byte{}) {
				return errTypeMismatch(jsValType, goVal.Type())
			}
			length := jsVal.Length()
			// Get("length").Int()
			src := make([]byte, length)
			js.CopyBytesToGo(src, jsVal)
			goVal.SetBytes(src)
			return nil
		}

		switch goVal.Kind() {
		case reflect.Interface:
			if !goVal.IsNil() {
				return unmarshal(ctx, jsVal, goVal.Elem())
			}

			if goVal.NumMethod() > 0 {
				return errTypeMismatch(jsValType, goVal.Type())
			}
			if jsVal.InstanceOf(jsArray) {
				len := jsVal.Length()
				s := reflect.MakeSlice(reflect.TypeOf([]any{}), len, len)
				goVal.Set(s)
				return unmarshal(ctx, jsVal, s)
			} else {
				m := reflect.MakeMap(reflect.TypeFor[map[string]any]())
				goVal.Set(m)
				return unmarshal(ctx, jsVal, m)
			}
		case reflect.Map:
			goValT := goVal.Type()
			// if !maybeJsKey(goValT.Key()) {
			// 	return fmt.Errorf("invalid key type: cannot represent js value as %v, key is %v", goValT, goValT.Key())
			// }
			jsKeys := objectKeysGoField(jsVal)
			if goVal.IsNil() {
				goVal.Set(reflect.MakeMapWithSize(goValT, len(jsKeys)))
			}
			for _, k := range jsKeys {
				goValKey := reflect.New(goValT.Key()).Elem()
				if err := parseTo(k, goValKey); err != nil {
					return fmt.Errorf("%v[%v]: JS key %v cannot be represented as %s go type: %w",
						goValT, k, k, goValT.Key(), err)
				}
				goValVal := reflect.New(goValT.Elem()).Elem()
				if err := unmarshal(ctx, jsVal.Get(k), goValVal); err != nil {
					return fmt.Errorf("%v[%v]: %w", goValT, k, err)
				}
				goVal.SetMapIndex(goValKey, goValVal)
			}
		case reflect.Slice, reflect.Array:
			if !IsArray(jsVal) {
				return errTypeMismatch(jsValType, goVal.Type())
			}
			len := jsVal.Length()
			if goVal.Kind() == reflect.Slice {
				if goVal.IsNil() || goVal.Cap() == 0 {
					goVal.Set(reflect.MakeSlice(goVal.Type(), len, len))
				} else {
					needed_space := len - goVal.Len()
					cap := goVal.Cap()
					if needed_space-cap > 0 {
						goVal.Grow(needed_space - cap)
					}

					goVal.SetLen(len)
				}
			}
			if goVal.Len() != len {
				return fmt.Errorf("js array length %d does not match Go array length %d", len, goVal.Len())
			}
			for i := 0; i < len; i++ {
				ith := goVal.Index(i)
				if err := unmarshal(ctx, jsVal.Get(strconv.Itoa(i)), ith); err != nil {
					return fmt.Errorf("%v[%d]: %w", goVal.Type(), i, err)
				}
			}
		case reflect.Struct:
			goValT := goVal.Type()
			fieldInfo := getCachedFields(goValT)
			// Optimize this part... also look for cheap ways
			// to bring back structtag mustbe
			var embeddedJsValue js.Value
			if fieldInfo.embeddedJsValueField != nil {
				embeddedJsValueRv := goVal.
					Field(fieldInfo.embeddedJsValueField.index[0])

				if embeddedJsValueRv.Kind() == reflect.Pointer {
					// We assume they want object
					if embeddedJsValueRv.IsNil() {
						embeddedJsValueRv.Set(reflect.ValueOf(NewObject()).Addr())
					}
					embeddedJsValue = *embeddedJsValueRv.Interface().(*js.Value)
				} else {
					embeddedJsValue = embeddedJsValueRv.Interface().(js.Value)
					if embeddedJsValue.IsUndefined() {
						embeddedJsValue = NewObject()
						embeddedJsValueRv.Set(reflect.ValueOf(embeddedJsValue))
					}
				}
			}
			// Check that it's an object
			if embeddedJsValue.Type() != js.TypeObject {
				embeddedJsValue = js.Undefined()
			}
			// If we go from go fields to requesting for the js key
			// we will be able to remove the added syscalls from
			// getting the keys (even though this has been tricked to 2)
			// we also get the benefit of enforcing that fields with
			// "mustbe" tags are not undefined. So consider going back
			for _, key := range objectKeysGoField(jsVal) {
				field, ok := fieldInfo.normalFields[key]
				if !ok {
					if embeddedJsValue.Type() != js.TypeUndefined {
						embeddedJsValue.Set(key, jsVal.Get(key))
					}
					continue
				}

				goValField, err := getOrFillField(goVal, field.index)
				if err != nil {
					return err
				}
				val := jsVal.Get(key)
				if val.Type() == js.TypeUndefined {
					if field.tag.omitempty {
						continue
					}
					return fmt.Errorf("%v: %s: js value is undefined. want go type %v",
						goValT, field.tag.name, field._type)
				}
				if err := unmarshal(ctx, val, goValField); err != nil {
					return fmt.Errorf("%v: %s: %w", goValT, field.tag.name, err)
				}
			}
		default:
			return errTypeMismatch(jsValType, goVal.Type())
		}

	case js.TypeNull:
		switch goVal.Kind() {
		case reflect.Struct, reflect.Map, reflect.Interface, reflect.Pointer:
			goVal.SetZero()
			return nil
		default:
			return fmt.Errorf("cannot set %v to null", goVal.Kind())
		}
	case js.TypeUndefined:
		return fmt.Errorf("js value is undefined. want go type %v", goVal.Type())
	default:
		// This could also be js.TypeFunc to go func which can't be handled
		return errTypeMismatch(jsValType, goVal.Type())
	}
	return nil
}

func errTypeMismatch(jsType js.Type, goType reflect.Type) error {
	return fmt.Errorf("type mismatch: cannot unmarshal js %v into go type %v", jsType, goType)
}

type number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

func errOverflow[T number](v T, t reflect.Type) error {
	return fmt.Errorf("%v overflows %v", v, t)
}

func unmarshalNumber(jsVal js.Value, goVal reflect.Value) error {
	// Handle BigInt first
	if jsBigInt.Truthy() && jsVal.InstanceOf(jsBigInt) {
		bigIntStr := jsVal.Call("toString").String()
		bigInt := new(big.Int)
		if _, ok := bigInt.SetString(bigIntStr, 10); !ok {
			return fmt.Errorf("invalid BigInt: %s", bigIntStr)
		}

		switch {
		case goVal.CanInt():
			if !bigInt.IsInt64() {
				return fmt.Errorf("BigInt %s overflows int64", bigIntStr)
			}
			goVal.SetInt(bigInt.Int64())
			return nil
		case goVal.CanUint():
			if !bigInt.IsUint64() {
				return fmt.Errorf("BigInt %s overflows uint64", bigIntStr)
			}
			goVal.SetUint(bigInt.Uint64())
			return nil
		case goVal.Kind() == reflect.Interface && goVal.NumMethod() == 0:
			goVal.Set(reflect.ValueOf(bigInt))
			return nil
		case goVal.Type() == reflect.TypeOf(big.Int{}):
			goVal.Set(reflect.ValueOf(*bigInt))
			return nil
		}
	}

	if jsVal.Type() != js.TypeNumber {
		panic("type not number") // panicking because the caller is from this package so it must be a bug
	}

	num := jsVal.Float()
	switch {
	case goVal.CanInt():
		if math.IsNaN(num) || math.Trunc(num) != num {
			return fmt.Errorf("cannot convert float %v to integer", num)
		}
		if goVal.OverflowInt(int64(num)) {
			return errOverflow(num, goVal.Type())
		}
		goVal.SetInt(int64(num))
	case goVal.CanUint():
		if num < 0 || math.Trunc(num) != num {
			return fmt.Errorf("cannot convert float %v to unsigned integer", num)
		}
		if goVal.OverflowUint(uint64(num)) {
			return errOverflow(num, goVal.Type())
		}
		goVal.SetUint(uint64(num))
	case goVal.CanFloat():
		if goVal.OverflowFloat(num) {
			return errOverflow(num, goVal.Type())
		}
		goVal.SetFloat(num)
	case goVal.Kind() == reflect.Interface && goVal.NumMethod() == 0:
		goVal.Set(reflect.ValueOf(num))
	default:
		return errTypeMismatch(jsVal.Type(), goVal.Type())
	}

	return nil
}

var textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()

func errParse(v string, t reflect.Type, err error) error {
	if err == nil {
		return fmt.Errorf("failed to parse %s as %v", v, t)
	}
	return fmt.Errorf("failed to parse %s as %v: %w", v, t, err)
}

func parseTo(s string, v reflect.Value) error {
	kp, v := indirect[encoding.TextUnmarshaler](v)
	if kp != nil {
		return (*kp).UnmarshalText([]byte(s))
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			nv := reflect.New(v.Type().Elem())
			v.Set(nv)
		}
		return parseTo(s, v.Elem())
	case reflect.Interface:
		if v.NumMethod() == 0 {
			v.Set(reflect.ValueOf(s))
			return nil
		}

		if v.IsValid() {
			return parseTo(s, v.Elem())
		}
		return errParse(s, v.Type(), nil)
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		if s == "true" {
			v.SetBool(true)
		} else if s == "false" {
			v.SetBool(false)
		}
		return errParse(s, v.Type(), nil)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.Atoi(s)
		if err != nil {
			return errParse(s, v.Type(), err)
		}
		if v.OverflowInt(int64(i)) {
			return errOverflow(i, v.Type())
		}
		v.SetInt(int64(i))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.Atoi(s)
		if err != nil {
			return errParse(s, v.Type(), err)
		}
		if u < 0 || v.OverflowUint(uint64(u)) {
			return errOverflow(u, v.Type())
		}
		v.SetUint(uint64(u))
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return errParse(s, v.Type(), err)
		}
		if v.OverflowFloat(f) {
			return errOverflow(f, v.Type())
		}
		v.SetFloat(f)
	default:
		return errParse(s, v.Type(), nil)
	}
	return nil
}

const startDetectingCyclesAfter = 1000

// disgusting
type contextU struct {
	ptrLevel uint
	seen     []js.Value
}

func (c *contextU) see(obj js.Value) {
	if !c.isSeen(obj) {
		c.seen = append(c.seen, obj)
	}
}

func (c *contextU) unsee(obj js.Value) {
	for i, v := range c.seen {
		if v.Equal(obj) {
			c.seen[i] = c.seen[len(c.seen)-1]
			c.seen = c.seen[:len(c.seen)-1]
			return
		}
	}
}

func (c *contextU) isSeen(obj js.Value) bool {
	for _, v := range c.seen {
		if v.Equal(obj) {
			return true
		}
	}
	return false
}

// TODO: Consider bringing back
// type contextU struct {
// 	ptrLevel uint
// 	// since we can't compare js.Value, we'll have to use []js.Value
// 	// which would have us at O(n)... considering this is a rare
// 	// case, and we're going to check this after 1000 objects, maybe
// 	// we should use pure go
// 	seen map[uint64]struct{}
// }

// func (c *contextU) see(obj js.Value) {
// 	// we target the ref field
// 	c.seen[*(*uint64)(unsafe.Add(unsafe.Pointer(&obj), 0))] = struct{}{}
// }

// func (c *contextU) unsee(obj js.Value) {
// 	// we target the ref field
// 	delete(c.seen, *(*uint64)(unsafe.Add(unsafe.Pointer(&obj), 0)))
// }

// func (c *contextU) isSeen(obj js.Value) bool {
// 	// we target the ref field
// 	_, ok := c.seen[*(*uint64)(unsafe.Add(unsafe.Pointer(&obj), 0))]
// 	return ok
// }

// var _ = func() struct{} {
// 	rJ := reflect.TypeFor[js.Value]()
// 	mayStillBeOk := rJ.NumField() > 2 && rJ.Field(0).Type == reflect.TypeFor[[0]func()]() &&
// 		rJ.Field(1).Type.Kind() == reflect.Uint64 && rJ.Field(1).Name == "ref"

// 	if !mayStillBeOk {
// 		panic("I'm sorry for my past mistake... don't sue me, please. You shouldn't have updated.")
// 	}
// 	return struct{}{}
// }()

// Oh god not another pool
var unmarshalContextPool = sync.Pool{
	New: func() any {
		return &contextU{
			// seen: make(map[uint64]struct{}),
		}
	},
}

func unmarshalRval(jsVal js.Value, rVal reflect.Value) error {
	ctx := unmarshalContextPool.Get().(*contextU)
	defer func() {
		ctx.ptrLevel = 0
		clear(ctx.seen)
		unmarshalContextPool.Put(ctx)
	}()
	return unmarshal(ctx, jsVal, rVal)
}
