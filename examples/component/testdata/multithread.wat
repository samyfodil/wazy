;; Multi-threaded map-reduce over a shared array, with Component Model threads.
;;
;; run-async fills an 8-element u32 array with [1..8], then spawns FIVE worker
;; threads over one shared linear memory (thread.new-indirect): four "mapper"
;; threads each doubling their own 2-element chunk, and one "reducer" thread
;; that sums the whole (now-doubled) array and returns the total. main parks all
;; five, then drives them with thread.yield-then-resume -- mappers first, reducer
;; last. Result: 2*(1+..+8) = 72.
;;
;; Build: wasm-tools parse multithread.wat -o multithread.wasm
(component
  (core module $MemTable
    (table (export "ftbl") 2 funcref) ;; [0]=mapper [1]=reducer
    (memory (export "mem") 1))
  (core instance $memTable (instantiate $MemTable))

  (core module $m
    (import "" "task.return" (func $task.return (param i32)))
    (import "" "thread.new-indirect" (func $thread.new-indirect (param i32 i32) (result i32)))
    (import "" "thread.yield-then-resume" (func $thread.yield-then-resume (param i32) (result i32)))
    (import "" "ftbl" (table $ftbl 2 funcref))
    (import "" "mem" (memory 1))

    ;; mapper(chunk): double the two u32s in [chunk*2, chunk*2+2), in shared mem.
    (func $mapper (param $chunk i32)
      (local $b i32)
      (local.set $b (i32.mul (local.get $chunk) (i32.const 8)))
      (i32.store (local.get $b)
        (i32.mul (i32.load (local.get $b)) (i32.const 2)))
      (i32.store (i32.add (local.get $b) (i32.const 4))
        (i32.mul (i32.load (i32.add (local.get $b) (i32.const 4))) (i32.const 2))))
    (elem (table $ftbl) (i32.const 0) func $mapper)

    ;; reducer(_): sum the eight (doubled) elements and resolve the task. Runs
    ;; last, so the task is resolved when this final thread exits.
    (func $reducer (param $ignored i32)
      (local $i i32) (local $sum i32)
      (block $ue (loop $ul
        (br_if $ue (i32.ge_u (local.get $i) (i32.const 8)))
        (local.set $sum (i32.add (local.get $sum) (i32.load (i32.mul (local.get $i) (i32.const 4)))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $ul)))
      (call $task.return (local.get $sum)))
    (elem (table $ftbl) (i32.const 1) func $reducer)

    (func (export "run")
      (local $i i32)
      ;; array[i] = i+1  for i in 0..8  (bytes 0..32)
      (block $ie (loop $il
        (br_if $ie (i32.ge_u (local.get $i) (i32.const 8)))
        (i32.store (i32.mul (local.get $i) (i32.const 4)) (i32.add (local.get $i) (i32.const 1)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $il)))
      ;; spawn 4 mappers (chunks 0..4) + 1 reducer; stash handles at 64+i*4
      (local.set $i (i32.const 0))
      (block $se (loop $sl
        (br_if $se (i32.ge_u (local.get $i) (i32.const 4)))
        (i32.store (i32.add (i32.const 64) (i32.mul (local.get $i) (i32.const 4)))
          (call $thread.new-indirect (i32.const 0) (local.get $i)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $sl)))
      (i32.store (i32.const 80) ;; reducer handle at 64+4*4=80
        (call $thread.new-indirect (i32.const 1) (i32.const 0)))
      ;; drive the 4 mappers
      (local.set $i (i32.const 0))
      (block $re (loop $rl
        (br_if $re (i32.ge_u (local.get $i) (i32.const 4)))
        (drop (call $thread.yield-then-resume
          (i32.load (i32.add (i32.const 64) (i32.mul (local.get $i) (i32.const 4))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $rl)))
      ;; drive the reducer last (it resolves the task)
      (drop (call $thread.yield-then-resume (i32.load (i32.const 80)))))

    (func (export "ra") (param i32 i32 i32 i32) (result i32) unreachable))

  (core type $start-func-ty (func (param i32)))
  (alias core export $memTable "ftbl" (core table $ftbl))
  (alias core export $memTable "mem" (core memory $mem))

  (canon task.return (result u32) (core func $task.return))
  (core func $thread.new-indirect (canon thread.new-indirect $start-func-ty (table $ftbl)))
  (core func $thread.yield-then-resume (canon thread.yield-then-resume))

  (core instance $i (instantiate $m
    (with "" (instance
      (export "task.return" (func $task.return))
      (export "thread.new-indirect" (func $thread.new-indirect))
      (export "thread.yield-then-resume" (func $thread.yield-then-resume))
      (export "ftbl" (table $ftbl))
      (export "mem" (memory $mem))))))

  (alias core export $i "run" (core func $run))
  (alias core export $i "ra" (core func $ra))

  (type $ft (func async (result u32)))
  (canon lift (core func $run) (memory $mem) (realloc $ra) async (func $lifted (type $ft)))
  (export "run-async" (func $lifted))
)
