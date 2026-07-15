;; stdout_write_alias.wat is the "natural" cross-import-alias version of
;; stdout_write.wat's fixture: instead of test:cli/stdout declaring its own
;; independent local "output-stream" resource type (stdout_write.wat's
;; workaround for a pre-existing decoder gap), it references the SAME
;; nominal resource type aliased out of test:cli/streams -- exactly the
;; shape real WIT-generated WASI guests use to share a resource type across
;; interfaces (wasi:cli/stdout's get-stdout returns the same
;; wasi:io/streams#output-stream that wasi:io/streams's own methods take).
;;
;; This exercises two decoder fixes together:
;;
;;   - `(alias export $streams "output-stream" (type $ot_outer))` is a
;;     type-sort alias. It occupies an index in the component's TYPE index
;;     space, ahead of the func types declared later in the type section for
;;     "run"/"get-handle"/"write-handle"/"write-oob"'s canon lifts. Before
;;     Component.TypeSpace exdisted, those later canon lifts' TypeIdx values
;;     (which count this alias) pointed past binary.Component.Types (which
;;     only holds type-section deftypes), and instantiation failed loud with
;;     "lift references type N, out of range of M types". `wasm-tools print`
;;     on the compiled binary confirms canon lift "run" uses type_index=3,
;;     while the type SECTION alone (ignoring the alias) only has "run"'s
;;     func type at deftype index 2 -- exactly the off-by-one this fixture
;;     proves is fixed.
;;
;;   - `(alias core export $libci "memory" (core memory $mem))` is a
;;     core-export alias whose core:sort discriminator is memory (not func).
;;     AliasDef.CoreSort now carries that discriminator directly from the
;;     decoder, rather than internal/component/instance inferring func-vs-not
;;     by probing the instantiated module's exports.
;;
;; The generated own<$ot_outer>/borrow<$ot_outer> handle types still don't
;; get a shared, dereferenceable ResourceDesc across the two imports (this
;; decoder does not decode nested type declarations inside an imported
;; instance type -- see internal/component/instance's package doc), but that
;; is fine: per the Canonical ABI, own/borrow always flatten to a bare i32
;; handle regardless of the resource's structure, and this package never
;; dereferences an own/borrow's ResourceType index through the type
;; resolver -- it is only ever used as an opaque tag into the shared
;; resource handle table (see stdout_test.go's outputStreamResourceType).
(component
  (import "test:cli/streams" (instance $streams
    (export "output-stream" (type $ot (sub resource)))
    (export "[method]output-stream.write" (func (param "self" (borrow $ot)) (param "contents" (list u8))))))
  (alias export $streams "[method]output-stream.write" (func $write))
  (alias export $streams "output-stream" (type $ot_outer))

  (import "test:cli/stdout" (instance $stdout
    (export "get-stdout" (func (result (own $ot_outer))))))
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
  ;; (mirroring stdout_write.wat) so Go tests can drive specific error paths
  ;; -- an unknown/dropped handle, and an out-of-bounds list<u8> pointer --
  ;; through the real host-import boundary rather than only unit-testing the
  ;; helpers directly.
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
