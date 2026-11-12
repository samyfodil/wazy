(component
  (import "a:b/c@1" (version-suffix ".2.3") (instance
    (export "f" (func))
  ))
  (import "x" (implements "a:b/d@0.2") (version-suffix ".1") (instance
    (export "g" (func))
  ))
)
