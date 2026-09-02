#[allow(warnings)]
mod bindings;

use bindings::Guest;

struct Component;

impl Guest for Component {
    fn greet(name: String) -> String {
        format!("Hello, {name}! (from Rust)")
    }
}

bindings::export!(Component with_types_in bindings);
