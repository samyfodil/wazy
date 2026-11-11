;; A sync export whose result holds a stream and an error context below its
;; top level: pair returns the readable end of a stream it has already
;; closed, and an error context saying "nested".
(component
  (type $st (stream u32))
  (core module $mh (memory (export "mem") 1))
  (core instance $mi (instantiate $mh))
  (alias core export $mi "mem" (core memory $mem))
  (core module $m
    (import "canon" "stream.new" (func $sn (result i64)))
    (import "canon" "stream.drop-writable" (func $sdw (param i32)))
    (import "canon" "error-context.new" (func $ecn (param i32 i32) (result i32)))
    (import "mem" "mem" (memory 1))
    (data (i32.const 0) "nested")
    (func (export "pair") (result i32)
      (local $p i64)
      (local.set $p (call $sn))
      (call $sdw (i32.wrap_i64 (i64.shr_u (local.get $p) (i64.const 32))))
      (i32.store (i32.const 16) (i32.wrap_i64 (local.get $p)))
      (i32.store (i32.const 20) (call $ecn (i32.const 0) (i32.const 6)))
      (i32.const 16)))
  (canon stream.new $st (core func $sn))
  (canon stream.drop-writable $st (core func $sdw))
  (canon error-context.new (memory $mem) (core func $ecn))
  (core instance $i (instantiate $m
    (with "mem" (instance $mi))
    (with "canon" (instance
      (export "stream.new" (func $sn))
      (export "stream.drop-writable" (func $sdw))
      (export "error-context.new" (func $ecn))))))
  (alias core export $i "pair" (core func $pair))
  (type $t (tuple $st error-context))
  (type $ft (func (result $t)))
  (canon lift (core func $pair) (memory $mem) (func $lifted (type $ft)))
  (export "pair" (func $lifted))
)
