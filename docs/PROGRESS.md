# PROGRESS

Append-only log of completed TODO items, newest first. Each entry records
the item, the branch of docs/TODO.md it came from, the commit summary,
and a one-line note on the approach taken.

## 2026-08-16

- **Client (Ubuntu/Debian) / Create TUN interface — pin dep + minimal package** — shipped.
  After writing the wrapper on top of gvisor's `pkg/tcpip/link/tun`, the
  open path turned out to be three syscalls (`unix.Open("/dev/net/tun",
  O_RDWR, 0)` + `SYS_IOCTL(fd, TUNSETIFF, ifreq)` + `SetNonblock`),
  wrapping a `struct ifreq`. Pulling gvisor in for one wrapper function
  is wrong: it brings the netstack / buffer / waiter / stateify
  transitive surface for zero benefit. Revised ADR 0003 accordingly —
  the implementation now uses stdlib `syscall` + `golang.org/x/sys/unix`
  (a foundational Go module the rest of the binary pulls in anyway for
  netlink / nftables wrappers). `gvisor.dev/gvisor` is **not** in
  `go.mod`; `go mod tidy` removed it after the gvisor import was
  dropped. Three files added:
  - `internal/contract/tun/tun.go` — abstract `Device` interface
    (`Name`, `MTU`, `Read`, `Write`, `Close`) + sentinel `ErrClosed`.
    Owns the contract; nothing about `/dev/net/tun` leaks out.
  - `internal/tunnel/tun/tun_linux.go` — `//go:build linux`. Three
    syscalls, an `*os.File` wrap with `atomic.Bool` for idempotent
    Close, and an `SIOCGIFMTU` lookup via `unix.Ifreq.Uint32()`. The
    resolved interface name is captured from the ifreq's `name[16]`
    field after the ioctl runs.
  - `internal/tunnel/tun/tun_other.go` — `//go:build !linux` stub that
    returns a typed error from `Open` so cross-compile stays green.
  - `internal/contract/tun/tun_test.go` — 7 contract-level tests
    exercising Name/MTU/Read/Write/Close + the central
    `ErrClosed`-after-Close property + Close idempotence + Close
    error-surfacing semantics. Hand-rolled fake (interface is small
    enough that moq would be more code, not less; the future moq rule
    targets larger interfaces).
  build/vet/test/-race all green.

## 2026-08-15

- **Client (Ubuntu/Debian) / Create TUN interface — choose the library** — ADR 0003 accepted.
  Resolves to `gvisor.dev/gvisor/pkg/tcpip/link/tun`. Rationale, in
  priority order: (1) `sing-tun` is GPL-3.0-or-later, which would
  propagate to the whole static binary — unacceptable for a
  re-distributable single-binary daemon; `gvisor` is Apache-2.0 /
  BSD-3 / MIT. (2) `sing-tun` pulls in six extra Go modules
  (sagernet gvisor, netlink, nftables, sing, mdlayher netlink, go-nfqueue)
  for one capability; gvisor's TUN sub-package adds only gvisor's own
  internal packages. (3) Linux-only is fine — ARCH §1 is Linux-only.
  (4) `tun.Device` exposes the primitives we actually need (raw fd +
  ioctl + Read/Write/MTU); sing-tun's broader API (packet batching,
  ICMP-error forwarding, traceroute) is upstream feature work that
  does not belong in our data path. (5) No CGO. (6) gvisor was
  released 2026-08-15, actively maintained. The TODO parent item was
  split into four sub-items (library choice, dependency pin + minimal
  package, capability probe, interface-based unit test); this iter
  closed the first. No code changes; build/vet/test/-race still green.
- **Protocol / Document handshake / authentication flow** — committed.

- **Protocol / Document handshake / authentication flow** — committed.
  Added `docs/handshake.md`, the single source of truth for the order
  of operations that brings a client and server from TCP open to a
  Tunnel ready for IP packets. Five phases: TCP → TLS 1.3 mTLS (hybrid
  KEM, hybrid cert chain via `InsecureSkipVerify`+`VerifyPeerCertificate`
  per ARCH §7.2) → gRPC stream open → inner KEM + transcript signature
  (ML-DSA-65 over canonicalised transcript, domain-separated
  `"unvfd/v1/inner-kem-transcript"`) → AEAD key derivation (HKDF-SHA-512,
  per-direction info strings). Each phase's preconditions, what it
  proves, and what it does NOT cover are documented. No code changes;
  build/vet/test/-race still green.
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
