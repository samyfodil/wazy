package binary_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/component/binary"
	"github.com/samyfodil/wazy/internal/component/testfixtures"
)

func TestDecode_RealComponent(t *testing.T) {
	c, err := binary.Decode(testfixtures.RealHello)
	if err != nil {
		t.Fatal(err)
	}
	var stdout *binary.Import
	for i := range c.Imports {
		if c.Imports[i].Name == "wasi:cli/stdout@0.2.3" {
			stdout = &c.Imports[i]
		}
	}
	if stdout == nil || stdout.ExternType != 0x05 {
		t.Fatalf("no instance import of wasi:cli/stdout@0.2.3 in %+v", c.Imports)
	}
	desc, err := c.ResolveType(stdout.ExternIndex)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := desc.(binary.InstanceDesc); !ok {
		t.Errorf("stdout import resolves to %T, want an InstanceDesc", desc)
	}
	if len(c.Exports) != 1 || c.Exports[0].Name != "wasi:cli/run@0.2.3" {
		t.Errorf("exports %+v, want wasi:cli/run@0.2.3", c.Exports)
	}
	if len(c.CoreModules) == 0 {
		t.Error("no embedded core modules")
	}
}

func TestDecode_Rejects(t *testing.T) {
	core := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if _, err := binary.Decode(core); !errors.Is(err, binary.ErrInvalidVersion) {
		t.Errorf("core module: err %v, want ErrInvalidVersion", err)
	}
	if _, err := binary.Decode([]byte("not wasm")); !errors.Is(err, binary.ErrInvalidMagicNumber) {
		t.Errorf("garbage: err %v, want ErrInvalidMagicNumber", err)
	}
}

// Decode a component and list what it imports and exports.
func Example() {
	wasm := testfixtures.RealHello // the bytes of a .wasm component

	c, err := binary.Decode(wasm)
	if err != nil {
		panic(err)
	}
	for _, imp := range c.Imports {
		fmt.Println("import", imp.Name)
	}
	for _, exp := range c.Exports {
		fmt.Println("export", exp.Name)
	}
	// Output:
	// import wasi:cli/environment@0.2.3
	// import wasi:cli/exit@0.2.3
	// import wasi:io/error@0.2.3
	// import wasi:io/streams@0.2.3
	// import wasi:cli/stdin@0.2.3
	// import wasi:cli/stdout@0.2.3
	// import wasi:cli/stderr@0.2.3
	// import wasi:clocks/wall-clock@0.2.3
	// import wasi:filesystem/types@0.2.3
	// import wasi:filesystem/preopens@0.2.3
	// export wasi:cli/run@0.2.3
}
