//! The guest half of bindgen's kitchen test: each export either reshapes its
//! argument itself or hands it to a host import and returns what comes back,
//! so every WIT type crosses the boundary in both directions.

wit_bindgen::generate!({ path: "wit", world: "kitchen" });

use std::cell::RefCell;

use wit_bindgen::rt::async_support::ErrorContext;
use wit_bindgen::{FutureReader, StreamReader};

use exports::wazy::bindgen_test::api::{Blob, BlobBorrow, Guest as Api, GuestBlob};
use wazy::bindgen_test::host;
use wazy::bindgen_test::types::{Body, Color, Everything, Perms, Point, Shape, Token};

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

    async fn sum_bytes(s: StreamReader<u8>) -> u32 {
        s.collect().await.iter().map(|&b| b as u32).sum()
    }

    // The stream outlives the call: a spawned task writes it after the
    // reader is returned.
    async fn points(n: u32) -> StreamReader<Point> {
        let (mut w, r) = wit_stream::new();
        wit_bindgen::spawn_local(async move {
            w.write_all((0..n).map(|i| point(i as i32)).collect()).await;
        });
        r
    }

    // Bytes from the host's stream become points in a stream back to it,
    // written while the host reads.
    async fn relay(n: u32) -> i32 {
        let bytes = host::count_up(n).await.collect().await;
        let (mut w, r) = wit_stream::new();
        wit_bindgen::spawn_local(async move {
            w.write_all(bytes.iter().map(|&b| point(b as i32)).collect()).await;
        });
        host::sum_points(r).await
    }

    async fn color_of(s: Shape) -> Result<Color, String> {
        host::color_later(&s).await
    }

    async fn double(f: FutureReader<u32>) -> u32 {
        f.await * 2
    }

    // A future the guest makes and sets while the host waits on it.
    async fn hand_off(v: u32) -> u32 {
        let (w, r) = wit_future::new(|| 0);
        wit_bindgen::spawn_local(async move {
            w.write(v + 1).await.unwrap();
        });
        host::wait_for(r).await
    }

    async fn ready(v: u32) -> FutureReader<u32> {
        let (w, r) = wit_future::new(|| 0);
        wit_bindgen::spawn_local(async move {
            w.write(v * 10).await.unwrap();
        });
        r
    }

    async fn count_ticks(s: StreamReader<()>) -> u32 {
        s.collect().await.len() as u32
    }

    async fn fail(msg: String) -> ErrorContext {
        ErrorContext::new(&format!("guest: {msg}"))
    }

    fn reason(e: ErrorContext) -> String {
        e.debug_message()
    }

    // The host's error context, back to the host.
    fn relay_error(code: u32) -> String {
        host::describe(host::explain(code))
    }

    async fn read_body(b: Body) -> u32 {
        let data: u32 = b.data.collect().await.iter().map(|&x| x as u32).sum();
        data + b.trailer.await
    }

    // The body outlives the call: a spawned task writes its bytes, then its
    // trailer, after it is returned.
    async fn make_body(n: u32) -> Body {
        let (mut w, data) = wit_stream::new();
        let (fw, trailer) = wit_future::new(|| 0);
        wit_bindgen::spawn_local(async move {
            w.write_all((0..n).map(|i| i as u8).collect()).await;
            drop(w);
            fw.write(n * 100).await.unwrap();
        });
        Body { data, trailer }
    }

    // The host's body, back to the host.
    async fn pipe_body(n: u32) -> u32 {
        match host::open_body(n).await {
            Ok(b) => host::sum_body(b).await,
            Err(_) => 0,
        }
    }

    fn join_errors(es: Vec<ErrorContext>) -> String {
        es.iter().map(|e| e.debug_message()).collect::<Vec<_>>().join(",")
    }

    fn relay_errors(n: u32) -> String {
        host::messages(&host::errors(n))
    }

    async fn some_error(fail: bool) -> Option<ErrorContext> {
        fail.then(|| ErrorContext::new("failed"))
    }

    async fn pipe_spilled(n: u32) -> u32 {
        match host::open_body(n).await {
            Ok(b) => host::spill_body(1, 2, 3, 4, b).await,
            Err(_) => 0,
        }
    }

    fn relay_spilled(n: u32) -> String {
        host::spill_errors(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, &host::errors(n))
    }

    fn nothing() {}
}

fn point(i: i32) -> Point {
    Point { x: i, y: i * 2, label: format!("p{i}") }
}

export!(Component);
