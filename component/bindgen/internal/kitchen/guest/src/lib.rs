//! The guest half of bindgen's kitchen test: each export either reshapes its
//! argument itself or hands it to a host import and returns what comes back,
//! so every WIT type crosses the boundary in both directions.

wit_bindgen::generate!({ path: "wit", world: "kitchen" });

use exports::wazy::bindgen_test::api::Guest as Api;
use wazy::bindgen_test::host;
use wazy::bindgen_test::types::{Color, Everything, Perms, Point, Shape};

struct Component;

impl Guest for Component {
    fn ping(s: String) -> String {
        format!("pong:{s}")
    }
}

impl Api for Component {
    // The host echoes; the guest marks what came back so the test can tell
    // the round trip happened.
    fn roundtrip(e: Everything) -> Everything {
        let mut e = host::echo(&e);
        e.s.push('!');
        e
    }

    fn area(s: Shape) -> f64 {
        match s {
            Shape::Circle(r) => 3.0 * r * r,
            Shape::Rect(p) => (p.x as f64) * (p.y as f64),
            Shape::Empty => 0.0,
        }
    }

    fn check(n: u32) -> Result<u32, String> {
        host::checked(n).map(|()| n * 2)
    }

    fn first(xs: Vec<Point>) -> Option<Point> {
        xs.into_iter().next()
    }

    fn swap(t: (String, u32)) -> (u32, String) {
        (t.1, t.0)
    }

    // The host's counter: constructed, bumped, and dropped when it goes out
    // of scope; the static zero is bumped and dropped too.
    fn use_counter(start: u32, by: u32) -> u32 {
        let c = host::Counter::new(start);
        let z = host::Counter::zero();
        z.bump(1);
        c.bump(by)
    }

    fn classify_all(xs: Vec<Shape>) -> Vec<Result<Color, String>> {
        xs.iter().map(host::classify).collect()
    }

    fn pick_name(c: Color, p: Perms) -> Option<String> {
        host::pick(c, p)
    }

    // Two tokens from host::mint, read back through borrows, then dropped.
    fn use_tokens(a: u32, b: u32) -> u32 {
        let x = host::mint(a);
        let y = host::mint(b);
        host::peek(&x) + host::peek(&y)
    }

    fn nothing() {}
}

export!(Component);
