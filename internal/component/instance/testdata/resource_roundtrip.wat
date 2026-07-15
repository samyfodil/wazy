(component
  (type $rt (resource (rep i32)))

  (core func $res_new (canon resource.new $rt))
  (core func $res_rep (canon resource.rep $rt))
  (core func $res_drop (canon resource.drop $rt))
  (core instance $resci
    (export "new" (func $res_new))
    (export "rep" (func $res_rep))
    (export "drop" (func $res_drop)))

  (core module $main
    (import "resource" "new" (func $new (param i32) (result i32)))
    (import "resource" "rep" (func $rep (param i32) (result i32)))
    (import "resource" "drop" (func $drop (param i32)))

    ;; roundtrip: given a rep, create a handle, read the rep back, drop it,
    ;; and return the rep read back (so the Go test can assert it matches).
    (func (export "roundtrip") (param $r i32) (result i32)
      (local $h i32) (local $got i32)
      (local.set $h (call $new (local.get $r)))
      (local.set $got (call $rep (local.get $h)))
      (call $drop (local.get $h))
      (local.get $got))

    ;; expose new/rep/drop individually too, for finer-grained Go-side error
    ;; path tests (double-drop, rep-after-drop, unknown handle, ...).
    (func (export "new") (param i32) (result i32) (call $new (local.get 0)))
    (func (export "rep") (param i32) (result i32) (call $rep (local.get 0)))
    (func (export "drop") (param i32) (call $drop (local.get 0))))

  (core instance $mainci (instantiate $main
    (with "resource" (instance $resci))))

  (func $roundtrip (param "r" u32) (result u32) (canon lift (core func $mainci "roundtrip")))
  (func $new (param "r" u32) (result u32) (canon lift (core func $mainci "new")))
  (func $rep (param "h" u32) (result u32) (canon lift (core func $mainci "rep")))
  (func $drop (param "h" u32) (canon lift (core func $mainci "drop")))

  (export "roundtrip" (func $roundtrip))
  (export "new" (func $new))
  (export "rep" (func $rep))
  (export "drop" (func $drop)))
