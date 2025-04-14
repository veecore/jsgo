const fs = require('fs');
const vm = require('vm');
const path = require('path');
const process = require('process');


const wasmExecCode = fs.readFileSync(path.join(process.env.GOROOT, "wasm_exec.js"), 'utf8');
vm.runInThisContext(wasmExecCode);
const go = new Go();

const wasmBuffer = fs.readFileSync('example');

WebAssembly.instantiate(wasmBuffer, go.importObject).then(async (result) => {
  go.run(result.instance);

  let product = {
	"Name": "Satchel Bag",
	"Description": "High-quality fashionable bag",
	"BasePrice": 100000,
	"Currency": "USD",
	"Variations": [{"color": "green"}],
	"Image": new Uint8Array(1024*1024),
};


try {
	var started = Date.now();
	const protoEncodedProduct = global.protoEncode(product);
	var spent = Date.now() - started;
	const jsonEncodedProduct = JSON.stringify(product);
	console.log("protoEncodedProduct.length: %d\njsonEncodedProduct.length: %d", 
	protoEncodedProduct.length, jsonEncodedProduct.length);
	
	if (jsonEncodedProduct.length <= protoEncodedProduct.length) {
		console.log("'wasn't worth it.");
	}else {
		let r = jsonEncodedProduct.length / protoEncodedProduct.length;
		console.log("we cut down data by %dx in %d ms", r, spent);
	}
	
	// Send here
}catch (err) {
	console.log("Error: ", err);
}
});


