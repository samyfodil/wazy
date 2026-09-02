//! `wazy:compose/consumer` — the consumer half, in Rust.
//!
//! Imports `wazy:compose/greeter` and exports `run`. Nothing the provider says
//! is hardcoded here: every element of the returned list is text that crossed
//! the Canonical ABI, so a bad lift or lower shows up in the output instead of
//! being masked by a local constant.

#[allow(warnings)]
mod bindings;

use bindings::wazy::compose::greeter::{self, Visitor};
use bindings::Guest;

struct Component;

impl Guest for Component {
    fn run() -> Vec<String> {
        // 1. record-with-string argument, string result.
        let first = greeter::greet(&Visitor {
            name: "wazy".to_string(),
            id: 42,
        });

        // 2. list<string> in, list<string> out — take element 0.
        let all = greeter::greet_all(&["a".to_string(), "b".to_string()]);
        let second = match all.into_iter().next() {
            Some(s) => s,
            // Report the wrong answer rather than hiding it.
            None => "greet-all([a, b]) returned 0 elements".to_string(),
        };

        // 3. the empty-list path: report the real length, whatever it is.
        let empty = greeter::greet_all(&[]);
        let third = format!("empty-len={}", empty.len());

        vec![first, second, third]
    }
}

bindings::export!(Component with_types_in bindings);
