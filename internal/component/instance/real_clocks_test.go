package instance

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
)

// real_clocks.component.wasm is a genuine rustc wasm32-wasip2 component built
// from:
//
//	let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap();
//	println!("unix_secs={}", now.as_secs());
//	let start = Instant::now();
//	std::thread::sleep(Duration::from_millis(50));
//	println!("slept_at_least_50ms={}", start.elapsed().as_millis() >= 50);
//
// It exercises wasi:clocks/wall-clock.now (SystemTime), wasi:clocks/
// monotonic-clock.now + subscribe-duration, and wasi:io/poll.poll (the
// std::thread::sleep path: subscribe-duration mints a timer pollable, poll
// blocks on it). Confirmed under `wasmtime run` to print
// unix_secs=<real time> / slept_at_least_50ms=true before wazy's clocks host
// existed.
//
//go:embed testdata/real_clocks.component.wasm
var realClocksWasm []byte

// TestRealClocks proves wazy's wasi:clocks + timer-aware wasi:io/poll host.
// The wall clock is pinned to a fixed instant (WASIConfig.WallClock) so the
// guest's printed unix_secs is deterministically assertable; the monotonic
// clock stays real, so the guest's 50ms sleep genuinely elapses (poll blocks
// on the real timer deadline) and slept_at_least_50ms is true. Two fixed times
// prove the wall value flows from the injected clock, not a constant.
func TestRealClocks(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
	}{
		{name: "epoch_plus", when: time.Unix(1_700_000_000, 0)},
		{name: "different_instant", when: time.Unix(1_234_567_890, 0)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			var stdout, stderr bytes.Buffer
			start := time.Now()
			inst, err := Instantiate(ctx, r, realClocksWasm, WithWASI(WASIConfig{
				Stdout:    &stdout,
				Stderr:    &stderr,
				WallClock: func() time.Time { return tc.when },
			})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
				t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
			}
			// The guest's sleep must have really elapsed (proves poll blocked
			// on the real timer, not a no-op).
			if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
				t.Fatalf("guest returned in %v, expected the real 50ms sleep to elapse", elapsed)
			}

			wantSecs := fmt.Sprintf("unix_secs=%d\n", tc.when.Unix())
			if out := stdout.String(); out != wantSecs+"slept_at_least_50ms=true\n" {
				t.Fatalf("guest stdout = %q, want %q + slept_at_least_50ms=true (stderr: %q)", out, wantSecs, stderr.String())
			}
		})
	}
}
