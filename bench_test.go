//go:build js && wasm
// +build js,wasm

package jsgo_test

import (
	"fmt"
	"strconv"
	"syscall/js"
	"testing"
	"time"

	"github.com/veecore/jsgo"
)

func BenchmarkMarshal(b *testing.B) {
	type ComplexStruct struct {
		ID      int
		Name    string
		Tags    []string
		Created time.Time
	}

	// Static time to prevent allocation in loop
	staticTime := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	data := ComplexStruct{
		ID:      1,
		Name:    "benchmark",
		Tags:    []string{"go", "js", "wasm"},
		Created: staticTime,
	}

	b.ResetTimer()
	b.Run("ComplexStruct", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := jsgo.Marshal(data)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("PrimitiveTypes", func(b *testing.B) {
		inputs := []any{
			42,
			"simple string",
			[]int{1, 2, 3},
			map[string]any{"key": "value"},
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			val := inputs[i%len(inputs)]
			_, err := jsgo.Marshal(val)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkMarshalHandWritten(b *testing.B) {
	// Static time to prevent allocation in loop
	staticTime := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	b.ResetTimer()
	b.Run("ComplexStruct", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = js.ValueOf(map[string]any{
				"ID":      1,
				"Name":    "benchmark",
				"Tags":    []any{"go", "js", "wasm"},
				"Created": js.Global().Get("Date").New(staticTime.String()),
			})
		}
	})
}

func BenchmarkUnmarshal(b *testing.B) {
	jsObj := js.Global().Get("Object").New()
	jsObj.Set("id", 123)
	jsObj.Set("name", "benchmark")
	jsObj.Set("tags", []any{"go", "js", "wasm"})
	jsObj.Set("created", js.Global().Get("Date").New())

	var result struct {
		ID      int       `jsgo:"id"`
		Name    string    `jsgo:"name"`
		Tags    []string  `jsgo:"tags"`
		Created time.Time `jsgo:"created"`
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := jsgo.Unmarshal(jsObj, &result)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnMarshalHandWritten(b *testing.B) {
	jsObj := js.Global().Get("Object").New()
	jsObj.Set("id", 123)
	jsObj.Set("name", "benchmark")
	jsObj.Set("tags", []any{"go", "js", "wasm"})
	jsObj.Set("created", js.Global().Get("Date").New())

	var result struct {
		ID      int
		Name    string
		Tags    []string
		Created time.Time
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := unmarshalHandwritten(jsObj, &result)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func unmarshalHandwritten(jsVal js.Value, goVal *struct {
	ID      int
	Name    string
	Tags    []string
	Created time.Time
}) error {
	id := jsVal.Get("id")
	if id.Type() != js.TypeNumber {
		return fmt.Errorf("id: expected int parallel got: %v", id.Type())
	}
	goVal.ID = id.Int()

	name := jsVal.Get("name")
	if name.Type() != js.TypeString {
		return fmt.Errorf("name: expected string parallel got: %v", id.Type())
	}
	goVal.Name = name.String()

	tags := jsVal.Get("tags")
	if tags.Type() == js.TypeObject {
		len := tags.Length()
		goVal.Tags = make([]string, len)
		for i := 0; i < len; i++ {
			tag := tags.Get(strconv.Itoa(i))
			if tag.Truthy() && tag.Type() == js.TypeString {
				goVal.Tags[i] = tag.String()
			} else {
				return fmt.Errorf("tag[%d]: expected string parallel got: %v", i, tag.Type())
			}
		}
	} else {
		return fmt.Errorf("tags: expected []string parallel got: %v", id.Type())
	}
	t := jsVal.Get("created")
	if t.InstanceOf(js.Global().Get("Date")) {
		t = t.Call("toISOString")
		if err := (&goVal.Created).UnmarshalText([]byte(t.String())); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("created: expected time.Time parallel got: %v", id.Type())
	}
	return nil
}

func set_benchmark[T any, Out any](data []struct {
	Value T
	Size  uint
}, f func(input T) Out, assert func(Out, uint) bool) func(b *testing.B) {
	return func(b *testing.B) {
		for _, d := range data {
			b.Run(fmt.Sprintf("input_size_%d", d.Size), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if !assert(f(d.Value), d.Size) {
						b.Fatalf("benchmark assertion failed")
					}
				}
			})
		}
	}
}

var randomObjects = []struct {
	Value js.Value
	Size  uint
}{
	{randomObject(10), 10},
	{randomObject(50), 50},
	{randomObject(200), 200},
	{randomObject(2000), 2000},
}

func randomObject(fields uint) js.Value {
	object := make(map[string]any, fields)
	for i := range int(fields) {
		object[strconv.Itoa(i)] = true
	}
	return js.ValueOf(object)
}

func ObjectKeysAssert(out []string, size uint) bool {
	return len(out) == int(size) // This is a benchmark not a test
}

func BenchmarkObjectKeysIter(b *testing.B) {
	set_benchmark(randomObjects, jsgo.ObjectKeys, ObjectKeysAssert)(b)
}

func BenchmarkObjectKeysGoField(b *testing.B) {
	set_benchmark(randomObjects, jsgo.ObjectKeysGoField, ObjectKeysAssert)(b)
}
