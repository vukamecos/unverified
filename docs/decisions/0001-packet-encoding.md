# ADR 0001 — Packet encoding for the tunnel stream

Status: **Accepted** (2026-08-15).

## Context

The tunnel stream carries IP packets from the client to the server and
back. The TODO asks us to choose between **raw IPv4/IPv6 framing** and a
**TLV-based encoding**. The choice affects every byte that flows over
the inner bidi stream, the size of every record, the code that handles
it, and the size of the threat surface (every parser is a parser bug
waiting to happen).

## Decision

Use **raw IPv4/IPv6 framing** — the packet bytes themselves go on the
wire, prefixed by a single byte (4 or 6) and a 2-byte big-endian length.

The current codec (commit shipped with iter 1) already implements this:

```
0x04  LL LL  <IPv4 packet bytes>
0x06  LL LL  <IPv6 packet bytes>
0x10         ControlHello
0x11         ControlSession
0x12         ControlKeepalive
0x13         ControlRekey
0x14         ControlClose
```

LL is a 2-byte big-endian length prefix (0..65535). For control records
the tag is the same byte but no length prefix — the body is
self-delimiting by type.

## Rationale

1. **No re-encoding. Zero transformation.** The packet the kernel hands
   us from `/dev/net/tun` is what we ship. The server reads those bytes
   and writes them to its own TUN. The kernel handles IP fragmentation,
   header validation, and per-protocol semantics — we don't have to
   re-implement any of that.

2. **No parser attack surface.** A TLV encoding requires a parser that
   understands length fields, possibly nesting, definitely type tags. A
   malformed TLV is a bug class we don't otherwise have. With raw
   framing, the only framing we parse is the (1 tag, 2 length) prefix —
   three bytes — and then we hand the bytes to the kernel, which is the
   authoritative validator.

3. **MTU efficiency.** A TLV wrapper costs at least 4–8 bytes per packet.
   At 1500-byte packets that's noise; at 64-byte packets (DNS, ACK)
   it's a measurable fraction of throughput and a measurable extra
   inner-encryption cost (each byte costs one AEAD block plus nonce
   machinery).

4. **Wire-format simplicity.** The wire format mirrors `proto/tunnel.proto`
   1:1, the codec is hand-mirrored from the proto, and the choice is
   documented at the top of the codec file (see
   `internal/transport/grpc/tunnelpb/codec.go`). Anyone reading either
   file sees the same layout.

## What we lose

- **Per-packet metadata over the wire.** A TLV would let us carry
  out-of-band metadata (a packet sequence number, a class-of-service
  hint, a per-flow label) without re-encrypting a side channel. We
  don't need any of that: the inner AEAD (§7 of ARCH) already carries
  the packet sequence number in its AAD, and the kernel handles the
  rest.

- **Protocol agility at the framing layer.** If we ever want to send
  something other than an IP packet over the tunnel (e.g. an ARP reply,
  a DHCP relay), we'd add a new tag byte. That's a one-line change in
  the codec; we don't need TLV for it.

## Consequences

- The codec rejects unknown tags (0x00, 0xFF, anything not in
  `{0x04, 0x06, 0x10..0x14}`) as `ErrUnknownTag`. The stream is closed
  on such an error; this is the framing boundary, not a retry boundary.

- Length 0 packets are an error (`ErrLengthInvalid`). The kernel does
  not emit empty IP packets; if one appears, the peer is broken or
  malicious.

- Adding a new framing tag (e.g. a future `0x07` for ICMP-only or
  `0x08` for ARP relay) requires updating both `proto/tunnel.proto`
  and `codec.go`, in the same commit, with a test that exercises the
  new tag round-trip.

## Alternatives considered

- **TLV with a 1-byte type + varint length + payload.** More flexible,
  more parser, more attack surface. The flexibility is unused.
- **TLV with fixed alignment.** Slightly cheaper to parse on some CPUs,
  still a parser, still attack surface.
- **Pure length-prefixed (no type).** Smaller wire format, but a single
  mistake means the whole stream is corrupted; type bytes are cheaper
  than that.

## Cross-references

- ARCH §4 (process model, the inner bidi stream is the `Tunnel`
  abstraction).
- ARCH §7.1 (the inner AEAD's AAD binds the packet sequence number;
  this is the only out-of-band metadata that matters on the wire).
- TODO §"Protocol" (this ADR closes the first open item there).
