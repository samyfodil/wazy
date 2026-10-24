// Package binary decodes a core WebAssembly module -- a wasip1 program, say
// -- into its sections without a wazy.Runtime: decoding compiles and
// validates nothing.
//
//	m, err := binary.Decode(wasmBytes, 0) // e.g. from os.ReadFile("app.wasm")
//	if err != nil {
//		return err
//	}
//	for _, imp := range m.ImportSection {
//		fmt.Println("import", imp.Module, imp.Name)
//	}
//	for _, exp := range m.ExportSection {
//		fmt.Println("export", exp.Name)
//	}
//
// An import's or export's Type is an api.ExternType (api.ExternTypeFunc,
// ...). A func import's DescFunc indexes TypeSection.
//
// Every name here is an alias of the decoder wazy itself compiles from, so
// what this package reports is exactly what CompileModule reads. A component
// is not a core module: Decode rejects one with ErrInvalidVersion. Use
// package github.com/samyfodil/wazy/component/binary for those.
package binary

import (
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasm/binary"
)

// Decode decodes a core module, checking its encoding against the given
// features; 0 means api.CoreFeaturesV2, a runtime's default. Function bodies
// are decoded but not validated.
//
// It keeps custom sections (Module.CustomSections) and skips DWARF, and
// accepts any memory size the format can declare: a runtime's own memory
// limit is applied when it compiles the module, not here.
func Decode(b []byte, features api.CoreFeatures) (*Module, error) {
	if features == 0 {
		features = api.CoreFeaturesV2
	}
	return binary.DecodeModule(b, features, wasm.MemoryLimitPages, false, 0, false, true)
}

// Magic is the 4-byte preamble, "\0asm", that core modules and components
// share; the version that follows it is what tells them apart.
var Magic = binary.Magic

// The errors Decode returns for a malformed preamble or section.
var (
	ErrInvalidMagicNumber = binary.ErrInvalidMagicNumber
	ErrInvalidVersion     = binary.ErrInvalidVersion
	ErrInvalidSectionID   = binary.ErrInvalidSectionID
	ErrInvalidByte        = binary.ErrInvalidByte
)

// A decoded module and its parts.
type (
	Module             = wasm.Module
	Index              = wasm.Index
	ValueType          = wasm.ValueType
	FunctionType       = wasm.FunctionType
	Import             = wasm.Import
	Export             = wasm.Export
	Table              = wasm.Table
	Memory             = wasm.Memory
	GlobalType         = wasm.GlobalType
	Global             = wasm.Global
	Tag                = wasm.Tag
	Code               = wasm.Code
	DataSegment        = wasm.DataSegment
	ElementSegment     = wasm.ElementSegment
	ConstantExpression = wasm.ConstantExpression
	CustomSection      = wasm.CustomSection
	NameSection        = wasm.NameSection
)
