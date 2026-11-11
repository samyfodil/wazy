;; Types a nested component aliases out of the components around it. Each holds indices of the
;; space it is declared in, where the nested component's own types at those indices are others.
(component
  (type $e (enum "a" "b" "c"))
  (type $rec (record (field "x" $e) (field "y" u8)))
  (type $fn (func (param "r" $rec) (result $e)))
  (type $it (instance (export "f" (func (param "x" u32)))))
  (component $child
    (type $pad0 u32)
    (type $pad1 u64)
    (alias outer 1 $rec (type $rec-here))
    (alias outer 1 $fn (type $fn-here))
    (type $user-ty (instance
      (alias outer 2 $e (type $ec))
      (export "e" (type (eq $ec)))))
    (import "test:outer/user" (instance $user (type $user-ty)))
    (alias export $user "e" (type $user-e))
    (alias outer 1 $it (type $it-here))
    (component $grandchild
      (type $pad u32)
      (alias outer 2 $rec (type $rec-here)))
  )
)
