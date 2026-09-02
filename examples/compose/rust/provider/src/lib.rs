//! `wazy:compose/provider` — the provider half of the cross-language
//! composition matrix, in Rust.
//!
//! Exports `wazy:compose/greeter`. Every string it produces names Rust, so a
//! composed consumer written in another language proves which side ran.

#[allow(warnings)]
mod bindings;

use bindings::exports::wazy::compose::greeter::{Guest, Visitor};

struct Component;

impl Guest for Component {
    /// `Hello, <name> #<id>! (from Rust)`
    fn greet(who: Visitor) -> String {
        format!("Hello, {} #{}! (from Rust)", who.name, who.id)
    }

    /// One `<name> (via Rust)` per input, in order.
    ///
    /// An empty input yields an empty output — the empty-list path is part of
    /// the contract under test, so it is deliberately *not* special-cased.
    fn greet_all(names: Vec<String>) -> Vec<String> {
        names
            .into_iter()
            .map(|name| format!("{name} (via Rust)"))
            .collect()
    }
}

bindings::export!(Component with_types_in bindings);
