package wasip2

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// TestRealSockets_CancelWhileBlocked runs real guests until each blocks on a peer that never answers,
// and requires the call to end with its context: a blocking socket operation must not outlive it.
func TestRealSockets_CancelWhileBlocked(t *testing.T) {
	cases := []struct {
		name  string
		guest []byte
		cfg   func(t *testing.T) WASIConfig
	}{
		{
			name:  "udp receive",
			guest: realUDPWasm,
			cfg: func(t *testing.T) WASIConfig {
				conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				return WASIConfig{AllowUDP: true, Args: []string{conn.LocalAddr().String(), "never answered"}}
			},
		},
		{
			name:  "tcp read",
			guest: realTCPWasm,
			cfg: func(t *testing.T) WASIConfig {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { ln.Close() })
				go func() {
					if conn, err := ln.Accept(); err == nil {
						t.Cleanup(func() { conn.Close() })
					}
				}()
				return WASIConfig{AllowTCP: true, Args: []string{ln.Addr().String(), "never answered"}}
			},
		},
		{
			name:  "tcp connect",
			guest: realTCPWasm,
			cfg: func(t *testing.T) WASIConfig {
				release := make(chan struct{})
				t.Cleanup(func() { close(release) })
				dial := func(string, string) (net.Conn, error) {
					<-release
					return nil, errors.New("released")
				}
				return WASIConfig{AllowTCP: true, Dialer: dial, Args: []string{"127.0.0.1:9", "never sent"}}
			},
		},
		{
			name:  "tcp accept",
			guest: realTCPListenWasm,
			cfg: func(t *testing.T) WASIConfig {
				return WASIConfig{AllowTCP: true}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := wazy.NewRuntime(context.Background())
			defer r.Close(context.Background())
			inst, err := component.Instantiate(context.Background(), r, tc.guest, WithWASI(tc.cfg(t))...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(context.Background())

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run")
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Call run() = %v, want the context's deadline", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("Call run() still blocked 30s after its context ended")
			}
		})
	}
}
