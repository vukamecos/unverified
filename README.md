# VPN tunnel over gRPC

A VPN tunnel that encapsulates IP packets inside gRPC streams.

## Overview

- **Transport:** gRPC over HTTP/2 (bidirectional streaming), optional QUIC, dual-stack on one endpoint
- **Client:** reads packets from a local TUN interface and ships them to the server
- **Server:** decapsulates packets and forwards them to the target network; same port also serves a check-IP HTTP endpoint
- **Language:** Go (single static binary)
- **Distribution:** one static `unvfd` binary — server, client, CA/cert/ACME management all in one file
- **Platform:** Ubuntu / Debian (Linux)
- **Kill switch:** drops all client traffic outside the tunnel (via `nftables` + eBPF)
- **Packet handling:** eBPF (XDP / TC) for fast-path and enforcement
- **In-tunnel firewall:** per-client ACL with declarative DSL — granular allow/deny of traffic inside the tunnel
- **Security posture:** paranoid (mTLS + cert pinning, AEAD, PFS, hybrid post-quantum KEM, leak-proof, hardened process)

## Architecture

```
┌─────────────────┐         gRPC/HTTP2          ┌─────────────────┐
│  VPN Client     │ ◄─────────────────────────► │  VPN Server     │
│  (TUN interface)│   encapsulated IP packets   │  (forwarding)   │
└─────────────────┘                              └─────────────────┘
```

For end-to-end behaviour, see the sequence diagrams:

- Client lifecycle: [`docs/diagrams/client-sequence.puml`](docs/diagrams/client-sequence.puml)
- Server lifecycle: [`docs/diagrams/server-sequence.puml`](docs/diagrams/server-sequence.puml)

## Status

Under development. No source files yet.

## License

MIT — see [LICENSE](./LICENSE).
