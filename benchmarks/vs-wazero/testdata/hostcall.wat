;; hostcall.wat is the guest module used by BenchmarkHostCall.
;;
;; It mirrors the hand-encoded module built in
;; internal/integration_test/bench/hostfunc_bench_test.go via
;; binaryencoding.EncodeModule. Two exported functions each forward their
;; single i32 argument to an imported host function and return the f32 result:
;;
;;   call_go_host       -> host.go        (registered via WithGoModuleFunction)
;;   call_go_typed_host -> host.go-typed  (wazy: HostFunc1 / wazero: WithFunc)
;;
;; The host function reads 4 bytes at the given offset from guest memory and
;; returns them as an f32. The SAME compiled bytes (hostcall.wasm) are fed to
;; both runtimes so the comparison isolates runtime behaviour, not the module.
(module
  (type (;0;) (func (param i32) (result f32)))
  (import "host" "go"        (func (;0;) (type 0)))
  (import "host" "go-typed"  (func (;1;) (type 0)))
  (func (;2;) (type 0) (param i32) (result f32)
    local.get 0
    call 0)
  (func (;3;) (type 0) (param i32) (result f32)
    local.get 0
    call 1)
  (memory (;0;) 1)
  (export "call_go_host"       (func 2))
  (export "call_go_typed_host" (func 3)))
