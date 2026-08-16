// Package grpc is the thin wrapper around the tunnelpb wire format and
// the abstract transport.Tunnel interface. Per ARCH §5, generated
// wire-format code lives in tunnelpb, and this package exposes Tunnel
// implementations built on top of an arbitrary byte stream (gRPC
// bidi stream, QUIC stream, in-memory pipe for tests).
//
// The actual gRPC server / client wiring lives in cmd/unvfd and is
// not part of this package: the wrapper takes a FrameCodec-shaped
// stream and gives back a transport.Tunnel. The transport layer is
// what depends on grpc types; everything else sees transport.Tunnel.
package grpc

import (
	"context"
	"io"
	"sync"

	"github.com/vukamecos/unverified/internal/contract/transport"
	"github.com/vukamecos/unverified/internal/transport/grpc/tunnelpb"
)

// Stream is the minimal bidirectional byte stream the wrapper needs.
// google.golang.org/grpc's *BidiStreamingServer/Client methods fit this
// shape, as does net.Conn, bytes.Buffer (for tests), etc.
type Stream interface {
	io.Reader
	io.Writer
}

// Wrap returns a transport.Tunnel backed by s. The wrapper takes
// ownership of s; concurrent calls are NOT safe (per the contract).
func Wrap(s Stream) transport.Tunnel {
	return &tunnel{s: s, codec: tunnelpb.New()}
}

type tunnel struct {
	s     Stream
	codec tunnelpb.Codec

	mu     sync.Mutex
	closed bool
}

func (t *tunnel) SendFrame(_ context.Context, f *tunnelpb.Frame) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return transport.ErrClosed
	}
	if err := t.codec.WriteFrame(t.s, f); err != nil {
		t.closed = true
		return err
	}
	return nil
}

func (t *tunnel) RecvFrame(_ context.Context) (*tunnelpb.Frame, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, transport.ErrClosed
	}
	t.mu.Unlock()
	return t.codec.ReadFrame(t.s)
}

func (t *tunnel) Close(code uint32, message string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	f := &tunnelpb.Frame{
		Tag:   tunnelpb.TagClose,
		Close: &tunnelpb.Close{Code: code, Message: message},
	}
	err := t.codec.WriteFrame(t.s, f)
	t.closed = true
	return err
}
