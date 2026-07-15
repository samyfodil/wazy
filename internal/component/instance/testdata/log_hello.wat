(component
  (import "test:pkg/host" (instance $h
    (export "log" (func (param "msg" string)))))
  (alias export $h "log" (func $log))

  ;; memory-owning core module (no imports) — breaks the lower/main cycle
  (core module $libc
    (memory (export "memory") 1)
    (data (i32.const 8) "hello"))
  (core instance $libci (instantiate $libc))
  (alias core export $libci "memory" (core memory $mem))

  ;; lower the imported component func into a core func reading from $mem
  (core func $log_lowered (canon lower (func $log) (memory $mem)))
  (core instance $hostci
    (export "log" (func $log_lowered)))

  ;; main module: imports lowered log + shared memory, calls log(ptr,len)
  (core module $main
    (import "test:pkg/host" "log" (func $log_imp (param i32 i32)))
    (import "libc" "memory" (memory 1))
    (func (export "run")
      i32.const 8
      i32.const 5
      call $log_imp))
  (core instance $mainci (instantiate $main
    (with "test:pkg/host" (instance $hostci))
    (with "libc" (instance $libci))))

  (func $run (canon lift (core func $mainci "run")))
  (export "run" (func $run)))
