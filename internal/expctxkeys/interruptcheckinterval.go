package expctxkeys

// InterruptCheckInterval is a context.Context Value key. Its associated value
// should be a uint64: the loop interrupt-check interval used when a module is
// compiled under RuntimeConfig.WithCloseOnContextDone. 0 means check on every
// loop iteration; any other value must be a power of two and means check only
// once every N iterations.
type InterruptCheckInterval struct{}
