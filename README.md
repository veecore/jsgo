# jsgo - Seamless Go/JavaScript Interop 🚀

**Production-Grade Go/JavaScript Type Conversion** - The missing bridge for complex Go/WebAssembly applications

→ **Why?**  
Go's `syscall/js` requires manual type handling. jsgo enables:  
- Automatic struct ↔ object conversion  
- JS Class inheritance (`extends` tag)  
- Complex type support (Dates, Functions, BigInt, Protobufs)  
- Familiar struct tags like `jsgo:"fieldName,omitempty,extends=Parent"`

## 🚀 Quick Start (Problem → Solution)

**Problem**: Manual type conversion hell
```go
// Old way - error-prone and tedious
js.Global().Set("user", map[string]any{
    "name": u.Name,
    "age":  u.Age,
})
```

**jsgo Solution**:
```go
package main

import (
    "github.com/veecore/jsgo"
    "syscall/js"
    "time"
)

type User struct {
    Name string `jsgo:"name"`
    Age  int    `jsgo:"age,omitempty"`
    Feelings func(at time.Time) string // can be executed js side with js Date as argument
}

func main() {
    user := User{Name: "Alice", Age: 30}
    jsVal, _ := jsgo.Marshal(user)
    js.Global().Set("user", jsVal)
}
```

**Problem**: Manual class inheritance  
```javascript
// Old way - fragile prototype chains
class MyGoClass extends Parent {
  constructor() {
    super();  // 😖 Manual super handling
  }
}
```

**jsgo Solution**:  
```go
type Parent struct {
    js.Value `jsgo:",extends=HTMLElement"` // Inherit from browser API
}

jsgo.ExportGoType[Parent](nil)

type WebComponent struct {
    Parent  `jsgo:",extends=Parent"`  // Embedded parent
    State   string `jsgo:"state"`  // Custom fields
    private string                // Unexported fields stay in Go
}

// Register as custom element
jsgo.ExportGoType[WebComponent](nil) // supports constructor
```

```javascript
// Now use as native class!
customElements.define('my-component', window.WebComponent)
```

## 🔑 Key Differentiators
| Feature               | jsgo               | Standard Library |
|-----------------------|--------------------|------------------|
| Class Inheritance     | ✅ Prototype Chain | ❌ Manual        |
| Struct Tags           | ✅ Extends/Name    | ❌               |
| Date ↔ Time           | ✅ Auto            | ❌ Manual        |
| Cyclic Detection      | ✅                 | ❌               |
| Constructor Control   | ✅ Super Args      | ❌               |
| Production Benchmarks | ✅ 5 allocs        | ❌               |
| Function Handling     | ✅ Variadic/Auto   | ❌ Manual        |


## 🛠 Installation
```bash
go get github.com/veecore/jsgo@latest
```

## 🏗 Class Inheritance Deep Dive

**1. Simple Extension**
```go
type Animal struct {
    js.Value `jsgo:",extends=BaseCreature"`
    Legs int `jsgo:"limbs"`
}

// JS: class Animal extends BaseCreature { ... }
```

**2. Custom Super Arguments**
```go
cfg := jsgo.ConstructorConfig{
    Fn: func(size int) *Widget {
        return &Widget{Size: size*2}
    },
    SuperArgs: func(args []js.Value) []js.Value {
        return []js.Value{js.ValueOf("processed")}
    },
}

jsgo.ExportGoTypeWithName[Widget](cfg, "AdvancedWidget")
```

## 📈 Real-World Use Cases

**Web Component Framework**
```go
type WebButton struct {
    jsgo.Value `jsgo:",extends=HTMLButtonElement"`
    Theme string `jsgo:"data-theme,mustBe"`
}

// Constructor with theme validation
func NewButton(theme string) (*WebButton, error) {
    if theme == "" {
        return nil, errors.New("Theme required")
    }
    return &WebButton{Theme: theme}, nil
}

// Export as custom element
jsgo.ExportGoTypeWithName[WebButton](
    NewButton, 
    "MaterialButton",
)
```

```javascript
// Browser usage
const btn = new MaterialButton("dark");
document.body.appendChild(btn);
btn.dataset.theme = "light";  // Type-safe updates
```

## 🏆 Performance Matters

Competitive with handwritten implementations while maintaining developer ergonomics

| Benchmark                      | jsgo Performance | Handwritten      | Advantage |
|--------------------------------|------------------|------------------|-----------|
| **Marshaling**                 |                  |                  |           |
| Complex Struct                 | 431k ops/sec     | 367k ops/sec     | -17%      |
| Primitive Types                | 1.8M ops/sec     | -                |    _      |
| **Unmarshaling**               | 589k ops/sec     | 367k ops/sec     | +38%      |
| **Memory Efficiency**          | 5 allocs/op      | 25+ allocs/op    | **5x**    |


```bash
# Object key
input_size_10     106,949 ns/op    (5 allocs)
input_size_2000   1,392,638 ns/op  (5 allocs)  # 13x slower for 200x data

# Vanilla JS approach comparison
input_size_2000  60,768,849 ns/op  (5992 allocs) # 43x slower than GoField

# Class creation (50k ops/sec)
BenchmarkClassCreate-12      50,265 ops/s     19.8µs/op     0 B/op

# Method calls (1.2M ops/sec)
BenchmarkMethodCall-12    1,234,548 ops/s      812ns/op     0 B/op
```
