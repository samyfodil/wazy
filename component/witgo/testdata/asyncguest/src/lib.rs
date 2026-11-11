wit_bindgen::generate!({ path: "wit", world: "probe" });

struct Component;

impl Guest for Component {
    async fn fetch(x: u32) -> u32 {
        wazy::async_probe::host::slow(x).await + 1
    }
}

export!(Component);
