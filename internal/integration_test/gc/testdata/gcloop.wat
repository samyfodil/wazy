(module
  (type $s (struct (field (mut i32)) (field (mut anyref))))
  (global $keep (mut anyref) (ref.null any))
  (table $t 4 anyref)

  ;; Allocates n structs and drops every one of them.
  (func (export "churn") (param $n i32)
    (local $i i32)
    (loop $l
      (drop (struct.new $s (local.get $i) (ref.null any)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n)))
    )
  )

  ;; Allocates n structs, keeping the last one in a global and one in a table.
  (func (export "churn_keeping") (param $n i32)
    (local $i i32)
    (local $last (ref null $s))
    (loop $l
      (local.set $last (struct.new $s (local.get $i) (ref.null any)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n)))
    )
    (global.set $keep (local.get $last))
    (table.set $t (i32.const 1) (local.get $last))
  )

  ;; Builds a chain of n structs and keeps only the head, so everything is reachable through fields.
  (func (export "chain") (param $n i32)
    (local $i i32)
    (local $head (ref null $s))
    (loop $l
      (local.set $head (struct.new $s (local.get $i) (local.get $head)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n)))
    )
    (global.set $keep (local.get $head))
  )

  ;; Reads back the value of the struct the global names.
  (func (export "kept") (result i32)
    (struct.get $s 0 (ref.cast (ref $s) (global.get $keep)))
  )

  ;; Walks the chain from the global and counts its links.
  (func (export "chain_len") (result i32)
    (local $c i32)
    (local $r (ref null $s))
    (local.set $r (ref.cast (ref null $s) (global.get $keep)))
    (block $done
      (loop $l
        (br_if $done (ref.is_null (local.get $r)))
        (local.set $c (i32.add (local.get $c) (i32.const 1)))
        (local.set $r (ref.cast (ref null $s) (struct.get $s 1 (local.get $r))))
        (br $l)
      )
    )
    (local.get $c)
  )

  (func (export "clear")
    (global.set $keep (ref.null any))
    (table.set $t (i32.const 1) (ref.null any))
  )
)
