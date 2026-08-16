package tunnelpb_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/vukamecos/unverified/internal/transport/grpc/tunnelpb"
)

// roundtrip is a helper: encode f into a buffer, decode it back, return
// the result. Tests use it to keep each case small.
func roundtrip(t *testing.T, f *tunnelpb.Frame) *tunnelpb.Frame {
	t.Helper()
	var buf bytes.Buffer
	c := tunnelpb.New()
	if err := c.WriteFrame(&buf, f); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := c.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return got
}

func TestRoundtripIPv4Packet(t *testing.T) {
	pkt := []byte{0x45, 0x00, 0x00, 0x14, 0xde, 0xad, 0xbe, 0xef}
	f := &tunnelpb.Frame{Tag: tunnelpb.TagIPv4, Packet: pkt}
	got := roundtrip(t, f)
	if got.Tag != tunnelpb.TagIPv4 {
		t.Fatalf("tag = 0x%02x, want 0x%02x", got.Tag, tunnelpb.TagIPv4)
	}
	if !bytes.Equal(got.Packet, pkt) {
		t.Fatalf("packet = %x, want %x", got.Packet, pkt)
	}
}

func TestRoundtripIPv6Packet(t *testing.T) {
	pkt := make([]byte, 64)
	for i := range pkt {
		pkt[i] = byte(i)
	}
	f := &tunnelpb.Frame{Tag: tunnelpb.TagIPv6, Packet: pkt}
	got := roundtrip(t, f)
	if got.Tag != tunnelpb.TagIPv6 {
		t.Fatalf("tag = 0x%02x, want 0x%02x", got.Tag, tunnelpb.TagIPv6)
	}
	if !bytes.Equal(got.Packet, pkt) {
		t.Fatalf("packet mismatch")
	}
}

func TestRoundtripHello(t *testing.T) {
	in := &tunnelpb.Hello{
		ProtocolVersion: 1,
		ClientID:        "alice@example",
		CipherSuite:     1,
	}
	f := &tunnelpb.Frame{Tag: tunnelpb.TagHello, Hello: in}
	got := roundtrip(t, f)
	if got.Hello.ProtocolVersion != in.ProtocolVersion ||
		got.Hello.ClientID != in.ClientID ||
		got.Hello.CipherSuite != in.CipherSuite {
		t.Fatalf("hello roundtrip mismatch: got %+v want %+v", got.Hello, in)
	}
}

func TestRoundtripSession(t *testing.T) {
	in := &tunnelpb.Session{
		AssignedIPv4: net.IPv4(10, 0, 0, 5),
		AssignedIPv6: net.ParseIP("fd00::5"),
		DNSServers: []net.IP{
			net.IPv4(1, 1, 1, 1),
			net.ParseIP("2606:4700:4700::1111"),
		},
		MTU: 1420,
	}
	f := &tunnelpb.Frame{Tag: tunnelpb.TagSession, Session: in}
	got := roundtrip(t, f)
	if !got.Session.AssignedIPv4.Equal(in.AssignedIPv4) {
		t.Fatalf("v4 mismatch: got %v want %v",
			got.Session.AssignedIPv4, in.AssignedIPv4)
	}
	if !got.Session.AssignedIPv6.Equal(in.AssignedIPv6) {
		t.Fatalf("v6 mismatch: got %v want %v",
			got.Session.AssignedIPv6, in.AssignedIPv6)
	}
	if len(got.Session.DNSServers) != 2 {
		t.Fatalf("dns count = %d, want 2", len(got.Session.DNSServers))
	}
	if got.Session.MTU != 1420 {
		t.Fatalf("mtu = %d, want 1420", got.Session.MTU)
	}
}

func TestRoundtripKeepalive(t *testing.T) {
	in := &tunnelpb.Keepalive{Nonce: 0xdeadbeefcafebabe}
	f := &tunnelpb.Frame{Tag: tunnelpb.TagKeepalive, Keepalive: in}
	got := roundtrip(t, f)
	if got.Keepalive.Nonce != in.Nonce {
		t.Fatalf("nonce = %x, want %x", got.Keepalive.Nonce, in.Nonce)
	}
}

func TestRoundtripRekey(t *testing.T) {
	x := make([]byte, 32)
	m := make([]byte, 1184)
	for i := range x {
		x[i] = byte(i)
	}
	for i := range m {
		m[i] = byte(255 - i)
	}
	in := &tunnelpb.Rekey{
		X25519Public:  x,
		MLKEMPublic:   m,
		ActivateAtSeq: 12345,
	}
	f := &tunnelpb.Frame{Tag: tunnelpb.TagRekey, Rekey: in}
	got := roundtrip(t, f)
	if !bytes.Equal(got.Rekey.X25519Public, x) {
		t.Fatalf("x25519 mismatch")
	}
	if !bytes.Equal(got.Rekey.MLKEMPublic, m) {
		t.Fatalf("mlkem mismatch")
	}
	if got.Rekey.ActivateAtSeq != 12345 {
		t.Fatalf("seq = %d, want 12345", got.Rekey.ActivateAtSeq)
	}
}

func TestRoundtripClose(t *testing.T) {
	in := &tunnelpb.Close{Code: 7, Message: "going_away"}
	f := &tunnelpb.Frame{Tag: tunnelpb.TagClose, Close: in}
	got := roundtrip(t, f)
	if got.Close.Code != 7 || got.Close.Message != "going_away" {
		t.Fatalf("close roundtrip mismatch: %+v", got.Close)
	}
}

func TestUnknownTag(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0xfe)
	c := tunnelpb.New()
	_, err := c.ReadFrame(&buf)
	if !errors.Is(err, tunnelpb.ErrUnknownTag) {
		t.Fatalf("err = %v, want ErrUnknownTag", err)
	}
}

func TestShortReadEOF(t *testing.T) {
	c := tunnelpb.New()
	_, err := c.ReadFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty reader: err = %v, want io.EOF", err)
	}
}

func TestShortReadTruncated(t *testing.T) {
	// Tag + half the IPv4 length prefix.
	var buf bytes.Buffer
	buf.WriteByte(tunnelpb.TagIPv4)
	buf.WriteByte(0x00)
	c := tunnelpb.New()
	_, err := c.ReadFrame(&buf)
	if !errors.Is(err, tunnelpb.ErrShortRead) {
		t.Fatalf("truncated: err = %v, want ErrShortRead", err)
	}
}

func TestPacketLengthRejected(t *testing.T) {
	// Empty packet is a framing error.
	var buf bytes.Buffer
	buf.WriteByte(tunnelpb.TagIPv4)
	buf.WriteByte(0x00)
	buf.WriteByte(0x00)
	c := tunnelpb.New()
	_, err := c.ReadFrame(&buf)
	if !errors.Is(err, tunnelpb.ErrLengthInvalid) {
		t.Fatalf("empty pkt: err = %v, want ErrLengthInvalid", err)
	}
}

func TestRekeyLengthEnforced(t *testing.T) {
	c := tunnelpb.New()
	err := c.WriteFrame(&bytes.Buffer{}, &tunnelpb.Frame{
		Tag:  tunnelpb.TagRekey,
		Rekey: &tunnelpb.Rekey{
			X25519Public: make([]byte, 31), // wrong length
			MLKEMPublic:  make([]byte, 1184),
		},
	})
	if !errors.Is(err, tunnelpb.ErrLengthInvalid) {
		t.Fatalf("short x25519: err = %v, want ErrLengthInvalid", err)
	}
}

func TestHelloStringTooLong(t *testing.T) {
	c := tunnelpb.New()
	err := c.WriteFrame(&bytes.Buffer{}, &tunnelpb.Frame{
		Tag: tunnelpb.TagHello,
		Hello: &tunnelpb.Hello{
			ProtocolVersion: 1,
			ClientID:        string(make([]byte, tunnelpb.MaxStringLen+1)),
			CipherSuite:     0,
		},
	})
	if !errors.Is(err, tunnelpb.ErrFieldTooLong) {
		t.Fatalf("long id: err = %v, want ErrFieldTooLong", err)
	}
}
