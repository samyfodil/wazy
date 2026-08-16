package http_handler

import (
	"context"

	"github.com/samyfodil/wazy"
)

// Option is configuration for NewMiddleware.
//
// # Porting from http-wasm-host-go
//
// Upstream splits these across two packages, which lets it name an option
// Logger while api.Logger is an interface. This package is flat, so the
// options take the conventional With prefix:
//
//	handler.Runtime      -> http_handler.WithRuntime
//	handler.GuestConfig  -> http_handler.WithGuestConfig
//	handler.ModuleConfig -> http_handler.WithModuleConfig
//	handler.Logger       -> http_handler.WithLogger
type Option func(*options)

// NewRuntime returns a new wazy runtime, called when creating a middleware
// instance, which also closes it.
type NewRuntime func(context.Context) (wazy.Runtime, error)

// WithRuntime provides the wazy.Runtime and defaults to DefaultRuntime.
func WithRuntime(newRuntime NewRuntime) Option {
	return func(h *options) {
		h.newRuntime = newRuntime
	}
}

// WithGuestConfig is the configuration the guest reads with FuncGetConfig.
func WithGuestConfig(guestConfig []byte) Option {
	return func(h *options) {
		h.guestConfig = guestConfig
	}
}

// WithModuleConfig is the configuration used to instantiate the guest.
func WithModuleConfig(moduleConfig wazy.ModuleConfig) Option {
	return func(h *options) {
		h.moduleConfig = moduleConfig
	}
}

// WithLogger sets the logger used by the guest when it calls FuncLog.
// Defaults to NoopLogger.
func WithLogger(logger Logger) Option {
	return func(h *options) {
		h.logger = logger
	}
}

type options struct {
	newRuntime   NewRuntime
	guestConfig  []byte
	moduleConfig wazy.ModuleConfig
	logger       Logger
}

// DefaultRuntime implements options.newRuntime.
func DefaultRuntime(ctx context.Context) (wazy.Runtime, error) {
	return wazy.NewRuntime(ctx), nil
}
