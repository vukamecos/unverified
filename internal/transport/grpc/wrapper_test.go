package grpc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	transport "github.com/vukamecos/unverified/internal/contract/transport"
	"github.com/vukamecos/unverified/internal/transport/grpc"
	"github.com/vukamecos/unverified/internal/transport/grpc/tunnelpb"
)

// pipe is an in-memory Stream that lets two sides talk. Each side
// reads the other's writes: sideA reads from its peer's writer, sideB
// reads from sideA's writer.
type pipe struct {
	a, b *bytes.Buffer
}

// side A writes to b, reads from a.
func (p *pipe) Read(pb []byte) (int, error)  { return p.a.Read(pb) }
func (p *pipe) Write(pb []byte) (int, error) { return p.b.Write(pb) }

// side B writes to a, reads from b.
type other struct{ p *pipe }

func (o *other) Read(pb []byte) (int, error)  { return o.p.b.Read(pb) }
func (o *other) Write(pb []byte) (int, error) { return o.p.a.Write(pb) }

// newPipe returns (client-side Stream, server-side Stream).
func newPipe() (grpc.Stream, grpc.Stream) {
	p := &pipe{a: &bytes.Buffer{}, b: &bytes.Buffer{}}
	return p, &other{p: p}
}

func TestWrapperRoundtrip(t *testing.T) {
	client, server := newPipe()
	ct := grpc.Wrap(client)
	st := grpc.Wrap(server)

	ctx := context.Background()

	in := &tunnelpb.Frame{
		Tag:   tunnelpb.TagHello,
		Hello: &tunnelpb.Hello{ProtocolVersion: 1, ClientID: "bob", CipherSuite: 0},
	}
	if err := ct.SendFrame(ctx, in); err != nil {
		t.Fatalf("SendFrame: %v", err)
	}
	got, err := st.RecvFrame(ctx)
	if err != nil {
		t.Fatalf("RecvFrame: %v", err)
	}
	if got.Hello.ClientID != "bob" {
		t.Fatalf("client id = %q, want bob", got.Hello.ClientID)
	}
}

func TestWrapperCloseIsIdempotent(t *testing.T) {
	client, server := newPipe()
	ct := grpc.Wrap(client)
	st := grpc.Wrap(server)

	ctx := context.Background()
	if err := st.Close(0, "bye"); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Server should see a Close frame.
	got, err := ct.RecvFrame(ctx)
	if err != nil {
		t.Fatalf("RecvFrame after Close: %v", err)
	}
	if got.Tag != tunnelpb.TagClose {
		t.Fatalf("tag = 0x%02x, want TagClose", got.Tag)
	}
	// And a second Close on the same side is a no-op.
	if err := st.Close(0, "bye again"); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestWrapperClosedReturnsErrClosed(t *testing.T) {
	client, _ := newPipe()
	ct := grpc.Wrap(client)
	if err := ct.Close(0, "x"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := ct.SendFrame(context.Background(), &tunnelpb.Frame{
		Tag:    tunnelpb.TagIPv4,
		Packet: []byte{1, 2, 3, 4},
	})
	if !errors.Is(err, transport.ErrClosed) {
		t.Fatalf("SendFrame after Close: err = %v, want ErrClosed", err)
	}
}

func TestWrapperRecvEOFOnPeerClose(t *testing.T) {
	_, server := newPipe()
	// Drop the client side; the server should observe EOF on read.
	st := grpc.Wrap(server)
	got, err := st.RecvFrame(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v (%+v), want io.EOF", err, got)
	}
}
