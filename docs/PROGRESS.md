# PROGRESS

Append-only log of completed TODO items, newest first. Each entry records
the item, the branch of docs/TODO.md it came from, the commit summary,
and a one-line note on the approach taken.

## 2026-08-15

- **Protocol / Decide on stream multiplexing** — ADR 0002 accepted.
  One gRPC bidi stream per authenticated Tunnel session; the Tunnel
  abstraction IS the gRPC stream. No nested sub-multiplex. Rationale:
  1:1 identity binding (Tunnel = inner-KEM session = AEAD epoch = nonce
  space); HTTP/2 already multiplexes streams on one TCP connection, so
  per-inner-flow streams are pure overhead; conntrack keying is by the
  5-tuple inside the tunneled packet, independent of stream identity.
  Alternatives (per-flow streams, separate handshake stream, separate
  control stream) all explicitly rejected. No code changes; Tunnel
  interface and codec already implement this. build/vet/test still green.
- **Protocol / Decide on packet encoding** — ADR 0001 accepted.
  Raw IPv4/IPv6 framing (single tag byte `0x04`/`0x06`, 2-byte BE length,
  packet bytes verbatim) — already implemented in the codec shipped in
  iter 1. TLV explicitly rejected: extra parser, no flexibility we use,
  per-packet overhead, no protocol-agility benefit we couldn't get with
  one more tag byte. No code changes; `go build`, `go vet`,
  `go test -race` green.
- **Protocol / Define the `.proto` schema for tunnel messages** — committed.
  Added `proto/tunnel.proto` (5 control records: Hello/Session/Keepalive/Rekey/Close;
  raw IPv4/IPv6 packet framing) and a hand-rolled `internal/transport/grpc/tunnelpb/codec.go`
  mirroring the proto field layout byte-for-byte. Wrapper
  `internal/transport/grpc/wrapper.go` exposes the abstract
  `internal/contract/transport.Tunnel` interface backed by any `io.ReadWriter`.
  Tests cover every record type, every error path (short read, unknown tag,
  length-invalid, field-too-long), the EOF-on-peer-close path, and the
  wrapper-level invariants (Close is idempotent, SendFrame after Close
  returns `ErrClosed`). `go build`, `go vet`, `go test -race` all green.
  Approach chosen because `protoc` is not installed in the dev env and
  ARCH §6 prefers stdlib / no third-party codegen — the `.proto` remains
  the source of truth, the codec is hand-mirrored and documented as such.
