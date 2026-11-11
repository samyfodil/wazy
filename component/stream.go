package component

import "github.com/samyfodil/wazy/internal/component/instance"

// The host's ends of a stream<T> or future<T>. The host makes a stream with
// Instance.NewStream or NewStreamOf, keeping the writer and handing the guest
// the other value, as a call argument or an import's result. An import
// argument of a stream type arrives as a *StreamReader, and an export's
// stream result is a handle the host takes with Instance.TakeStream.
//
// Every operation runs on the instance's scheduler: between calls, inside a
// host import, or in an AsyncCall's Defer. Completion is a callback, not an
// event; a Write or Read pending on the guest completes from inside the
// guest's own stream builtin.
type (
	StreamWriter = instance.StreamWriter
	StreamReader = instance.StreamReader
	FutureWriter = instance.FutureWriter
	FutureReader = instance.FutureReader
)

// CopyResult is how a stream or future copy ended.
type CopyResult = instance.CopyResult

const (
	// CopyCompleted is a copy that transferred what it could.
	CopyCompleted = instance.CopyCompleted
	// CopyDropped is a copy whose other end was dropped: the stream is done.
	CopyDropped = instance.CopyDropped
	// CopyCancelled is a copy revoked by Cancel.
	CopyCancelled = instance.CopyCancelled
)

// ErrorContext is a WIT error-context, as the host gets one: an import's
// argument or an export's result. The host passes one, or a string to make
// one of, the other way.
type ErrorContext = instance.ErrorContext

// NewErrorContext is an error context of the host's, with message msg.
func NewErrorContext(msg string) *ErrorContext { return instance.NewErrorContext(msg) }
