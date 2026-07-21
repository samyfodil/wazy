//go:build tinygo

package instance

import (
	"context"
	"net"
)

func defaultResolveIP(_ context.Context, name string) ([]net.IP, error) {
	return net.LookupIP(name)
}
