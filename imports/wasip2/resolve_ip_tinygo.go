//go:build tinygo

package wasip2

import (
	"context"
	"net"
)

func defaultResolveIP(_ context.Context, name string) ([]net.IP, error) {
	return net.LookupIP(name)
}
