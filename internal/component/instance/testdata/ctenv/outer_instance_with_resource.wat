(component (type $r (resource (rep i32))) (type $i (instance (export "f" (func (param "x" (own $r)))))) (component (alias outer 1 $i (type))))
