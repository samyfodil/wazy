//! The guest half of wazy's fixed-length list test: each export hands its
//! list<T, N> to a host import, or reshapes it itself, so values cross the
//! boundary in both directions through generated bindings.

wit_bindgen::generate!({ path: "wit", world: "guest" });

use exports::wazy::fixed::api::{Guest, Point};
use wazy::fixed::host;

struct Component;

impl Guest for Component {
    // The host reverses; the guest adds one to each element of what comes back.
    fn run(v: [u32; 4]) -> [u32; 4] {
        let mut r = host::reverse(v);
        for x in r.iter_mut() {
            *x += 1;
        }
        r
    }

    // The host greets and bids farewell; the guest keeps the greeting for
    // the first name and the farewell for the second. (wit-bindgen 0.63.0
    // cannot pass a list<string, N> to an import, so strings reach the host
    // in this list's result, and the guest in the import's.)
    fn names(n: [String; 2]) -> [String; 2] {
        let [first, second] = n;
        let [hello, _] = host::greet(&first);
        let [_, bye] = host::greet(&second);
        [hello, bye]
    }

    // The host moves each point by 10.
    fn points(ps: [Point; 2]) -> [Point; 2] {
        host::shift(ps, 10)
    }

    // The guest turns the matrix half a turn: last row first, each reversed.
    fn matrix(m: [[u8; 3]; 2]) -> [[u8; 3]; 2] {
        let mut out = [[0u8; 3]; 2];
        for i in 0..2 {
            for j in 0..3 {
                out[1 - i][2 - j] = m[i][j];
            }
        }
        out
    }

    // Twenty u64s are too many to pass flat, so they arrive through memory.
    fn wide(v: [u64; 20]) -> u64 {
        v.iter().fold(0u64, |acc, x| acc.wrapping_mul(31).wrapping_add(*x))
    }
}

export!(Component);
