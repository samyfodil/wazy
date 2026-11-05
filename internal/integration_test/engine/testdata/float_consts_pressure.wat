;; A loop using more float constants than there are vector registers, with as many accumulators:
;; the constants LICM hoists out of it must be evicted, and their reloads must load them again.
(module
  (func (export "run") (param $n i32) (param $x f64) (result f64)
    (local f64 f64 f64 f64 f64 f64 f64 f64 f64 f64 f64 f64 f32)
    (loop $l
      (local.set 2 (f64.add (f64.mul (local.get 2) (f64.const 0.5)) (f64.sub (f64.mul (local.get $x) (f64.const 1.75)) (f64.const 3.0))))
      (local.set 3 (f64.add (f64.mul (local.get 3) (f64.const 4.25)) (f64.sub (f64.mul (local.get $x) (f64.const 5.5)) (f64.const 6.75))))
      (local.set 4 (f64.add (f64.mul (local.get 4) (f64.const 8.0)) (f64.sub (f64.mul (local.get $x) (f64.const 9.25)) (f64.const 10.5))))
      (local.set 5 (f64.add (f64.mul (local.get 5) (f64.const 11.75)) (f64.sub (f64.mul (local.get $x) (f64.const 13.0)) (f64.const 14.25))))
      (local.set 6 (f64.add (f64.mul (local.get 6) (f64.const 15.5)) (f64.sub (f64.mul (local.get $x) (f64.const 16.75)) (f64.const 18.0))))
      (local.set 7 (f64.add (f64.mul (local.get 7) (f64.const 19.25)) (f64.sub (f64.mul (local.get $x) (f64.const 20.5)) (f64.const 21.75))))
      (local.set 8 (f64.add (f64.mul (local.get 8) (f64.const 23.0)) (f64.sub (f64.mul (local.get $x) (f64.const 24.25)) (f64.const 25.5))))
      (local.set 9 (f64.add (f64.mul (local.get 9) (f64.const 26.75)) (f64.sub (f64.mul (local.get $x) (f64.const 28.0)) (f64.const 29.25))))
      (local.set 10 (f64.add (f64.mul (local.get 10) (f64.const 30.5)) (f64.sub (f64.mul (local.get $x) (f64.const 31.75)) (f64.const 33.0))))
      (local.set 11 (f64.add (f64.mul (local.get 11) (f64.const 34.25)) (f64.sub (f64.mul (local.get $x) (f64.const 35.5)) (f64.const 36.75))))
      (local.set 12 (f64.add (f64.mul (local.get 12) (f64.const 38.0)) (f64.sub (f64.mul (local.get $x) (f64.const 39.25)) (f64.const 40.5))))
      (local.set 13 (f64.add (f64.mul (local.get 13) (f64.const 41.75)) (f64.sub (f64.mul (local.get $x) (f64.const 43.0)) (f64.const 44.25))))
      (local.set 14 (f32.add (local.get 14) (f32.mul (f32.const 0.75) (f32.demote_f64 (local.get 2)))))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
    (f64.add (f64.add (f64.add (f64.add (f64.add (f64.add (f64.add (f64.add (f64.add (f64.add (f64.add (f64.add (f64.promote_f32 (local.get 14)) (local.get 2)) (local.get 3)) (local.get 4)) (local.get 5)) (local.get 6)) (local.get 7)) (local.get 8)) (local.get 9)) (local.get 10)) (local.get 11)) (local.get 12)) (local.get 13))))
