(component
  (type $allprims (record
    (field "a" bool) (field "b" s8) (field "c" u8) (field "d" s16) (field "e" u16)
    (field "f" s32) (field "g" u32) (field "h" s64) (field "i" u64)
    (field "j" f32) (field "k" f64) (field "l" char) (field "m" string)))
  (type $t (tuple u32 string bool))
  (type $nested (list (list u8)))
  (import "test:pkg/comp" (component $c
    (import "x" (func))
    (export "y" (func (result u32)))
  ))
  (core module $m
    (func (export "dtor") (param i32))
    (func (export "many") (param i32 i32 i32 i32 i32 i32) (result i32) local.get 0)
  )
  (core instance $ci (instantiate $m))
  (type $r (resource (rep i32) (dtor (func $ci "dtor"))))
  (func $many
    (param "a" u32) (param "b" u32) (param "c" u32) (param "d" u32)
    (param "e" (own $r)) (param "f" (borrow $r))
    (result u32)
    (canon lift (core func $ci "many")))
  (export "allprims" (type $allprims))
  (export "t" (type $t))
  (export "nested" (type $nested))
  (export "r" (type $r))
  (export "many" (func $many))
)
