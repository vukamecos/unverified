// Package tunnelpb holds the wire-format codec for the tunnel stream.
//
// It mirrors proto/tunnel.proto (the source of truth) and is hand-written
// instead of generated, because protoc is not available in the build
// environment and the design (ARCH §6) prefers stdlib / no extra codegen
// dependencies over third-party generators. Any change to a field number,
// enum value, or framing byte MUST be mirrored in the .proto file.
//
// The framing is one byte tag at the start of every record:
//   0x04          -> IPv4 packet, 2-byte big-endian length, packet bytes
//   0x06          -> IPv6 packet, 2-byte big-endian length, packet bytes
//   0x10          -> ControlHello
//   0x11          -> ControlSession
//   0x12          -> ControlKeepalive
//   0x13          -> ControlRekey
//   0x14          -> ControlClose
//
// 0x00, 0xFF, or any unrecognised tag is a framing error.
package tunnelpb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// Tag bytes.
const (
	TagIPv4     byte = 0x04
	TagIPv6     byte = 0x06
	TagHello    byte = 0x10
	TagSession  byte = 0x11
	TagKeepalive byte = 0x12
	TagRekey    byte = 0x13
	TagClose    byte = 0x14
)

// Hard limits from the proto definition / wire format.
const (
	MaxIPPacket   = 65535
	MaxStringLen  = 255
	MaxDNSEntries = 4
	MaxRecordBody = MaxIPPacket // generous upper bound for any control record
)

// Frame is the union of all in-band records. Exactly one of the data
// fields is set per Frame, determined by the Tag.
//
// All slice fields are owned by the Frame and may be retained by the
// caller without copying; the codec allocates fresh slices on each
// ReadFrame call. Callers must copy before mutating if they need to keep
// the payload across calls.
type Frame struct {
	Tag byte

	// Packet is set for TagIPv4 / TagIPv6. The slice holds the IP packet
	// verbatim (including the IP header).
	Packet []byte

	Hello     *Hello
	Session   *Session
	Keepalive *Keepalive
	Rekey     *Rekey
	Close     *Close
}

type Hello struct {
	ProtocolVersion uint32
	ClientID        string // max 255 bytes
	CipherSuite     uint32 // 0 = AES-256-GCM, 1 = ChaCha20-Poly1305
}

type Session struct {
	AssignedIPv4 net.IP // 4 bytes
	AssignedIPv6 net.IP // 16 bytes
	DNSServers   []net.IP
	MTU          uint32
}

type Keepalive struct {
	Nonce uint64
}

type Rekey struct {
	X25519Public   []byte // 32 bytes
	MLKEMPublic    []byte // 1184 bytes (ML-KEM-768)
	ActivateAtSeq  uint64
}

type Close struct {
	Code    uint32
	Message string // max 255 bytes
}

// Errors exposed to callers; tests assert on these.
var (
	ErrShortRead     = errors.New("tunnelpb: short read")
	ErrUnknownTag    = errors.New("tunnelpb: unknown tag")
	ErrLengthInvalid = errors.New("tunnelpb: length invalid")
	ErrFieldTooLong  = errors.New("tunnelpb: field exceeds declared max")
)

// Codec reads/writes Frames over an io.ReadWriter (typically a gRPC
// bidi stream). Codec is stateless; safe for concurrent use.
type Codec struct{}

// New returns a Codec.
func New() Codec { return Codec{} }

// WriteFrame encodes f into w. It returns the number of bytes written
// and any error. The encoded form is one tag byte followed by the
// record-specific body; for packet records the body is a 2-byte
// big-endian length prefix and the packet bytes.
func (Codec) WriteFrame(w io.Writer, f *Frame) error {
	switch f.Tag {
	case TagIPv4, TagIPv6:
		if len(f.Packet) > MaxIPPacket {
			return fmt.Errorf("tunnelpb: packet length %d exceeds max %d: %w",
				len(f.Packet), MaxIPPacket, ErrFieldTooLong)
		}
		if _, err := w.Write([]byte{f.Tag}); err != nil {
			return err
		}
		var lenBuf [2]byte
		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(f.Packet)))
		if _, err := w.Write(lenBuf[:]); err != nil {
			return err
		}
		if _, err := w.Write(f.Packet); err != nil {
			return err
		}
		return nil

	case TagHello:
		if _, err := w.Write([]byte{f.Tag}); err != nil {
			return err
		}
		return writeHello(w, f.Hello)
	case TagSession:
		if _, err := w.Write([]byte{f.Tag}); err != nil {
			return err
		}
		return writeSession(w, f.Session)
	case TagKeepalive:
		if _, err := w.Write([]byte{f.Tag}); err != nil {
			return err
		}
		return writeKeepalive(w, f.Keepalive)
	case TagRekey:
		if _, err := w.Write([]byte{f.Tag}); err != nil {
			return err
		}
		return writeRekey(w, f.Rekey)
	case TagClose:
		if _, err := w.Write([]byte{f.Tag}); err != nil {
			return err
		}
		return writeClose(w, f.Close)
	default:
		return fmt.Errorf("%w: 0x%02x", ErrUnknownTag, f.Tag)
	}
}

// ReadFrame decodes one record from r. It returns io.EOF only when no
// bytes have been read; a partial frame returns ErrShortRead.
func (Codec) ReadFrame(r io.Reader) (*Frame, error) {
	var tagBuf [1]byte
	if _, err := io.ReadFull(r, tagBuf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: tag byte", ErrShortRead)
		}
		return nil, err
	}
	tag := tagBuf[0]
	switch tag {
	case TagIPv4, TagIPv6:
		var lenBuf [2]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return nil, fmt.Errorf("%w: packet length: %v", ErrShortRead, err)
		}
		n := binary.BigEndian.Uint16(lenBuf[:])
		if n == 0 {
			return nil, fmt.Errorf("%w: zero-length packet", ErrLengthInvalid)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("%w: packet body: %v", ErrShortRead, err)
		}
		return &Frame{Tag: tag, Packet: buf}, nil

	case TagHello:
		h, err := readHello(r)
		return &Frame{Tag: tag, Hello: h}, err
	case TagSession:
		s, err := readSession(r)
		return &Frame{Tag: tag, Session: s}, err
	case TagKeepalive:
		k, err := readKeepalive(r)
		return &Frame{Tag: tag, Keepalive: k}, err
	case TagRekey:
		rk, err := readRekey(r)
		return &Frame{Tag: tag, Rekey: rk}, err
	case TagClose:
		c, err := readClose(r)
		return &Frame{Tag: tag, Close: c}, err
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownTag, tag)
	}
}

// ---------------------------------------------------------------------------
// Control records
// ---------------------------------------------------------------------------
//
// All control records share a compact layout: one tag byte (already
// consumed by ReadFrame), followed by field-by-field encoding. Each
// length-prefixed field (strings, bytes) carries a single-byte length
// (max 255). uint32/uint64 are big-endian.

func writeHello(w io.Writer, h *Hello) error {
	if h == nil {
		return errors.New("tunnelpb: nil Hello")
	}
	if len(h.ClientID) > MaxStringLen {
		return fmt.Errorf("tunnelpb: Hello.ClientID %d > %d: %w",
			len(h.ClientID), MaxStringLen, ErrFieldTooLong)
	}
	var head [4 + 1 + 4]byte
	binary.BigEndian.PutUint32(head[0:4], h.ProtocolVersion)
	head[4] = byte(len(h.ClientID))
	binary.BigEndian.PutUint32(head[5:9], h.CipherSuite)
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if _, err := w.Write([]byte(h.ClientID)); err != nil {
		return err
	}
	return nil
}

func readHello(r io.Reader) (*Hello, error) {
	var head [9]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("%w: Hello head: %v", ErrShortRead, err)
	}
	idLen := int(head[4])
	id := make([]byte, idLen)
	if idLen > 0 {
		if _, err := io.ReadFull(r, id); err != nil {
			return nil, fmt.Errorf("%w: Hello.ClientID: %v", ErrShortRead, err)
		}
	}
	return &Hello{
		ProtocolVersion: binary.BigEndian.Uint32(head[0:4]),
		ClientID:        string(id),
		CipherSuite:     binary.BigEndian.Uint32(head[5:9]),
	}, nil
}

func writeSession(w io.Writer, s *Session) error {
	if s == nil {
		return errors.New("tunnelpb: nil Session")
	}
	if len(s.DNSServers) > MaxDNSEntries {
		return fmt.Errorf("tunnelpb: Session.DNSServers %d > %d: %w",
			len(s.DNSServers), MaxDNSEntries, ErrFieldTooLong)
	}
	var head [1 + 4]byte
	head[0] = byte(len(s.DNSServers))
	binary.BigEndian.PutUint32(head[1:5], s.MTU)
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	// IPv4: 0 bytes = unset, 4 bytes = present.
	if err := writeIP(w, s.AssignedIPv4.To4(), 4); err != nil {
		return fmt.Errorf("tunnelpb: Session.AssignedIPv4: %w", err)
	}
	// IPv6: 0 bytes = unset, 16 bytes = present. Use 16-byte form so
	// v4-mapped v6 addresses don't get mistaken for v4.
	if err := writeIP16(w, s.AssignedIPv6, 16); err != nil {
		return fmt.Errorf("tunnelpb: Session.AssignedIPv6: %w", err)
	}
	for i, dns := range s.DNSServers {
		ip := dns.To16()
		if ip == nil {
			return fmt.Errorf("tunnelpb: Session.DNSServers[%d]: invalid IP", i)
		}
		if _, err := w.Write(ip); err != nil {
			return err
		}
	}
	return nil
}

func readSession(r io.Reader) (*Session, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("%w: Session head: %v", ErrShortRead, err)
	}
	dnsCount := int(head[0])
	if dnsCount > MaxDNSEntries {
		return nil, fmt.Errorf("tunnelpb: Session.DNSServers count %d > %d: %w",
			dnsCount, MaxDNSEntries, ErrFieldTooLong)
	}
	mtu := binary.BigEndian.Uint32(head[1:5])
	v4, err := readIP(r, 4)
	if err != nil {
		return nil, fmt.Errorf("tunnelpb: Session.AssignedIPv4: %w", err)
	}
	v6, err := readIP16(r, 16)
	if err != nil {
		return nil, fmt.Errorf("tunnelpb: Session.AssignedIPv6: %w", err)
	}
	dns := make([]net.IP, dnsCount)
	for i := 0; i < dnsCount; i++ {
		buf := make([]byte, 16)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("%w: Session.DNSServers[%d]: %v", ErrShortRead, i, err)
		}
		dns[i] = net.IP(buf)
	}
	return &Session{
		AssignedIPv4: v4,
		AssignedIPv6: v6,
		DNSServers:   dns,
		MTU:          mtu,
	}, nil
}

func writeKeepalive(w io.Writer, k *Keepalive) error {
	if k == nil {
		return errors.New("tunnelpb: nil Keepalive")
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], k.Nonce)
	_, err := w.Write(buf[:])
	return err
}

func readKeepalive(r io.Reader) (*Keepalive, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, fmt.Errorf("%w: Keepalive: %v", ErrShortRead, err)
	}
	return &Keepalive{Nonce: binary.BigEndian.Uint64(buf[:])}, nil
}

func writeRekey(w io.Writer, rk *Rekey) error {
	if rk == nil {
		return errors.New("tunnelpb: nil Rekey")
	}
	if len(rk.X25519Public) != 32 {
		return fmt.Errorf("tunnelpb: Rekey.X25519Public length %d != 32: %w",
			len(rk.X25519Public), ErrLengthInvalid)
	}
	// ML-KEM-768 public key length is 1184 bytes per FIPS 203.
	if len(rk.MLKEMPublic) != 1184 {
		return fmt.Errorf("tunnelpb: Rekey.MLKEMPublic length %d != 1184: %w",
			len(rk.MLKEMPublic), ErrLengthInvalid)
	}
	if _, err := w.Write(rk.X25519Public); err != nil {
		return err
	}
	if _, err := w.Write(rk.MLKEMPublic); err != nil {
		return err
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], rk.ActivateAtSeq)
	_, err := w.Write(buf[:])
	return err
}

func readRekey(r io.Reader) (*Rekey, error) {
	x := make([]byte, 32)
	if _, err := io.ReadFull(r, x); err != nil {
		return nil, fmt.Errorf("%w: Rekey.X25519Public: %v", ErrShortRead, err)
	}
	m := make([]byte, 1184)
	if _, err := io.ReadFull(r, m); err != nil {
		return nil, fmt.Errorf("%w: Rekey.MLKEMPublic: %v", ErrShortRead, err)
	}
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, fmt.Errorf("%w: Rekey.ActivateAtSeq: %v", ErrShortRead, err)
	}
	return &Rekey{
		X25519Public:  x,
		MLKEMPublic:   m,
		ActivateAtSeq: binary.BigEndian.Uint64(buf[:]),
	}, nil
}

func writeClose(w io.Writer, c *Close) error {
	if c == nil {
		return errors.New("tunnelpb: nil Close")
	}
	if len(c.Message) > MaxStringLen {
		return fmt.Errorf("tunnelpb: Close.Message %d > %d: %w",
			len(c.Message), MaxStringLen, ErrFieldTooLong)
	}
	var head [4 + 1]byte
	binary.BigEndian.PutUint32(head[0:4], c.Code)
	head[4] = byte(len(c.Message))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if _, err := w.Write([]byte(c.Message)); err != nil {
		return err
	}
	return nil
}

func readClose(r io.Reader) (*Close, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("%w: Close head: %v", ErrShortRead, err)
	}
	msgLen := int(head[4])
	msg := make([]byte, msgLen)
	if msgLen > 0 {
		if _, err := io.ReadFull(r, msg); err != nil {
			return nil, fmt.Errorf("%w: Close.Message: %v", ErrShortRead, err)
		}
	}
	return &Close{
		Code:    binary.BigEndian.Uint32(head[0:4]),
		Message: string(msg),
	}, nil
}

// ---------------------------------------------------------------------------
// IP helpers
// ---------------------------------------------------------------------------

func writeIP(w io.Writer, ip net.IP, want int) error {
	if len(ip) == 0 {
		_, err := w.Write([]byte{0})
		return err
	}
	if len(ip) != want {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrLengthInvalid, len(ip), want)
	}
	if _, err := w.Write([]byte{byte(want)}); err != nil {
		return err
	}
	_, err := w.Write(ip)
	return err
}

func writeIP16(w io.Writer, ip net.IP, want int) error {
	// Force 16-byte form for IPv6.
	if len(ip) == 0 {
		_, err := w.Write([]byte{0})
		return err
	}
	v6 := ip.To16()
	if v6 == nil {
		return fmt.Errorf("%w: not a valid IPv6 address", ErrLengthInvalid)
	}
	if len(v6) != want {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrLengthInvalid, len(v6), want)
	}
	if _, err := w.Write([]byte{byte(want)}); err != nil {
		return err
	}
	_, err := w.Write(v6)
	return err
}

func readIP(r io.Reader, want int) (net.IP, error) {
	var lenBuf [1]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("length byte: %w", err)
	}
	if lenBuf[0] == 0 {
		return nil, nil
	}
	if int(lenBuf[0]) != want {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrLengthInvalid, lenBuf[0], want)
	}
	buf := make([]byte, want)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("body: %w", err)
	}
	return net.IP(buf), nil
}

func readIP16(r io.Reader, want int) (net.IP, error) {
	// Same shape as readIP but kept separate to make the v6 path obvious.
	var lenBuf [1]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("length byte: %w", err)
	}
	if lenBuf[0] == 0 {
		return nil, nil
	}
	if int(lenBuf[0]) != want {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrLengthInvalid, lenBuf[0], want)
	}
	buf := make([]byte, want)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("body: %w", err)
	}
	return net.IP(buf), nil
}
