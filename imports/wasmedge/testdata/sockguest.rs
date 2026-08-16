// A guest exercising the WasmEdge sockets extension through hand-declared
// imports, so it depends on no SDK -- only on the wire contract.
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
    // The extension.
    fn sock_open(family: u32, sock_type: u32, fd: *mut u32) -> u32;
    fn sock_bind(fd: u32, addr: *const WasiAddress, port: u32) -> u32;
    fn sock_connect(fd: u32, addr: *const WasiAddress, port: u32) -> u32;
    fn sock_listen(fd: u32, backlog: u32) -> u32;
    fn sock_accept(fd: u32, fdflags: u32, fd_out: *mut u32) -> u32; // V2 signature
    fn sock_getlocaladdr(fd: u32, addr: *mut WasiAddress, port: *mut u32) -> u32; // V2
    fn sock_send_to(
        fd: u32,
        si_data: *const IoVec,
        si_data_len: u32,
        addr: *const WasiAddress,
        port: u32,
        si_flags: u32,
        so_datalen: *mut u32,
    ) -> u32;
    fn sock_recv_from(
        fd: u32,
        ri_data: *const IoVec,
        ri_data_len: u32,
        addr: *mut WasiAddress,
        ri_flags: u32,
        port: *mut u32,
        ro_datalen: *mut u32,
        ro_flags: *mut u32,
    ) -> u32; // V2

    // Standard WASI preview 1, used here on a socket the extension created.
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

    fn sock_getpeeraddr(fd: u32, addr: *mut WasiAddress, port: *mut u32) -> u32; // V2
    fn sock_setsockopt(fd: u32, level: u32, name: u32, value: *const u32, len: u32) -> u32;
    fn sock_getsockopt(fd: u32, level: u32, name: u32, value: *mut u32, len: u32) -> u32;
    fn sock_getaddrinfo(
        node: *const u8,
        node_len: u32,
        server: *const u8,
        server_len: u32,
        hints: *const WasiAddrinfo,
        res: *mut *mut WasiAddrinfo,
        max_len: u32,
        res_len: *mut u32,
    ) -> u32;
}

#[repr(C)]
struct WasiSockaddr {
    family: u8,
    _pad: [u8; 3],
    sa_data_len: u32,
    sa_data: *mut u8,
}

#[repr(C)]
struct WasiAddrinfo {
    ai_flags: u16,
    ai_family: u8,
    ai_socktype: u8,
    ai_protocol: u32,
    ai_addrlen: u32,
    ai_addr: *mut WasiSockaddr,
    ai_canonname: *mut u8,
    ai_canonnamelen: u32,
    ai_next: *mut WasiAddrinfo,
}

const INET4: u32 = 1;
const STREAM: u32 = 2;
const DATAGRAM: u32 = 1;

// A V2 address: 128 bytes, family first.
fn v2_addr(ip: [u8; 4]) -> [u8; 128] {
    let mut buf = [0u8; 128];
    buf[0] = INET4 as u8;
    buf[2..6].copy_from_slice(&ip);
    buf
}

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
        "udp" => udp(port),
        "peer" => peer(port),
        "opts" => opts(port),
        "addrinfo" => addrinfo(),
        other => println!("unknown mode {other}"),
    }
}

// Connect to the host's listener, send a line, read the reply, and report the
// local address the connection was assigned.
fn client(port: u32) {
    unsafe {
        let mut fd: u32 = 0;
        check("sock_open", sock_open(INET4, STREAM, &mut fd));

        let buf = v2_addr([127, 0, 0, 1]);
        let addr = WasiAddress { buf: buf.as_ptr(), size: 128 };
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

        // The local address is assigned by connect, so the port must be set.
        let mut local = [0u8; 128];
        let mut laddr = WasiAddress { buf: local.as_ptr(), size: 128 };
        let mut lport: u32 = 0;
        check("sock_getlocaladdr", sock_getlocaladdr(fd, &mut laddr, &mut lport));
        println!(
            "local {}.{}.{}.{} port_set={}",
            local[2], local[3], local[4], local[5],
            lport != 0
        );
    }
}

// Listen, report the port the host should dial, then echo one line back.
fn server() {
    unsafe {
        let mut fd: u32 = 0;
        check("sock_open", sock_open(INET4, STREAM, &mut fd));

        let buf = v2_addr([127, 0, 0, 1]);
        let addr = WasiAddress { buf: buf.as_ptr(), size: 128 };
        check("sock_bind", sock_bind(fd, &addr, 0));
        check("sock_listen", sock_listen(fd, 8));

        let mut local = [0u8; 128];
        let mut laddr = WasiAddress { buf: local.as_ptr(), size: 128 };
        let mut lport: u32 = 0;
        check("sock_getlocaladdr", sock_getlocaladdr(fd, &mut laddr, &mut lport));
        println!("listening {lport}");

        let mut conn: u32 = 0;
        check("sock_accept", sock_accept(fd, 0, &mut conn));

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

// Datagram round trip against the host's UDP socket.
fn udp(port: u32) {
    unsafe {
        let mut fd: u32 = 0;
        check("sock_open", sock_open(INET4, DATAGRAM, &mut fd));

        let bind_buf = v2_addr([127, 0, 0, 1]);
        let bind_addr = WasiAddress { buf: bind_buf.as_ptr(), size: 128 };
        check("sock_bind", sock_bind(fd, &bind_addr, 0));

        let to_buf = v2_addr([127, 0, 0, 1]);
        let to = WasiAddress { buf: to_buf.as_ptr(), size: 128 };
        let msg = b"ping";
        let iov = IoVec { buf: msg.as_ptr(), size: msg.len() };
        let mut sent: u32 = 0;
        check("sock_send_to", sock_send_to(fd, &iov, 1, &to, port, 0, &mut sent));
        println!("sent {sent}");

        let mut reply = [0u8; 64];
        let riov = IoVec { buf: reply.as_ptr(), size: reply.len() };
        let mut from = [0u8; 128];
        let mut faddr = WasiAddress { buf: from.as_ptr(), size: 128 };
        let mut fport: u32 = 0;
        let mut got: u32 = 0;
        let mut oflags: u32 = 0;
        check(
            "sock_recv_from",
            sock_recv_from(fd, &riov, 1, &mut faddr, 0, &mut fport, &mut got, &mut oflags),
        );
        println!(
            "recv {} from {}.{}.{}.{} port_set={}",
            String::from_utf8_lossy(&reply[..got as usize]),
            from[2], from[3], from[4], from[5],
            fport != 0
        );
    }
}

// Connect, then read back the peer address the extension reports.
fn peer(port: u32) {
    unsafe {
        let mut fd: u32 = 0;
        check("sock_open", sock_open(INET4, STREAM, &mut fd));
        let buf = v2_addr([127, 0, 0, 1]);
        let addr = WasiAddress { buf: buf.as_ptr(), size: 128 };
        check("sock_connect", sock_connect(fd, &addr, port));

        let mut peer = [0u8; 128];
        let mut paddr = WasiAddress { buf: peer.as_ptr(), size: 128 };
        let mut pport: u32 = 0;
        check("sock_getpeeraddr", sock_getpeeraddr(fd, &mut paddr, &mut pport));
        println!(
            "peer {}.{}.{}.{} port={}",
            peer[2], peer[3], peer[4], peer[5], pport
        );
    }
}

// Round-trip a socket option, and confirm an unsupported one is refused
// rather than silently accepted.
fn opts(port: u32) {
    unsafe {
        let mut fd: u32 = 0;
        check("sock_open", sock_open(INET4, STREAM, &mut fd));
        let buf = v2_addr([127, 0, 0, 1]);
        let addr = WasiAddress { buf: buf.as_ptr(), size: 128 };
        check("sock_connect", sock_connect(fd, &addr, port));

        // TCP_NODELAY, level 6.
        let on: u32 = 1;
        check("sock_setsockopt", sock_setsockopt(fd, 6, 15, &on, 4));

        // SO_TYPE is answerable without a raw socket: expect Stream (2).
        let mut got: u32 = 0;
        check("sock_getsockopt", sock_getsockopt(fd, 0, 1, &mut got, 4));
        println!("sotype {got}");

        // SO_LINGER is not reachable through the backend: expect ENOTSUP (58).
        let linger: u32 = 0;
        let errno = sock_setsockopt(fd, 0, 9, &linger, 4);
        println!("linger errno {errno}");
    }
}

// Resolve a name into the caller-allocated linked list the extension fills.
fn addrinfo() {
    unsafe {
        let mut data = [0u8; 26];
        let mut sockaddr = WasiSockaddr {
            family: 0,
            _pad: [0; 3],
            // The WasmEdge library sets 14 here while allocating 26, which the
            // host has to correct for; pass 14 so that path is exercised.
            sa_data_len: 14,
            sa_data: data.as_mut_ptr(),
        };
        let mut info = WasiAddrinfo {
            ai_flags: 0,
            ai_family: 0,
            ai_socktype: 0,
            ai_protocol: 0,
            ai_addrlen: 0,
            ai_addr: &mut sockaddr,
            ai_canonname: std::ptr::null_mut(),
            ai_canonnamelen: 0,
            ai_next: std::ptr::null_mut(),
        };
        let hints = WasiAddrinfo {
            ai_flags: 0,
            ai_family: INET4 as u8,
            ai_socktype: STREAM as u8,
            ai_protocol: 0,
            ai_addrlen: 0,
            ai_addr: std::ptr::null_mut(),
            ai_canonname: std::ptr::null_mut(),
            ai_canonnamelen: 0,
            ai_next: std::ptr::null_mut(),
        };

        let node = b"localhost";
        let service = b"80";
        let mut res: *mut WasiAddrinfo = &mut info;
        let mut res_len: u32 = 0;
        check(
            "sock_getaddrinfo",
            sock_getaddrinfo(
                node.as_ptr(),
                node.len() as u32,
                service.as_ptr(),
                service.len() as u32,
                &hints,
                &mut res,
                1,
                &mut res_len,
            ),
        );
        // Port is big-endian in the first two bytes, then the address.
        let port = ((data[0] as u32) << 8) | data[1] as u32;
        println!(
            "addrinfo n={res_len} family={} port={port} addr={}.{}.{}.{}",
            sockaddr.family, data[2], data[3], data[4], data[5]
        );
    }
}
