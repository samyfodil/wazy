;; Definitions are instantiated in the order the binary gives them, core and
;; component instances alike. Each start function logs a digit through the
;; host: core instance $a logs 1, the nested component instance $c 2, and core
;; instance $b, defined after $c, 3 -- so the host sees 1, 2, 3.
(component
  (import "test:order/log" (instance $log
    (export "log" (func (param "d" u32)))))
  (alias export $log "log" (func $logf))
  (core func $log' (canon lower (func $logf)))
  (core instance $logi (export "log" (func $log')))

  (core module $One
    (import "host" "log" (func $log (param i32)))
    (func $start (call $log (i32.const 1)))
    (start $start))
  (core instance $a (instantiate $One (with "host" (instance $logi))))

  (component $C
    (import "test:order/log" (instance $log
      (export "log" (func (param "d" u32)))))
    (alias export $log "log" (func $logf))
    (core func $log' (canon lower (func $logf)))
    (core module $Two
      (import "host" "log" (func $log (param i32)))
      (func $start (call $log (i32.const 2)))
      (start $start))
    (core instance (instantiate $Two
      (with "host" (instance (export "log" (func $log')))))))
  (instance $c (instantiate $C (with "test:order/log" (instance $log))))

  (core module $Three
    (import "host" "log" (func $log (param i32)))
    (func $start (call $log (i32.const 3)))
    (start $start))
  (core instance $b (instantiate $Three (with "host" (instance $logi)))))
