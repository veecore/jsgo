package jsgo_test

import (
	"fmt"
	"syscall/js"

	"github.com/veecore/jsgo"
)

// Marshal example (Go → JS conversion)
func ExampleMarshal() {
	type User struct {
		Name string `jsgo:"username"`
		Age  int    `jsgo:"userAge"`
	}

	u := User{Name: "Alice", Age: 30}
	jsVal, _ := jsgo.Marshal(u)
	fmt.Println(jsVal.Get("username").String())
	// Output: Alice
}

// Unmarshal example (JS → Go conversion)
func ExampleUnmarshal() {
	type User struct {
		Name string `jsgo:"username"`
		Age  int    `jsgo:"userAge"`
	}

	jsObj := js.Global().Get("Object").New()
	jsObj.Set("username", "Bob")
	jsObj.Set("userAge", 25)

	var result User
	jsgo.Unmarshal(jsObj, &result)
	fmt.Println(result.Name)
	// Output: Bob
}

// Function export and invocation
func ExampleFuncOfAny() {
	// Export Go function
	double := jsgo.FuncOfAny(func(x int) int {
		return x * 2
	})
	defer double.Release()

	js.Global().Set("double", double)

	// JavaScript equivalent:
	// console.log(double(4)) // Output: 8
}

// Class creation and inheritance
func ExampleTypeFor() {
	type Base struct {
		ID string `jsgo:"id"`
	}

	type User struct {
		Base     `jsgo:",extends=BaseClass"`
		Username string `jsgo:"username"`
	}

	// Export as JS class
	class, _ := jsgo.TypeFor[User](jsgo.ConstructorConfig{
		Fn: func(id, name string) *User {
			return &User{Base: Base{ID: id}, Username: name}
		},
	})
	defer class.Release()

	// JavaScript equivalent:
	// const user = new User("123", "alice");
	// console.log(user.id, user.username); // 123, alice
}
