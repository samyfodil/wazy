//go:build !tinygo

package wasip2

import (
	"context"
	"net"
)

func defaultResolveIP(ctx context.Context, name string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", name)
}
