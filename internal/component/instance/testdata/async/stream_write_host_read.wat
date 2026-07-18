;; Phase 2 acceptance fixture (a): a guest that WRITES a stream<u8> the host
;; reads via a host StreamReader (docs/component-model-async-phase2-design.md
;; §3.4 "Guest hands the host a stream"). Entirely synchronous -- the host's
;; StreamReader.Read parks BEFORE the guest calls stream.write, so the
;; rendezvous completes inline inside stream.write; no callback/async lift is
;; needed to exercise the copy machinery end to end.
(component
  (type $st (stream u8))
  (import "sink" (func $sink_import (param "s" $st)))
  (canon lower (func $sink_import) (core func $lowered_sink))

  (core module $m
    (import "canon" "stream_new" (func $stream_new (result i64)))
    (import "canon" "stream_write" (func $stream_write (param i32 i32 i32) (result i32)))
    (import "canon" "sink" (func $sink (param i32)))
    (memory (export "mem") 1)
    (data (i32.const 0) "\01\02\03\04")
    (func (export "run") (result i32)
      (local $packed i64) (local $readable i32) (local $writable i32)
      (local.set $packed (call $stream_new))
      (local.set $readable (i32.wrap_i64 (local.get $packed)))
      (local.set $writable (i32.wrap_i64 (i64.shr_u (local.get $packed) (i64.const 32))))
      (call $sink (local.get $readable))
      (call $stream_write (local.get $writable) (i32.const 0) (i32.const 4))))

  (canon stream.new $st (core func $stream_new))
  (canon stream.write $st (core func $stream_write))

  (core instance $i (instantiate $m (with "canon" (instance
    (export "stream_new" (func $stream_new))
    (export "stream_write" (func $stream_write))
    (export "sink" (func $lowered_sink))))))

  (alias core export $i "run" (core func $run))
  (alias core export $i "mem" (core memory $mem))
  (type $ft (func (result u32)))
  (canon lift (core func $run) (memory $mem) (func $lifted (type $ft)))
  (export "run" (func $lifted))
)
