package internalapi

type WazyOnly interface {
	wazyOnly()
}

type WazyOnlyType struct{}

func (WazyOnlyType) wazyOnly() {}
