;; Hinted if, if without else, and br_if, in the shapes the layout treats
;; differently: an unlikely arm, an unlikely loop exit, a loop that is rarely
;; entered at all, and a cold arm that rejoins the hot path at two points.
;; branch_hints_inverted.wat is this file with every hint flipped.
(module
  (func (export "ifelse") (param i32) (result i32)
    local.get 0
    (@metadata.code.branch_hint "\00")
    if (result i32)
      i32.const 10
    else
      i32.const 20
    end)

  (func (export "ifnoelse") (param i32) (result i32)
    (local i32)
    local.get 0
    (@metadata.code.branch_hint "\01")
    if
      i32.const 5
      local.set 1
    end
    local.get 1
    i32.const 1
    i32.add)

  ;; Sums 1..n, stopping early once the sum passes 1000.
  (func (export "sum") (param i32) (result i32)
    (local i32 i32)
    block
      loop
        local.get 1
        local.get 0
        i32.ge_u
        (@metadata.code.branch_hint "\00")
        br_if 1
        local.get 1
        i32.const 1
        i32.add
        local.tee 1
        local.get 2
        i32.add
        local.tee 2
        i32.const 1000
        i32.gt_u
        (@metadata.code.branch_hint "\00")
        br_if 1
        br 0
      end
    end
    local.get 2)

  ;; Returns n below 100, and otherwise counts n down to 100 in a loop.
  (func (export "rareloop") (param i32) (result i32)
    (local i32)
    local.get 0
    i32.const 100
    i32.gt_u
    (@metadata.code.branch_hint "\00")
    if
      loop
        local.get 1
        i32.const 1
        i32.add
        local.set 1
        local.get 0
        i32.const 1
        i32.sub
        local.tee 0
        i32.const 100
        i32.gt_u
        (@metadata.code.branch_hint "\01")
        br_if 0
      end
    end
    local.get 0
    local.get 1
    i32.const 1000
    i32.mul
    i32.add)

  ;; The odd arm is cold, and leaves for either of two hot blocks.
  (func (export "rejoin") (param i32) (result i32)
    (local i32)
    block
      block
        local.get 0
        i32.const 1
        i32.and
        (@metadata.code.branch_hint "\00")
        if
          local.get 0
          i32.const 3
          i32.and
          i32.const 3
          i32.eq
          br_if 2
          i32.const 7
          local.set 1
          br 1
        end
        i32.const 2
        local.set 1
      end
      local.get 1
      i32.const 100
      i32.add
      local.set 1
    end
    local.get 1
    local.get 0
    i32.add)
)
