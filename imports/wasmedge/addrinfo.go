package wasmedge

import (
	"context"
	"encoding/binary"
	"net"
	"strconv"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/wasip1"
)

// The addrinfo struct the guest passes, matching WasmEdge's layout:
//
//	flags u16, family u8, socket_type u8, protocol u32, address_length u32,
//	address u32, canonical_name u32, canonical_name_length u32, next u32
//
// 28 bytes, with the trailing fields all 4-byte aligned.
const (
	addrInfoSize = 28

	addrInfoOffFamily        = 2
	addrInfoOffSocketType    = 3
	addrInfoOffAddressLength = 8
	addrInfoOffAddress       = 12
	addrInfoOffCanonNameLen  = 20
	addrInfoOffNext          = 24
)

// sockGetAddrInfo implements
// sock_getaddrinfo(name, name_len, service, service_len, hints, res, max_res, res_len_out) -> errno.
//
// The result shape is the guest's, not ours: it pre-allocates a linked list of
// addrinfo structs, each pointing at its own sockaddr buffer, and the host
// fills them in place. See the reference implementation's notes on how
// under-specified this interface is -- the lengths the guest sets are not
// reliable, so the layout below follows what the WasmEdge sockets library
// actually does rather than what its struct definitions claim.
func sockGetAddrInfo(_ context.Context, mod api.Module, namePtr, nameLen, servicePtr, serviceLen, hintsPtr, resPtr, maxRes, resLenOut uint32) uint32 {
	mem := mod.Memory()

	if (nameLen == 0 && serviceLen == 0) || maxRes == 0 {
		return uint32(wasip1.ErrnoInval)
	}

	name, errno := readGuestString(mod, namePtr, nameLen)
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	service, errno := readGuestString(mod, servicePtr, serviceLen)
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}

	// The hints tell us which family to filter to; the rest are advisory.
	var wantFamily int32 = addressFamilyUnspec
	if hintsPtr != 0 {
		if f, ok := mem.ReadByte(hintsPtr + addrInfoOffFamily); ok {
			wantFamily = int32(f)
		}
	}

	ips, port, errno := resolve(name, service, wantFamily)
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}

	// Walk the guest's linked list, filling one entry per result. resPtr is a
	// pointer to the first entry's pointer, then each entry links to the next.
	entry, ok := mem.ReadUint32Le(resPtr)
	if !ok {
		return uint32(wasip1.ErrnoFault)
	}
	count := uint32(0)
	for _, ip := range ips {
		if count == maxRes || entry == 0 {
			break
		}
		if errno = writeAddrInfo(mod, entry, ip, port); errno != wasip1.ErrnoSuccess {
			return uint32(errno)
		}
		count++

		next, ok := mem.ReadUint32Le(entry + addrInfoOffNext)
		if !ok {
			return uint32(wasip1.ErrnoFault)
		}
		if next == 0 {
			break
		}
		entry = next
	}

	if !mem.WriteUint32Le(resLenOut, count) {
		return uint32(wasip1.ErrnoFault)
	}
	return uint32(wasip1.ErrnoSuccess)
}

// writeAddrInfo fills one addrinfo entry and the sockaddr it points at.
func writeAddrInfo(mod api.Module, entry uint32, ip net.IP, port int) wasip1.Errno {
	mem := mod.Memory()

	addrPtr, ok := mem.ReadUint32Le(entry + addrInfoOffAddress)
	if !ok {
		return wasip1.ErrnoFault
	}
	if addrPtr == 0 {
		return wasip1.ErrnoFault
	}

	// The sockaddr the entry points at: family byte, then a length and a
	// pointer to the address data itself.
	dataLen, ok := mem.ReadUint32Le(addrPtr + 4)
	if !ok {
		return wasip1.ErrnoFault
	}
	// WasmEdge's own library sets this to 14 while allocating 26 bytes, so the
	// declared length cannot be trusted. The reference implementation applies
	// the same correction.
	if dataLen == 14 {
		dataLen = 26
	}
	dataPtr, ok := mem.ReadUint32Le(addrPtr + 8)
	if !ok {
		return wasip1.ErrnoFault
	}

	var family int32
	var addrBytes []byte
	if ip4 := ip.To4(); ip4 != nil {
		family, addrBytes = addressFamilyInet4, ip4
	} else {
		family, addrBytes = addressFamilyInet6, ip.To16()
	}
	if uint32(2+len(addrBytes)) > dataLen {
		return wasip1.ErrnoFault
	}

	// Port first, in network byte order, then the address.
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	if !mem.Write(dataPtr, portBytes[:]) {
		return wasip1.ErrnoFault
	}
	if !mem.Write(dataPtr+2, addrBytes) {
		return wasip1.ErrnoFault
	}
	if !mem.WriteByte(addrPtr, byte(family)) {
		return wasip1.ErrnoFault
	}

	if !mem.WriteByte(entry+addrInfoOffFamily, byte(family)) {
		return wasip1.ErrnoFault
	}
	if !mem.WriteUint32Le(entry+addrInfoOffAddressLength, 16) { // sizeof(WasiSockaddr)
		return wasip1.ErrnoFault
	}
	if !mem.WriteUint32Le(entry+addrInfoOffCanonNameLen, 0) { // canonical names unsupported
		return wasip1.ErrnoFault
	}
	return wasip1.ErrnoSuccess
}

// resolve turns a host and service into addresses and a port.
func resolve(name, service string, wantFamily int32) ([]net.IP, int, wasip1.Errno) {
	port := 0
	if service != "" {
		if p, err := strconv.Atoi(service); err == nil {
			port = p
		} else if p, err := net.LookupPort("tcp", service); err == nil {
			port = p
		} else {
			return nil, 0, wasip1.ErrnoInval
		}
	}

	if name == "" {
		// A service with no host resolves to the wildcard, as getaddrinfo does
		// with AI_PASSIVE.
		if wantFamily == addressFamilyInet6 {
			return []net.IP{net.IPv6unspecified}, port, wasip1.ErrnoSuccess
		}
		return []net.IP{net.IPv4zero}, port, wasip1.ErrnoSuccess
	}

	if ip := net.ParseIP(name); ip != nil {
		return []net.IP{ip}, port, wasip1.ErrnoSuccess
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), name)
	if err != nil {
		return nil, 0, toWasiErrno(err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		switch wantFamily {
		case addressFamilyInet4:
			if a.IP.To4() == nil {
				continue
			}
		case addressFamilyInet6:
			if a.IP.To4() != nil {
				continue
			}
		}
		ips = append(ips, a.IP)
	}
	if len(ips) == 0 {
		return nil, 0, wasip1.ErrnoNoent
	}
	return ips, port, wasip1.ErrnoSuccess
}

// readGuestString reads a string the guest passed, trimming the trailing NUL
// the WasmEdge sockets library appends.
func readGuestString(mod api.Module, ptr, length uint32) (string, wasip1.Errno) {
	if length == 0 {
		return "", wasip1.ErrnoSuccess
	}
	b, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return "", wasip1.ErrnoFault
	}
	if b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return string(b), wasip1.ErrnoSuccess
}
