package http_handler

import (
	"context"
	"fmt"
)

// LogLevel controls the volume of logging. The lower the number the more
// detail is logged.
//
// Note: The most voluminous level, LogLevelDebug is -1 to prevent users from
// accidentally defaulting to it.
type LogLevel int32

const (
	LogLevelDebug LogLevel = -1
	LogLevelInfo  LogLevel = 0
	LogLevelWarn  LogLevel = 1
	LogLevelError LogLevel = 2
	LogLevelNone  LogLevel = 3
)

type Logger interface {
	// IsEnabled returns true if the input level would be logged.
	//
	// Note: Guests may cache this value at request scope.
	IsEnabled(LogLevel) bool

	// Log logs a message to the host's logs.
	Log(context.Context, LogLevel, string)
}

// compile-time check to ensure NoopLogger implements Logger.
var _ Logger = NoopLogger{}

// NoopLogger is a convenience which ignores all output.
type NoopLogger struct{}

// IsEnabled implements the same method as documented on Logger.
func (NoopLogger) IsEnabled(level LogLevel) bool {
	return level == LogLevelNone
}

// Log implements the same method as documented on Logger.
func (NoopLogger) Log(context.Context, LogLevel, string) {}

// compile-time check to ensure ConsoleLogger implements Logger.
var _ Logger = ConsoleLogger{}

// ConsoleLogger is a convenience which writes anything above LogLevelInfo to
// os.Stdout.
type ConsoleLogger struct{}

// IsEnabled implements the same method as documented on Logger.
func (ConsoleLogger) IsEnabled(level LogLevel) bool {
	return level >= LogLevelInfo
}

// Log implements the same method as documented on Logger.
func (ConsoleLogger) Log(_ context.Context, level LogLevel, message string) {
	if level >= LogLevelInfo {
		fmt.Println(message)
	}
}
