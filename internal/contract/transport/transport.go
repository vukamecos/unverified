// Package transport defines the contract surface between the tunnel
// session and the transport layer. The transport layer (gRPC / QUIC)
// implements Tunnel, and tunnel/session consumes it. Per ARCH §5, the
// generated wire code is the only place that knows about framing; the
// rest of the codebase operates on this abstract interface.
package transport

import (
	"context"
	"errors"
	"io"

	"github.com/vukamecos/unverified/internal/transport/grpc/tunnelpb"
)

// Tunnel is the abstract bidi stream between client and server. It is
// the seam between the wire-format layer (tunnelpb) and the session
// state machine (tunnel/session). One Tunnel = one authenticated session.
type Tunnel interface {
	// SendFrame writes one frame to the underlying stream. Implementations
	// MUST serialise writes; concurrent calls are not safe.
	SendFrame(ctx context.Context, f *tunnelpb.Frame) error

	// RecvFrame reads the next frame. Returns io.EOF when the peer has
	// closed the stream.
	RecvFrame(ctx context.Context) (*tunnelpb.Frame, error)

	// Close shuts down the stream with a Close frame (best-effort). The
	// stream is unusable after this returns.
	Close(code uint32, message string) error
}

// Listener accepts new Tunnels. The transport layer (gRPC server,
// QUIC listener) implements this; the session layer registers a
// handler that consumes each accepted Tunnel.
type Listener interface {
	// Serve accepts Tunnels in a loop until ctx is cancelled or Accept
	// returns a non-temporary error. Each accepted Tunnel is handed to
	// Handle in a new goroutine.
	Serve(ctx context.Context, handle func(context.Context, Tunnel)) error

	// Close stops accepting and tears down in-flight Tunnels.
	Close() error
}

// AttestationSink receives periodic reports of the daemon's eBPF /
// nftables / process state hash. The server compares the report
// against the expected hash to detect configuration drift on the
// client (ARCH §11.1, TODO §"Audit & monitoring"). It is NOT a defence
// against a malicious client process (the threat model says so).
type AttestationSink interface {
	Report(stateHash [32]byte) error
}

// ErrClosed is returned by SendFrame/RecvFrame after Close.
var ErrClosed = errors.New("transport: stream closed")

// Compile-time guard: io.EOF must be the natural terminator for
// RecvFrame, not a wrapped error.
var _ = io.EOF
