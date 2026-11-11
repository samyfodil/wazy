//! The guest half of bindgen's kitchen test: each export either reshapes its
//! argument itself or hands it to a host import and returns what comes back,
//! so every WIT type crosses the boundary in both directions.

wit_bindgen::generate!({ path: "wit", world: "kitchen" });

use std::cell::RefCell;

use exports::wazy::bindgen_test::api::{Blob, BlobBorrow, Guest as Api, GuestBlob};
use wazy::bindgen_test::host;
use wazy::bindgen_test::types::{Color, Everything, Perms, Point, Shape, Token};

struct Component;

impl Guest for Component {
    fn ping(s: String) -> String {
        format!("pong:{s}")
    }

    async fn top(s: String) -> String {
        host::slow_ping().await;
        format!("top:{s}")
    }
}

struct MyBlob(RefCell<String>);

impl GuestBlob for MyBlob {
    fn new(init: String) -> Self {
        MyBlob(RefCell::new(init))
    }

    fn append(&self, s: String) {
        self.0.borrow_mut().push_str(&s);
    }

    fn read(&self) -> String {
        self.0.borrow().clone()
    }

    async fn measure(&self) -> u32 {
        let n = self.0.borrow().len() as u32;
        host::slow_add(n, 0).await
    }

    fn join(a: BlobBorrow<'_>, b: BlobBorrow<'_>) -> Blob {
        let s = format!("{}{}", a.get::<MyBlob>().read(), b.get::<MyBlob>().read());
        Blob::new(MyBlob(RefCell::new(s)))
    }
}

impl Api for Component {
    type Blob = MyBlob;

    fn make_blobs(n: u32) -> Vec<Blob> {
        (0..n).map(|i| Blob::new(MyBlob(RefCell::new(format!("b{i}"))))).collect()
    }

    fn blob_size(b: BlobBorrow<'_>) -> u32 {
        b.get::<MyBlob>().0.borrow().len() as u32
    }

    // Host tokens the host gives up, nested in a list; dropped at the end.
    fn tally(ts: Vec<Token>) -> u32 {
        ts.iter().map(host::peek).sum()
    }

    // An own token the host gives up; dropping it reaches the host.
    fn take_token(t: Token) -> u32 {
        host::peek(&t)
    }

    // A token the guest gets from the host and hands back to it.
    fn give_token(v: u32) -> Token {
        host::mint(v)
    }

    fn peek_token(t: &Token) -> u32 {
        host::peek(t)
    }

    // Tokens nested in a host import's tuple, list and result.
    fn pair_tokens(a: u32, b: u32) -> u32 {
        let (x, y) = host::mint_pair(a, b);
        let both = host::peek_all(&[&x, &y]);
        let z = host::try_mint(7).unwrap();
        let bad = host::try_mint(0).is_err() as u32;
        both + host::peek(&z) + bad
    }

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

    // Through the host's deepen, which adds one to a some(some(n)).
    fn nest(x: Option<Option<u32>>) -> Option<Option<u32>> {
        host::deepen(x)
    }

    async fn fetch(a: u32, b: u32) -> u32 {
        host::slow_add(a, b).await
    }

    async fn fetch_shape(s: Shape) -> Result<Color, String> {
        host::slow_shape(s).await
    }

    // Two pings, so the call waits on the host twice.
    async fn fire() -> u32 {
        host::slow_ping().await;
        host::slow_ping().await;
        2
    }

    async fn fetch_tokens(a: u32, b: u32) -> u32 {
        let (x, y) = host::slow_mint(a, b).await;
        host::peek(&x) + host::peek(&y)
    }

    fn nothing() {}
}

export!(Component);
