;; Loops of the shapes the compiler turns into vector map loops (see the native frontend's
;; vecmap_plan.go), run against the interpreter by TestVecMap. Each takes byte addresses and an element
;; count, so the test chooses aliasing and bounds.
(module
  (memory (export "memory") 2)

  ;; dst[i] = a[i] * s + b[i], f64, counting i up to n by lt_s.
  (func (export "axpy") (param $dst i32) (param $a i32) (param $b i32) (param $n i32) (param $s f64)
    (local $i i32)
    (loop $l
      (f64.store
        (i32.add (local.get $dst) (i32.shl (local.get $i) (i32.const 3)))
        (f64.add
          (f64.mul (f64.load (i32.add (local.get $a) (i32.shl (local.get $i) (i32.const 3)))) (local.get $s))
          (f64.load (i32.add (local.get $b) (i32.shl (local.get $i) (i32.const 3))))))
      (br_if $l (i32.lt_s (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n)))))

  ;; p[i] = sqrt(abs(p[i])) / 2 - (-p[i]), f32 in place, the counter counting down to zero by ne.
  (func (export "inplace32") (param $p i32) (param $n i32)
    (loop $l
      (f32.store (local.get $p)
        (f32.sub
          (f32.div (f32.sqrt (f32.abs (f32.load (local.get $p)))) (f32.const 2))
          (f32.neg (f32.load (local.get $p)))))
      (local.set $p (i32.add (local.get $p) (i32.const 4)))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1))))))

  ;; Unrolled by two, as clang emits: d[2i] = x[2i] * c[0], d[2i+1] = x[2i+1] * c[0], with c[0] loaded
  ;; every iteration and the counter going up to a limit by lt_u. n counts pairs.
  (func (export "unrolled") (param $d i32) (param $x i32) (param $c i32) (param $n i32)
    (local $i i32)
    (loop $l
      (f64.store offset=0 (i32.add (local.get $d) (local.get $i))
        (f64.mul (f64.load offset=0 (i32.add (local.get $x) (local.get $i))) (f64.load (local.get $c))))
      (f64.store offset=8 (i32.add (local.get $d) (local.get $i))
        (f64.mul (f64.load offset=8 (i32.add (local.get $x) (local.get $i))) (f64.load (local.get $c))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 16)))
                          (i32.shl (local.get $n) (i32.const 4))))))
)
