(component
  ;; test:cli/streams: resource output-stream + [method]output-stream.write.
  ;; The resource type is local to this import's instance type (an abstract,
  ;; host-owned resource -- no rep is declared, since the component itself
  ;; never sees the host representation, only opaque i32 handles).
  (import "test:cli/streams" (instance $streams
    (export "output-stream" (type $ot (sub resource)))
    (export "[method]output-stream.write" (func (param "self" (borrow $ot)) (param "contents" (list u8))))))
  (alias export $streams "[method]output-stream.write" (func $write))

  ;; test:cli/stdout: get-stdout -> own<output-stream>. This declares its own
  ;; local "output-stream" resource type rather than aliasing $streams's --
  ;; see internal/component/instance's package doc on decoder gaps: this
  ;; package does not decode nested types inside an imported instance type
  ;; (or type-sort aliases into the outer type index space), so the two
  ;; imports' own/borrow signatures come from the Go-side WithImport call,
  ;; not from the binary. The two host funcs are tied to the *same* host
  ;; stream via a shared resource-type tag on the Go side (see stdout_test.go).
  (import "test:cli/stdout" (instance $stdout
    (export "output-stream" (type $ot2 (sub resource)))
    (export "get-stdout" (func (result (own $ot2))))))
  (alias export $stdout "get-stdout" (func $get_stdout))

  ;; memory-owning core module (no imports) -- breaks the lower/main cycle
  (core module $libc
    (memory (export "memory") 1)
    (data (i32.const 8) "hello world"))
  (core instance $libci (instantiate $libc))
  (alias core export $libci "memory" (core memory $mem))

  (core func $write_lowered (canon lower (func $write) (memory $mem)))
  (core func $get_stdout_lowered (canon lower (func $get_stdout)))
  (core instance $hostci
    (export "write" (func $write_lowered))
    (export "get-stdout" (func $get_stdout_lowered)))

  ;; main module: exercises the happy path ("run") plus fine-grained exports
  ;; (mirroring resource_roundtrip.wat's new/rep/drop split) so Go tests can
  ;; drive specific error paths -- an unknown/dropped handle, and an
  ;; out-of-bounds list<u8> pointer -- through the real host-import boundary
  ;; rather than only unit-testing the helpers directly.
  (core module $main
    (import "host" "write" (func $write_imp (param i32 i32 i32)))
    (import "host" "get-stdout" (func $get_stdout_imp (result i32)))
    (import "libc" "memory" (memory 1))

    (func (export "run")
      (local $h i32)
      (local.set $h (call $get_stdout_imp))
      (call $write_imp (local.get $h) (i32.const 8) (i32.const 11)))

    (func (export "get-handle") (result i32)
      (call $get_stdout_imp))

    (func (export "write-handle") (param $h i32)
      (call $write_imp (local.get $h) (i32.const 8) (i32.const 11)))

    (func (export "write-oob") (param $h i32)
      (call $write_imp (local.get $h) (i32.const 999999) (i32.const 11))))

  (core instance $mainci (instantiate $main
    (with "host" (instance $hostci))
    (with "libc" (instance $libci))))

  (func $run (canon lift (core func $mainci "run")))
  (func $get_handle (result u32) (canon lift (core func $mainci "get-handle")))
  (func $write_handle (param "h" u32) (canon lift (core func $mainci "write-handle")))
  (func $write_oob (param "h" u32) (canon lift (core func $mainci "write-oob")))

  (export "run" (func $run))
  (export "get-handle" (func $get_handle))
  (export "write-handle" (func $write_handle))
  (export "write-oob" (func $write_oob)))
