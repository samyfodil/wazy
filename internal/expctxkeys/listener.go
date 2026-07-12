package expctxkeys

// FunctionListenerFactoryKey is a context.Context Value key.
// Its associated value should be a FunctionListenerFactory.
//
// See https://github.com/samyfodil/wazy/issues/451
type FunctionListenerFactoryKey struct{}
