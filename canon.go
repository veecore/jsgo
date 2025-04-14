package jsgo

import (
	"fmt"
	"reflect"
	"syscall/js"
	"time"
)

var timeType = reflect.TypeFor[time.Time]()

var (
	canons = map[string]string{
		"time": timeType.String(),
	}

	canonUnmarshals = map[string]func(js.Value, reflect.Value) error{
		canons["time"]: timeUnmarshal,
	}

	canonMarshals = map[string]encoderFunc{
		canons["time"]: func(ctx *contextM, goVal reflect.Value) (js.Value, error) {
			var jsDate = jsDate.New()
			return jsDate, timeMarshalInto(ctx, jsDate, goVal)
		},
	}

	canonMarshalIntos = map[string]encodeIntoFunc{
		canons["time"]: timeMarshalInto,
	}
)

func isCanonUnmarshal(t reflect.Type) (func(js.Value, reflect.Value) error, bool) {
	un, ok := canonUnmarshals[t.String()]
	return un, ok
}

func timeUnmarshal(jsVal js.Value, goVal reflect.Value) error {
	if goVal.Type().String() != canons["time"] {
		return errTypeMismatch(jsVal.Type(), goVal.Type())
	}

	if !jsVal.InstanceOf(jsDate) {
		return errTypeMismatch(jsVal.Type(), goVal.Type())
	}

	// if jsVal.Type() != js.TypeObject {
	// 	return errTypeMismatch(jsVal.Type(), goVal.Type())
	// }

	// if jsVal.Get("toISOString").Type() != js.TypeFunction {
	// 	return fmt.Errorf("%w: missing toISOString.", errTypeMismatch(jsVal.Type(), goVal.Type()))
	// }

	t := jsVal.Call("toISOString")
	if t.Type() != js.TypeString {
		return fmt.Errorf("invalid Date implementation returned non-string: %v toISOString()", t.Type())
	}

	var goTime time.Time
	if err := (&goTime).UnmarshalText([]byte(t.String())); err != nil {
		return err
	}
	goVal.Set(reflect.ValueOf(goTime))
	return nil
}

func isCanonMarshal(t reflect.Type) (encoderFunc, bool) {
	m, ok := canonMarshals[t.String()]
	return m, ok
}

func isCanonMarshalInto(t reflect.Type) (encodeIntoFunc, bool) {
	m, ok := canonMarshalIntos[t.String()]
	return m, ok
}

func timeMarshalInto(_ *contextM, jsVal js.Value, goVal reflect.Value) error {
	if !jsVal.InstanceOf(jsDate) {
		return fmt.Errorf("cannot MarshalInto js type: %v from time.Time", jsVal.Type())
	}
	// JS Date constructor takes milliseconds since epoch
	millis := goVal.Interface().(time.Time).UnixNano() / int64(time.Millisecond)
	jsVal.Call("setTime", millis)
	return nil
}
