package main

import (
	"syscall/js"

	"github.com/veecore/jsgo"

	"google.golang.org/protobuf/proto"
)

// -go:generate protoc --go_out=. model.proto

func main() {
	js.Global().Set("protoEncode", js.FuncOf(protoEncode))
	select {}
}

func protoEncode(this js.Value, args []js.Value) interface{} {
	// Unmarshal from JS to Go struct
	var product = new(Product)
	if err := jsgo.Unmarshal(args[0], product); err != nil {
		return jsgo.Throw(err)
	}

	// Marshal to protobuf bytes
	data, err := proto.Marshal(product)
	if err != nil {
		return jsgo.Throw(err)
	}

	return jsgo.Bytes(data)
}
