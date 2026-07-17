//go:build plan9

package instance

import "errors"

// Plan 9 has no BSD-socket errno constants. These sentinels never match a real
// net error, so the wasi:sockets mapping just falls back to the unknown code —
// fine, since Plan 9 is a compile-only target for this package.
var (
	errConnRefused  = errors.New("connection refused")
	errAddrInUse    = errors.New("address in use")
	errAddrNotAvail = errors.New("address not available")
)
