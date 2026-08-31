;; Whether a frame can catch is a property of the function that frame is running, and
;; return_call replaces that function in place. Both directions must hold.
(module
  (tag $e (param))

  (func $throwing
    throw $e
  )

  ;; Has a try_table, so a call it makes can be caught here.
  (func $catcher (result i32)
    (block $b
      (try_table (catch_all $b)
        (call $throwing)
        (return (i32.const 0))
      )
    )
    (i32.const 1)
  )

  ;; No try_table of its own; the frame it leaves behind for $catcher must start catching.
  (func (export "tail_into_catcher") (result i32)
    (return_call $catcher)
  )

  ;; No try_table; tail-called from a frame that had one, so the throw must escape.
  (func $nocatch (result i32)
    (call $throwing)
    (i32.const 0)
  )

  (func (export "catcher_tail_into_nocatch") (result i32)
    (block $b
      (try_table (catch_all $b)
        (return_call $nocatch)
      )
    )
    (i32.const 1)
  )
)
