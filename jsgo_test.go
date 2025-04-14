package jsgo_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall/js"
	"testing"
	"time"

	"github.com/veecore/jsgo"
)

type Model struct {
	Name    string     `jsgo:"name"`
	Info    *string    `jsgo:"info"`
	Version **string   `jsgo:"version"`
	Authors []string   `jsgo:"authors"`
	Release *time.Time `jsgo:"release"`
	Custom  *custom    `jsgo:"custom"`
}

type custom struct {
	Val map[string]string
}

func (c *custom) UnmarshalJS(v js.Value) error {
	if v.Type() != js.TypeString {
		return fmt.Errorf("main: js value is not string")
	}

	return json.Unmarshal([]byte(v.String()), &c.Val)
}

func TestUnmarshalModel(t *testing.T) {
	jsModel := js.ValueOf(map[string]interface{}{
		"name":    "jsgo",
		"info":    js.Null(),
		"version": "v1.0.0",
		"authors": []interface{}{"developer1", "developer2"},
		"release": jsgo.Type("Date").New("2024-04-30T23:00:00Z"),
		"custom":  `{"key":"value"}`,
	})

	var model Model
	if err := jsgo.Unmarshal(jsModel, &model); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	var version = "v1.0.0"
	version_ptr := &version
	var date = time.Date(2024, 04, 30, 23, 0, 0, 0, time.UTC)
	var expected = Model{
		Name:    "jsgo",
		Info:    nil,
		Version: &version_ptr,
		Authors: []string{"developer1", "developer2"},
		Release: &date,
		Custom:  &custom{Val: map[string]string{"key": "value"}},
	}

	if model.Name != expected.Name {
		t.Errorf("Name mismatch: %q vs %q", model.Name, expected.Name)
	}
	if model.Info != expected.Info {
		t.Errorf("Info mismatch: %q vs %q", model.Name, expected.Name)
	}
	if **model.Version != **expected.Version {
		t.Errorf("Version mismatch: %q vs %q", **model.Version, **expected.Version)
	}
	if !slices.Equal(model.Authors, expected.Authors) {
		t.Errorf("Authors mismatch: %q vs %q", model.Authors, expected.Authors)
	}
	if !model.Release.Equal(*expected.Release) {
		t.Errorf("Date mismatch: %v vs %v", model.Release, expected.Release)
	}
	if !maps.Equal(model.Custom.Val, expected.Custom.Val) {
		t.Errorf("custom mismatch: %q vs %q", model.Custom, expected.Custom)
	}
}

func TestUnmarshal(t *testing.T) {
	t.Run("NestedNilPointers", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"name":    "test",
			"version": js.Null(),
		})
		var m Model
		if err := jsgo.Unmarshal(jsVal, &m); err != nil {
			t.Fatal(err)
		}
		if m.Version != nil {
			t.Error("Version should remain nil")
		}
	})

	t.Run("Cyclic Js Value", func(t *testing.T) {
		jsVal := js.Global().Get("Object").New()
		jsVal.Set("Value", 3)
		jsVal.Set("Next", jsVal)
		next := jsVal.Get("Next")
		if !jsVal.Equal(next) {
			t.Fatal("js-std: field set incorrectly")
		}
		type LinkedList struct {
			Value int
			Next  *LinkedList
		}
		var l LinkedList
		if err := jsgo.Unmarshal(jsVal, &l); err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatal("cyclic js value should be handled")
		}
	})

	t.Run("NullToPointer", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"info": js.Null(),
		})

		var model struct {
			Info *string `jsgo:"info"`
		}
		if err := jsgo.Unmarshal(jsVal, &model); err != nil {
			t.Fatal(err)
		}
		if model.Info != nil {
			t.Error("Expected nil pointer for JS null")
		}
	})

	t.Run("PointerHell", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"version": "v2.0.0",
		})

		var model struct {
			Version ***string `jsgo:"version"`
		}

		if err := jsgo.Unmarshal(jsVal, &model); err != nil {
			t.Fatal(err)
		}

		if ***model.Version != "v2.0.0" {
			t.Errorf("Triple pointer failure: got %q", ***model.Version)
		}
	})
}

func TestCustomUnmarshaler(t *testing.T) {
	t.Run("InvalidCustomType", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"custom": 42, // Should be string
		})

		var model struct {
			Custom *custom `jsgo:"custom"`
		}
		err := jsgo.Unmarshal(jsVal, &model)
		if err == nil || !strings.Contains(err.Error(), "not string") {
			t.Errorf("Should fail on type mismatch for custom unmarshaler: %v", err)
		}
	})

	t.Run("NilCustomUnmarshaler", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"custom": `{"key":"value"}`,
		})

		var model struct {
			Custom *custom `jsgo:"custom"`
		}
		err := jsgo.Unmarshal(jsVal, &model)
		if err != nil {
			t.Fatal("Should handle nil custom unmarshaler pointer")
		}
		if model.Custom == nil || model.Custom.Val["key"] != "value" {
			t.Error("Failed to initialize nil custom pointer")
		}
	})
}

func TestTypeConversion(t *testing.T) {
	t.Run("NumberOverflow", func(t *testing.T) {
		jsVal := js.ValueOf(1 << 30)

		var smallInt int8
		err := jsgo.Unmarshal(jsVal, &smallInt)
		if err == nil || !strings.Contains(err.Error(), "overflow") {
			t.Error("Should detect numeric overflow")
		}
	})

	t.Run("BoolToString", func(t *testing.T) {
		jsVal := js.ValueOf(true)

		var s string
		err := jsgo.Unmarshal(jsVal, &s)
		if err == nil || !strings.Contains(err.Error(), "type mismatch") {
			t.Error("Should reject bool→string conversion")
		}
	})
}

func TestStructTag(t *testing.T) {
	t.Run("IgnoredField", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"_secret": "should be ignored",
		})

		var model struct {
			Secret string `jsgo:"_"`
		}
		if err := jsgo.Unmarshal(jsVal, &model); err != nil {
			t.Fatal(err)
		}
		if model.Secret != "" {
			t.Error("Ignored field should not be set")
		}
	})

	t.Run("RenamedField", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"js_name": "test",
		})

		var model struct {
			Name string `jsgo:"js_name"`
		}
		if err := jsgo.Unmarshal(jsVal, &model); err != nil {
			t.Fatal(err)
		}
		if model.Name != "test" {
			t.Error("Field renaming via tag failed")
		}
	})
}

func TestJSDate(t *testing.T) {
	t.Run("InvalidDateObject", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"release": js.ValueOf("not a date"),
		})

		var model struct {
			Release *time.Time `jsgo:"release"`
		}
		err := jsgo.Unmarshal(jsVal, &model)
		if err == nil || !strings.Contains(err.Error(), "time.Time") {
			t.Error("Should reject non-date values for time.Time")
		}
	})

	t.Run("JS Date to time.Time", func(t *testing.T) {
		jsDate := js.Global().Get("Date").New("2023-01-01T12:00:00Z")
		var got time.Time
		err := jsgo.Unmarshal(jsDate, &got)
		if err != nil {
			t.Fatal(err)
		}

		if !got.Equal(time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("unexpected time: %v", got)
		}
	})

	t.Run("DateWithTimezone", func(t *testing.T) {
		jsDate := js.Global().Get("Date").New(2024, 4, 1, 12, 30, 0)
		var Release *time.Time
		if err := jsgo.Unmarshal(jsDate, &Release); err != nil {
			t.Fatal(err)
		}

		expected := time.Date(2024, 5, 1, 11, 30, 0, 0, time.UTC)
		if !Release.Equal(expected) {
			t.Errorf("Timezone handling failed: got %v", Release)
		}
	})
}

func TestSliceAndArray(t *testing.T) {
	t.Run("ArrayLengthMismatch", func(t *testing.T) {
		jsVal := js.ValueOf([]interface{}{1, 2, 3})

		var arr [2]int
		err := jsgo.Unmarshal(jsVal, &arr)
		if err == nil || !strings.Contains(err.Error(), "length") {
			t.Error("Should detect array length mismatch")
		}
	})

	t.Run("NilSlice", func(t *testing.T) {
		jsVal := js.ValueOf([]interface{}{1, 2, 3})

		var slice []int
		if err := jsgo.Unmarshal(jsVal, &slice); err != nil {
			t.Fatal(err)
		}
		if len(slice) != 3 || cap(slice) != 3 {
			t.Error("Slice not properly initialized")
		}
	})
}

func TestMapKeyHandling(t *testing.T) {
	t.Run("NonStringMapKeys", func(t *testing.T) {
		jsVal := js.ValueOf(map[string]interface{}{
			"42": "answer",
		})

		var m map[int]string
		if err := jsgo.Unmarshal(jsVal, &m); err != nil {
			t.Fatal(err)
		}
		if m[42] != "answer" {
			t.Error("Numeric map key conversion failed")
		}
	})

	t.Run("CustomKeyType", func(t *testing.T) {
		type customKey struct{ id string }

		jsVal := js.ValueOf(map[string]interface{}{
			"key1": "value1",
		})

		var m map[customKey]string
		err := jsgo.Unmarshal(jsVal, &m)
		if err == nil {
			t.Errorf("Should reject unhandlable key types: %v", err)
		}
	})
}

// TestThrow tests error propagation to JS
func TestThrow(t *testing.T) {
	t.Run("basic error", func(t *testing.T) {
		err := errors.New("validation failed")
		jsErr := jsgo.Throw(err)
		if jsErr.Type() != js.TypeObject {
			t.Fatal("thrown error should be object")
		}

		if message := jsErr.Get("message").String(); message != err.Error() {
			t.Errorf("expected message %q, got %q", err.Error(), message)
		}
	})

	t.Run("error wrapping", func(t *testing.T) {
		root := fmt.Errorf("root cause")
		wrapped := fmt.Errorf("operation failed: %w", root)
		jsErr := jsgo.Throw(wrapped)

		if !jsErr.InstanceOf(js.Global().Get("Error")) {
			t.Error("should be Error instance")
		}
	})
}

func TestMarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   any
		jsType  js.Type
		wantErr bool
	}{
		{
			name:    "cyclic structure",
			input:   createCyclicStruct(),
			jsType:  js.TypeNull,
			wantErr: true,
		},
		{
			name:   "time.Time to Date",
			input:  time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC),
			jsType: js.TypeObject,
		},
		{
			name:   "big.Int to BigInt",
			input:  big.NewInt(123456789),
			jsType: js.TypeObject,
		},
		{
			name:   "nil pointer",
			input:  (*int)(nil),
			jsType: js.TypeNull,
		},
		{
			name:    "channel should error",
			input:   make(chan int),
			jsType:  js.TypeUndefined,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := jsgo.Marshal(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("Marshal() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && got.Type() != tt.jsType {
				t.Errorf("expected JS type %v, got %v", tt.jsType, got.Type())
			}
		})
	}
}

func TestExtendsTag(t *testing.T) {
	t.Run("basic inheritance", func(t *testing.T) {
		type S struct {
			js.Value `jsgo:",extends=ParentClass"`
		}
		typ := reflect.TypeOf(S{})
		if jsgo.GetExtendsTag(typ) != "ParentClass" {
			t.Error("Failed to detect extends tag")
		}
	})

	t.Run("multiple embedded fields", func(t *testing.T) {
		type A struct{}
		type B struct{}
		type S struct {
			A `jsgo:",extends=FirstClass"`
			B `jsgo:",extends=SecondClass"`
		}
		typ := reflect.TypeOf(S{})
		if jsgo.GetExtendsTag(typ) != "FirstClass" {
			t.Error("Should return first valid extends tag")
		}
	})

	t.Run("non-embedded field", func(t *testing.T) {
		type S struct {
			Field struct{} `jsgo:",extends=SomeClass"`
		}
		typ := reflect.TypeOf(S{})
		if jsgo.GetExtendsTag(typ) != "" {
			t.Error("Should ignore non-embedded fields")
		}
	})
}

func TestFuncOfAny(t *testing.T) {
	t.Run("argument mutation not reflected", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(1)

		fn := jsgo.FuncOfAny(func(x int) {
			x++ // Mutation shouldn't affect JS side
			wg.Done()
		})
		defer fn.Release()

		initial := 5
		fn.Invoke(js.ValueOf(initial))
		wg.Wait()
		if initial != 5 {
			t.Error("js value cannot be changed through go")
		}
	})

	// t.Run("panic recovery", func(t *testing.T) {
	// 	fn := jsgo.FuncOfAny(func() {
	// 		panic("runtime failure")
	// 	})
	// 	defer fn.Release()

	// 	defer func() {
	// 		if r := recover(); r != nil {
	// 			t.Error("panic leaked to Go runtime")
	// 		}
	// 	}()

	// 	result := fn.Invoke()
	// 	if result.Type() != js.TypeObject {
	// 		t.Error("expected error object")
	// 	}
	// })

	t.Run("return values with error", func(t *testing.T) {
		fn := jsgo.FuncOfAny(func() (int, error) {
			return 0, errors.New("failed")
		})
		defer fn.Release()

		result := fn.Invoke()
		if result.Type() != js.TypeObject {
			t.Error("expected error object")
		}
	})
}

func createCyclicStruct() any {
	type Node struct {
		Child *Node
	}
	n := &Node{}
	n.Child = n
	return n
}

// TestBasicClass tests basic class export with default constructor
func TestBasicClass(t *testing.T) {
	type SimpleStruct struct{}

	t.Run("basic export", func(t *testing.T) {
		release, err := jsgo.ExportGoTypeWithName[SimpleStruct](
			nil,
			"SimpleClass",
		)
		if err != nil {
			t.Fatalf("Export failed: %v", err)
		}
		defer release()

		// Test JS class existence
		if !js.Global().Get("SimpleClass").Truthy() {
			t.Error("Class not registered globally")
		}

		t.Run("instance creation", func(t *testing.T) {
			js.Global().Call("eval", `
				globalThis.testInstance = new SimpleClass();
			`)

			instance := js.Global().Get("testInstance")
			if !instance.Truthy() {
				t.Error("Failed to create instance")
			}
		})

		if !js.Global().Get("SimpleClass").Truthy() {
			t.Error("Class disappeared prematurely")
		}
	})

	// Now test cleanup
	t.Run("after release", func(t *testing.T) {
		if js.Global().Get("SimpleClass").Truthy() {
			t.Error("Class not cleaned up")
		}
	})
}

// TestClassInheritance tests prototype chain setup
func TestClassInheritance(t *testing.T) {
	// Setup parent class
	drop := createClass("ParentClass", `
		class  {
			parentMethod() { return 42 }
		}
	`)
	defer drop()
	type ChildStruct struct {
		js.Value `jsgo:",extends=ParentClass"`
	}

	t.Run("inheritance setup", func(t *testing.T) {
		release, err := jsgo.ExportGoTypeWithName[ChildStruct](
			nil,
			"ChildClass",
		)
		if err != nil {
			t.Fatalf("Export failed: %v", err)
		}
		defer release()

		// Verify prototype chain
		childProto := js.Global().Get("ChildClass").Get("prototype")
		parentProto := js.Global().Get("ParentClass").Get("prototype")
		if !js.Global().Get("Object").Get("prototype").Call("isPrototypeOf", parentProto, childProto).Bool() {
			t.Error("Prototype chain not properly set up")
		}

		t.Run("parent method access", func(t *testing.T) {
			js.Global().Call("eval", `
			globalThis.childInstance = new ChildClass();
		`)
			result := js.Global().Get("childInstance").Call("parentMethod").Int()
			if result != 42 {
				t.Errorf("Expected 42 from parent method, got %d", result)
			}
		})
	})
}

// TestConstructors tests various constructor scenarios
func TestConstructors(t *testing.T) {
	type ConfiguredStruct struct {
		Value int
	}

	t.Run("default constructor", func(t *testing.T) {
		release, err := jsgo.ExportGoTypeWithName[ConfiguredStruct](
			nil,
			"DefaultClass",
		)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		instance := js.Global().Get("DefaultClass").New()
		if !instance.Truthy() {
			t.Error("Default constructor failed")
		}
	})

	t.Run("custom constructor", func(t *testing.T) {
		constructor := func(v int) (*ConfiguredStruct, error) {
			return &ConfiguredStruct{Value: v * 2}, nil
		}

		release, err := jsgo.ExportGoTypeWithName[ConfiguredStruct](
			constructor,
			"ConstructedClass",
		)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		instance := js.Global().Get("ConstructedClass").New(21)
		result := instance.Get("Value").Int()
		if result != 42 {
			t.Errorf("Expected Value=42, got %d", result)
		}
	})

	t.Run("constructor with super args", func(t *testing.T) {

		drop := createClass("SuperClass", `
		class {
				constructor(v) { this.value = v }
			}
		`)
		defer drop()

		type SubClass struct {
			js.Value `jsgo:",extends=SuperClass"`
			Extra    int
		}

		cfg := jsgo.ConstructorConfig{
			Fn: func(v int) (*SubClass, error) {
				return &SubClass{Extra: v * 2}, nil
			},
			SuperArgs: func(args []js.Value) []js.Value {
				return []js.Value{js.ValueOf(args[0].Int() * 3)}
			},
		}

		release, err := jsgo.ExportGoTypeWithName[SubClass](
			cfg,
			"ConfiguredSubClass",
		)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		instance := js.Global().Get("ConfiguredSubClass").New(14)
		if instance.Get("value").Int() != 42 || // From super
			instance.Get("Extra").Int() != 28 { // From child
			t.Errorf("Unexpected values: %v", instance)
		}
	})
}

type MethodStruct struct{}

func (m *MethodStruct) Add(a, b int) int { return a + b }
func (m *MethodStruct) unexported()      {} // Should not be exposed

// TestMethodExport tests method exposure
func TestMethodExport(t *testing.T) {

	t.Run("method availability", func(t *testing.T) {
		release, err := jsgo.ExportGoTypeWithName[*MethodStruct](
			nil,
			"MethodClass",
		)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		instance := js.Global().Get("MethodClass").New()
		result := instance.Call("Add", 19, 23).Int()
		if result != 42 {
			t.Errorf("Expected 42, got %d", result)
		}

		// Verify unexported method isn't present
		if instance.Get("unexported").Truthy() {
			t.Error("Unexported method was exposed")
		}
	})
}

// TestErrorHandling tests various error scenarios
func TestErrorHandling(t *testing.T) {
	// t.Run("invalid type", func(t *testing.T) {
	// 	_, err := jsgo.ExportGoTypeWithName[[]chan struct{}](
	// 		nil,
	// 		"InvalidClass",
	// 	)

	// 	if err == nil {
	// 		t.Error("Expected error for invalid type")
	// 	}
	// })

	t.Run("empty name", func(t *testing.T) {
		_, err := jsgo.ExportGoTypeWithName[struct{}](
			nil,
			"",
		)
		if err == nil {
			t.Error("Expected error for empty name")
		}
	})

	t.Run("missing parent class", func(t *testing.T) {
		type BadStruct struct {
			js.Value `jsgo:",extends=NonExistentClass"`
		}

		_, err := jsgo.ExportGoTypeWithName[BadStruct](
			nil,
			"BadClass",
		)
		if err == nil {
			t.Error("Expected error for missing parent class")
		}
	})

	t.Run("invalid constructor", func(t *testing.T) {
		type ValidStruct struct{}
		badConstructor := "not a function"

		_, err := jsgo.ExportGoTypeWithName[ValidStruct](
			badConstructor,
			"BadConstructorClass",
		)
		if err == nil {
			t.Error("Expected error for invalid constructor")
		}
	})
}

// TestResourceCleanup tests resource release functionality
func TestResourceCleanup(t *testing.T) {
	type CleanupStruct struct{}

	release, err := jsgo.ExportGoTypeWithName[CleanupStruct](
		nil,
		"CleanupClass",
	)
	if err != nil {
		t.Fatal(err)
	}

	// Release resources and delete class
	release()

	// Verify class was removed
	if js.Global().Get("CleanupClass").Truthy() {
		t.Error("Class not cleaned up")
	}

	// Verify methods are gone
	class := js.Global().Get("CleanupClass")
	if class.Truthy() {
		t.Error("Constructor still works after release")
	}
}

func createClass(name, raw string) (drop func()) {
	js.Global().Call("eval", fmt.Sprintf("globalThis.%v = %s", name, raw))
	return func() {
		js.Global().Delete("ParentClass")
	}
}
