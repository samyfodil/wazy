// The same exercise against the V1 table: sock_accept without fdflags, the
// address getters reporting the family separately, and bare 16-byte addresses.
use std::env;

#[repr(C)]
struct WasiAddress {
    buf: *const u8,
    size: usize,
}

#[repr(C)]
struct IoVec {
    buf: *const u8,
    size: usize,
}

#[link(wasm_import_module = "wasi_snapshot_preview1")]
extern "C" {
    fn sock_open(family: u32, sock_type: u32, fd: *mut u32) -> u32;
    fn sock_connect(fd: u32, addr: *const WasiAddress, port: u32) -> u32;
    fn sock_bind(fd: u32, addr: *const WasiAddress, port: u32) -> u32;
    fn sock_listen(fd: u32, backlog: u32) -> u32;
    // V1: no fdflags.
    fn sock_accept(fd: u32, fd_out: *mut u32) -> u32;
    // V1: the family comes back in its own result.
    fn sock_getlocaladdr(
        fd: u32,
        addr: *mut WasiAddress,
        addr_type: *mut u32,
        port: *mut u32,
    ) -> u32;
    fn sock_getpeeraddr(
        fd: u32,
        addr: *mut WasiAddress,
        addr_type: *mut u32,
        port: *mut u32,
    ) -> u32;
    fn sock_send(
        fd: u32,
        si_data: *const IoVec,
        si_data_len: u32,
        si_flags: u32,
        so_datalen: *mut u32,
    ) -> u32;
    fn sock_recv(
        fd: u32,
        ri_data: *const IoVec,
        ri_data_len: u32,
        ri_flags: u32,
        ro_datalen: *mut u32,
        ro_flags: *mut u32,
    ) -> u32;
}

const INET4: u32 = 1;
const STREAM: u32 = 2;

fn check(what: &str, errno: u32) {
    if errno != 0 {
        println!("{what}: errno={errno}");
        std::process::exit(1);
    }
}

fn main() {
    let args: Vec<String> = env::args().collect();
    let mode = args.get(1).map(|s| s.as_str()).unwrap_or("client");
    let port: u32 = args.get(2).and_then(|s| s.parse().ok()).unwrap_or(0);
    match mode {
        "client" => client(port),
        "server" => server(),
        other => println!("unknown mode {other}"),
    }
}

fn client(port: u32) {
    unsafe {
        let mut fd: u32 = 0;
        check("sock_open", sock_open(INET4, STREAM, &mut fd));

        // V1: a bare IPv4 address, four bytes, port passed alongside.
        let ip: [u8; 4] = [127, 0, 0, 1];
        let addr = WasiAddress { buf: ip.as_ptr(), size: 4 };
        check("sock_connect", sock_connect(fd, &addr, port));

        let msg = b"ping";
        let iov = IoVec { buf: msg.as_ptr(), size: msg.len() };
        let mut sent: u32 = 0;
        check("sock_send", sock_send(fd, &iov, 1, 0, &mut sent));
        println!("sent {sent}");

        let mut reply = [0u8; 64];
        let riov = IoVec { buf: reply.as_ptr(), size: reply.len() };
        let mut got: u32 = 0;
        let mut oflags: u32 = 0;
        check("sock_recv", sock_recv(fd, &riov, 1, 0, &mut got, &mut oflags));
        println!("recv {}", String::from_utf8_lossy(&reply[..got as usize]));

        // The getters use the wider 16-byte buffer and report the family
        // separately: an IPv4 address must come back as type 4.
        let mut local = [0u8; 16];
        let mut laddr = WasiAddress { buf: local.as_ptr(), size: 16 };
        let mut ltype: u32 = 0;
        let mut lport: u32 = 0;
        check(
            "sock_getlocaladdr",
            sock_getlocaladdr(fd, &mut laddr, &mut ltype, &mut lport),
        );
        println!(
            "local {}.{}.{}.{} type={ltype} port_set={}",
            local[0], local[1], local[2], local[3],
            lport != 0
        );

        let mut peer = [0u8; 16];
        let mut paddr = WasiAddress { buf: peer.as_ptr(), size: 16 };
        let mut ptype: u32 = 0;
        let mut pport: u32 = 0;
        check(
            "sock_getpeeraddr",
            sock_getpeeraddr(fd, &mut paddr, &mut ptype, &mut pport),
        );
        println!(
            "peer {}.{}.{}.{} type={ptype} port={pport}",
            peer[0], peer[1], peer[2], peer[3]
        );
    }
}

fn server() {
    unsafe {
        let mut fd: u32 = 0;
        check("sock_open", sock_open(INET4, STREAM, &mut fd));
        let ip: [u8; 4] = [127, 0, 0, 1];
        let addr = WasiAddress { buf: ip.as_ptr(), size: 4 };
        check("sock_bind", sock_bind(fd, &addr, 0));
        check("sock_listen", sock_listen(fd, 8));

        let mut local = [0u8; 16];
        let mut laddr = WasiAddress { buf: local.as_ptr(), size: 16 };
        let mut ltype: u32 = 0;
        let mut lport: u32 = 0;
        check(
            "sock_getlocaladdr",
            sock_getlocaladdr(fd, &mut laddr, &mut ltype, &mut lport),
        );
        println!("listening {lport}");

        // V1 accept: two parameters.
        let mut conn: u32 = 0;
        check("sock_accept", sock_accept(fd, &mut conn));

        let mut req = [0u8; 64];
        let riov = IoVec { buf: req.as_ptr(), size: req.len() };
        let mut got: u32 = 0;
        let mut oflags: u32 = 0;
        check("sock_recv", sock_recv(conn, &riov, 1, 0, &mut got, &mut oflags));
        println!("server got {}", String::from_utf8_lossy(&req[..got as usize]));

        let msg = b"pong";
        let iov = IoVec { buf: msg.as_ptr(), size: msg.len() };
        let mut sent: u32 = 0;
        check("sock_send", sock_send(conn, &iov, 1, 0, &mut sent));
    }
}
