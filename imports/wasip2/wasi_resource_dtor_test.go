package wasip2

import (
	"context"
	"testing"
)

// The wasi:filesystem and wasi:sockets destructors release the node their rep
// named, and ignore a rep they do not own.
//
// Both packages tracked every descriptor, stream and socket a guest opened in
// a map, and nothing ever removed one: an instance held all of them for as
// long as it lived. These are the destructors that release them when the guest
// drops the handle.
//
// The "not mine" cases are not padding. input-stream and output-stream are one
// tag shared by wasi:filesystem, wasi:sockets and wasi:http, each minting from
// a disjoint range of reps, so every one of these destructors is called for
// every stream any of them created. Treating another subsystem's rep as an
// error would turn a normal drop into a failure.
func TestFilesystemDestructorsReleaseNodes(t *testing.T) {
	ctx := context.Background()
	fs := newWasiFS(nil)

	fs.descs[100] = &fsDescNode{}
	fs.dirStreams[101] = &fsDirStreamNode{}
	fs.streams[102] = &fsStreamNode{}
	fs.writeStreams[103] = &fsWriteStreamNode{}

	for _, tc := range []struct {
		name string
		drop func(context.Context, uint32) error
		rep  uint32
		size func() int
	}{
		{"descriptor", fs.dropDescriptor, 100, func() int { return len(fs.descs) }},
		{"directory-entry-stream", fs.dropDirStream, 101, func() int { return len(fs.dirStreams) }},
		{"input-stream", fs.dropInputStream, 102, func() int { return len(fs.streams) }},
		{"output-stream", fs.dropOutputStream, 103, func() int { return len(fs.writeStreams) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.size(); got != 1 {
				t.Fatalf("setup: map holds %d entries, want 1", got)
			}
			if err := tc.drop(ctx, tc.rep); err != nil {
				t.Fatalf("drop: %v", err)
			}
			if got := tc.size(); got != 0 {
				t.Errorf("after drop: map holds %d entries, want 0", got)
			}
			// A rep from another subsystem sharing this tag.
			if err := tc.drop(ctx, 1<<24); err != nil {
				t.Errorf("dropping a rep this package did not mint: %v, want no error", err)
			}
		})
	}
}

func TestSocketDestructorsReleaseNodes(t *testing.T) {
	ctx := context.Background()
	s := newWasiSockets(nil, nil, nil, nil)

	s.tcpSocks[200] = &tcpSockNode{}
	s.udpSocks[201] = &udpSockNode{}
	s.resolveStream[202] = &resolveAddrStream{}
	s.inStreams[203] = &sockInStream{}
	s.outStreams[204] = &sockOutStream{}
	s.inDgrams[205] = &incomingDatagramStream{}
	s.outDgrams[206] = &outgoingDatagramStream{}

	for _, tc := range []struct {
		name string
		drop func(context.Context, uint32) error
		rep  uint32
		size func() int
	}{
		{"tcp-socket", s.dropTCPSock, 200, func() int { return len(s.tcpSocks) }},
		{"udp-socket", s.dropUDPSock, 201, func() int { return len(s.udpSocks) }},
		{"resolve-address-stream", s.dropResolveStream, 202, func() int { return len(s.resolveStream) }},
		{"input-stream", s.dropInputStream, 203, func() int { return len(s.inStreams) }},
		{"output-stream", s.dropOutputStream, 204, func() int { return len(s.outStreams) }},
		{"incoming-datagram-stream", s.dropInDgram, 205, func() int { return len(s.inDgrams) }},
		{"outgoing-datagram-stream", s.dropOutDgram, 206, func() int { return len(s.outDgrams) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.size(); got != 1 {
				t.Fatalf("setup: map holds %d entries, want 1", got)
			}
			if err := tc.drop(ctx, tc.rep); err != nil {
				t.Fatalf("drop: %v", err)
			}
			if got := tc.size(); got != 0 {
				t.Errorf("after drop: map holds %d entries, want 0", got)
			}
			if err := tc.drop(ctx, 7); err != nil {
				t.Errorf("dropping a rep this package did not mint: %v, want no error", err)
			}
		})
	}
}
