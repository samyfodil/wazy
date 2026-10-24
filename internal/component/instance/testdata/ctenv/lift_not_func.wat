(component (type (record (field "a" u8))) (core module $m (func (export "f"))) (core instance $i (instantiate $m)) (func (type 0) (canon lift (core func $i "f"))))
