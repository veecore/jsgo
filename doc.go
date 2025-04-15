// Package jsgo provides complete Go↔JavaScript interoperability for WebAssembly
// with advanced features beyond standard library capabilities:
//
// Features:
// - Bidirectional type conversion with struct tags (`jsgo:"field"`)
// - Export any Go function to JavaScript
// - Create JS classes from Go structs with inheritance
// - Automatic cycle detection and error handling
// - Pooled memory management for high-performance
// - Extended type support (time.Time, big.Int)
//
// # Core Concepts
//
//  1. Function Export:
//     Export Go functions to JS with any signature using FuncOfAny:
//
//     jsgo.FuncOfAny(func(x int) string { ... })
//
//  2. Class System:
//     Create JS classes from Go structs with inheritance:
//
//     type User struct {
//     Base `jsgo:",extends=BaseClass"`
//     Name string `jsgo:"username"`
//     }
//
//  3. Type Conversion:
//     Convert complex types bidirectionally:
//
//     jsgo.Marshal(userStruct) → JS object
//     jsgo.Unmarshal(jsObj, &userStruct)
//
// # Security
//
// - Validate all JS callback arguments
// - Never expose unguarded system functions
// - Use Release() for sensitive resources
//
// # Struct Tags
// `jsgo:"-"`                     // Ignore field
// `jsgo:"fieldName"`             // Custom property name
// `jsgo:",omitempty"`            // Omit if empty
// `jsgo:",extends=BaseClass"`    // Inheritance
package jsgo
