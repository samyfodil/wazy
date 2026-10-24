package instance

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// A fixed-length list holds what its element holds: a borrow in one is a
// borrow in a stream or future element, as in a dynamic list.
func TestElemContainsBorrowHandle_FixedList(t *testing.T) {
	descs := []binary.TypeDesc{binary.BorrowDesc{ResourceType: 1}}
	resolve := func(i uint32) binary.TypeDesc { return descs[i] }
	zero := uint32(0)
	if !elemContainsBorrowHandle(binary.FixedListDesc{Element: binary.TypeRef{TypeIndex: &zero}, Length: 2}, resolve, 0) {
		t.Error("a fixed-length list of borrows holds a borrow")
	}
	if elemContainsBorrowHandle(binary.FixedListDesc{Element: binary.TypeRef{Primitive: "u32"}, Length: 2}, resolve, 0) {
		t.Error("a fixed-length list of u32 holds no borrow")
	}
}
