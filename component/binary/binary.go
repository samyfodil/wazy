// Package binary decodes a WebAssembly component binary -- its imports,
// exports, types, embedded core modules and instantiation graph -- without a
// wazy.Runtime: decoding is pure and compiles nothing.
//
//	c, err := binary.Decode(wasmBytes) // e.g. from os.ReadFile("app.wasm")
//	if err != nil {
//		return err
//	}
//	for _, imp := range c.Imports {
//		fmt.Println("import", imp.Name)
//	}
//	for _, exp := range c.Exports {
//		fmt.Println("export", exp.Name)
//	}
//
// An import's or export's ExternType is the sort byte of the binary format:
// 0x00 core module, 0x01 func, 0x02 value, 0x03 type, 0x04 component, 0x05
// instance. For a func, Component.ResolveType(ExternIndex) gives its
// component.FuncDesc; for an instance, its InstanceDesc.
//
// Every name here is an alias of the decoder wazy itself instantiates from,
// so what this package reports is exactly what Instantiate sees. The value
// type descriptors (component.FuncDesc, component.RecordDesc, ...) live in
// package component.
//
// A core wasm module is not a component: Decode rejects one with
// ErrInvalidVersion. Use package github.com/samyfodil/wazy/binary for those.
package binary

import "github.com/samyfodil/wazy/internal/component/binary"

// Decode decodes a component binary.
func Decode(b []byte) (*Component, error) {
	return binary.DecodeBytes(b)
}

// The errors Decode returns for a malformed preamble or section.
var (
	ErrInvalidMagicNumber = binary.ErrInvalidMagicNumber
	ErrInvalidVersion     = binary.ErrInvalidVersion
	ErrInvalidLayer       = binary.ErrInvalidLayer
	ErrInvalidSectionID   = binary.ErrInvalidSectionID
	ErrTruncatedBinary    = binary.ErrTruncatedBinary
)

// A decoded component and its parts.
type (
	Component          = binary.Component
	Type               = binary.Type
	Import             = binary.Import
	Export             = binary.Export
	CoreModule         = binary.CoreModule
	CoreInstance       = binary.CoreInstance
	CoreInstantiateArg = binary.CoreInstantiateArg
	CoreInlineExport   = binary.CoreInlineExport
	Instance           = binary.Instance
	InstantiateArg     = binary.InstantiateArg
	InlineExport       = binary.InlineExport
	Canon              = binary.Canon
	CanonOpt           = binary.CanonOpt
	AliasDef           = binary.AliasDef
	Start              = binary.Start
	RawSection         = binary.RawSection
)

// The type descriptors package component does not already name.
type (
	InstanceDesc  = binary.InstanceDesc
	ComponentDesc = binary.ComponentDesc
	ResourceDesc  = binary.ResourceDesc
)

// The declarations of an instance or component type, its Decls field,
// and what an import, an export's ascription, or a declaration says an item
// is.
type (
	TypeDecl     = binary.TypeDecl
	TypeDeclKind = binary.TypeDeclKind
	ExternDesc   = binary.ExternDesc
)

// The declaration kinds, TypeDecl.Kind.
const (
	TypeDeclCoreType = binary.TypeDeclCoreType
	TypeDeclType     = binary.TypeDeclType
	TypeDeclAlias    = binary.TypeDeclAlias
	TypeDeclImport   = binary.TypeDeclImport
	TypeDeclExport   = binary.TypeDeclExport
)

// The index spaces: each entry says what produced the item at that index.
type (
	TypeSpaceEntry                  = binary.TypeSpaceEntry
	ComponentFuncSpaceEntry         = binary.ComponentFuncSpaceEntry
	ComponentInstanceSpaceEntry     = binary.ComponentInstanceSpaceEntry
	ComponentSpaceEntry             = binary.ComponentSpaceEntry
	CoreFuncSpaceEntry              = binary.CoreFuncSpaceEntry
	CoreModuleSpaceEntry            = binary.CoreModuleSpaceEntry
	TypeSpaceEntryKind              = binary.TypeSpaceEntryKind
	ComponentFuncSpaceEntryKind     = binary.ComponentFuncSpaceEntryKind
	ComponentInstanceSpaceEntryKind = binary.ComponentInstanceSpaceEntryKind
	ComponentSpaceEntryKind         = binary.ComponentSpaceEntryKind
	CoreFuncSpaceEntryKind          = binary.CoreFuncSpaceEntryKind
	CoreModuleSpaceEntryKind        = binary.CoreModuleSpaceEntryKind
)

// The index-space entry kinds.
const (
	TypeSpaceDef    = binary.TypeSpaceDef
	TypeSpaceAlias  = binary.TypeSpaceAlias
	TypeSpaceImport = binary.TypeSpaceImport
	TypeSpaceExport = binary.TypeSpaceExport

	ComponentFuncFromImport    = binary.ComponentFuncFromImport
	ComponentFuncFromAlias     = binary.ComponentFuncFromAlias
	ComponentFuncFromCanonLift = binary.ComponentFuncFromCanonLift
	ComponentFuncFromExport    = binary.ComponentFuncFromExport

	ComponentInstanceFromImport     = binary.ComponentInstanceFromImport
	ComponentInstanceFromDefinition = binary.ComponentInstanceFromDefinition
	ComponentInstanceFromAlias      = binary.ComponentInstanceFromAlias
	ComponentInstanceFromExport     = binary.ComponentInstanceFromExport

	ComponentFromImport     = binary.ComponentFromImport
	ComponentFromDefinition = binary.ComponentFromDefinition
	ComponentFromAlias      = binary.ComponentFromAlias
	ComponentFromExport     = binary.ComponentFromExport

	CoreFuncFromAlias = binary.CoreFuncFromAlias
	CoreFuncFromCanon = binary.CoreFuncFromCanon

	CoreModuleFromDefinition = binary.CoreModuleFromDefinition
	CoreModuleFromAlias      = binary.CoreModuleFromAlias
)

// The canon definition kinds, Canon.Kind.
const (
	CanonKindLift                     = binary.CanonKindLift
	CanonKindLower                    = binary.CanonKindLower
	CanonKindResourceNew              = binary.CanonKindResourceNew
	CanonKindResourceDrop             = binary.CanonKindResourceDrop
	CanonKindResourceRep              = binary.CanonKindResourceRep
	CanonKindResourceDropAsync        = binary.CanonKindResourceDropAsync
	CanonKindTaskCancel               = binary.CanonKindTaskCancel
	CanonKindSubtaskCancel            = binary.CanonKindSubtaskCancel
	CanonKindTaskReturn               = binary.CanonKindTaskReturn
	CanonKindContextGet               = binary.CanonKindContextGet
	CanonKindContextSet               = binary.CanonKindContextSet
	CanonKindThreadYield              = binary.CanonKindThreadYield
	CanonKindSubtaskDrop              = binary.CanonKindSubtaskDrop
	CanonKindStreamNew                = binary.CanonKindStreamNew
	CanonKindStreamRead               = binary.CanonKindStreamRead
	CanonKindStreamWrite              = binary.CanonKindStreamWrite
	CanonKindStreamCancelRead         = binary.CanonKindStreamCancelRead
	CanonKindStreamCancelWrite        = binary.CanonKindStreamCancelWrite
	CanonKindStreamDropReadable       = binary.CanonKindStreamDropReadable
	CanonKindStreamDropWritable       = binary.CanonKindStreamDropWritable
	CanonKindFutureNew                = binary.CanonKindFutureNew
	CanonKindFutureRead               = binary.CanonKindFutureRead
	CanonKindFutureWrite              = binary.CanonKindFutureWrite
	CanonKindFutureCancelRead         = binary.CanonKindFutureCancelRead
	CanonKindFutureCancelWrite        = binary.CanonKindFutureCancelWrite
	CanonKindFutureDropReadable       = binary.CanonKindFutureDropReadable
	CanonKindFutureDropWritable       = binary.CanonKindFutureDropWritable
	CanonKindErrorContextNew          = binary.CanonKindErrorContextNew
	CanonKindErrorContextDebugMessage = binary.CanonKindErrorContextDebugMessage
	CanonKindErrorContextDrop         = binary.CanonKindErrorContextDrop
	CanonKindWaitableSetNew           = binary.CanonKindWaitableSetNew
	CanonKindWaitableSetWait          = binary.CanonKindWaitableSetWait
	CanonKindWaitableSetPoll          = binary.CanonKindWaitableSetPoll
	CanonKindWaitableSetDrop          = binary.CanonKindWaitableSetDrop
	CanonKindWaitableJoin             = binary.CanonKindWaitableJoin
	CanonKindBackpressureInc          = binary.CanonKindBackpressureInc
	CanonKindBackpressureDec          = binary.CanonKindBackpressureDec
	CanonKindThreadIndex              = binary.CanonKindThreadIndex
	CanonKindThreadNewIndirect        = binary.CanonKindThreadNewIndirect
	CanonKindThreadSuspend            = binary.CanonKindThreadSuspend
	CanonKindThreadYieldThenResume    = binary.CanonKindThreadYieldThenResume
)
