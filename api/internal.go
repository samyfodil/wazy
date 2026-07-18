package api

// InternalModule is an api.Module that exposes additional
// information.
type InternalModule interface {
	Module

	// NumGlobal returns the count of all globals in the module.
	NumGlobal() int

	// Global provides a read-only view for a given global index.
	//
	// The methods panics if i is out of bounds.
	Global(i int) Global
}

// ProgramCounter is an opaque value representing a specific execution point in
// a module. It is meant to be used with Function.SourceOffsetForPC and
// StackIterator.
type ProgramCounter uint64

// InternalFunction exposes some information about a function instance.
type InternalFunction interface {
	// Definition provides introspection into the function's names and
	// signature.
	Definition() FunctionDefinition

	// SourceOffsetForPC resolves a program counter into its corresponding
	// offset in the Code section of the module this function belongs to.
	// The source offset is meant to help map the function calls to their
	// location in the original source files. Returns 0 if the offset cannot
	// be calculated.
	SourceOffsetForPC(pc ProgramCounter) uint64
}
