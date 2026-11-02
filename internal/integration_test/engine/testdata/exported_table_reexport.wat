;; Imports a table and exports it again under another name.
(module
  (import "tables" "externs" (table $t 2 externref))
  (export "again" (table $t))
)
