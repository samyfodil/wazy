(component
  (core module $m
    (memory (export "mem") 1)
    (func (export "run") (param i32) unreachable)
    (func (export "ra") (param i32 i32 i32 i32) (result i32) unreachable))
  (type $ft (func (result u32)))
  (core instance $i (instantiate $m))
  (alias core export $i "run" (core func $run))
  (alias core export $i "mem" (core memory $mem))
  (alias core export $i "ra" (core func $ra))
  (canon lift (core func $run) (memory $mem) (realloc $ra) async (func $lifted (type $ft)))
  (export "run-stackful" (func $lifted))
)
