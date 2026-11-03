;; f(n) re-enters itself through the host n times, so the outer call's operand
;; stack and frames must survive inner calls on the same handle.
(module
  (import "env" "reenter" (func $reenter (param i32) (result i32)))
  (func (export "f") (param i32) (result i32)
    (if (result i32) (i32.eqz (local.get 0))
      (then (i32.const 100))
      (else (i32.add (call $reenter (i32.sub (local.get 0) (i32.const 1))) (i32.const 1)))))
  (func (export "trap") unreachable)
)
