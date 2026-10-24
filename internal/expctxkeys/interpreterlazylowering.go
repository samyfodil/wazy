package expctxkeys

// InterpreterLazyLowering is a context.Context Value key. Its associated value
// is a bool enabling lazy lowering of function bodies in the interpreter.
type InterpreterLazyLowering struct{}
