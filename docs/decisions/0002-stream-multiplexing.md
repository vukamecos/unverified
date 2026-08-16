# ADR 0002 — Stream multiplexing (one gRPC stream per Tunnel session)

Status: **Accepted** (2026-08-15).

## Context

TODO asks us to decide between **one stream per session** (a single
gRPC bidi stream carries everything for one authenticated session) and
**one stream per connection** (a separate gRPC stream per inner flow —
e.g. one for the handshake, one for each TCP connection inside the
tunnel, one for ICMP, etc., multiplexed under one Tunnel).

The choice affects the framing, the AEAD nonce space, the inner-KEM
binding, and the way conntrack / flow state lives in the data path.

## Decision

**One gRPC bidi stream per authenticated session.** The Tunnel
abstraction (`internal/contract/transport.Tunnel`) IS the gRPC bidi
stream; no inner sub-multiplex.

The codec (ADR 0001) carries both packet records (tag `0x04`/`0x06`)
and control records (tags `0x10..0x14`) on the same wire. The stream
is full-duplex: the client and server both send packet and control
records on it as needed.

## Rationale

1. **1:1 identity binding.** A Tunnel = one authenticated mTLS session
   = one inner KEM session = one AEAD epoch = one nonce space. With
   per-connection sub-streams, the inner-KEM binding (§7.1 of ARCH)
   would have to be re-derived per sub-stream, and the AEAD nonce space
   would be either shared (which makes window-replay across sub-streams
   ambiguous) or split (which means the per-connection key schedule
   diverges from the inner-KEM transcript signature). Both are
   complexity for no win.

2. **gRPC stream cost is one HTTP/2 stream + one TCP/TLS connection.**
   We pay neither cost per inner TCP flow. With HTTP/2, a single TCP
   connection multiplexes up to ~1000 concurrent gRPC streams; with
   QUIC (Mode B in TODO), the same. The "one stream per connection"
   model would mean we open a gRPC stream per TCP flow inside the
   tunnel — that's thousands of streams per typical workload, with
   per-stream TLS handshake work that we already paid for at the
   session layer.

3. **No nested framing.** With a single stream per session, the wire
   format is exactly the codec documented in ADR 0001: tag + body, no
   inner sub-IDs, no per-flow state in the framing. Adding a per-flow
   layer would mean either a TLV-with-flow-id (which we just rejected
   in ADR 0001) or a separate inner-header byte on every packet (which
   is per-packet overhead).

4. **Conntrack lives in userspace.** The firewall / conntrack state
   (TODO §"Connection tracking") is keyed by the 5-tuple inside the
   tunneled IP packet. That keying is independent of the gRPC stream
   identity; one stream carries many 5-tuples, and that is exactly the
   design.

5. **Failure mode is simpler.** When the gRPC connection dies, the
   Tunnel dies, the session dies. There are no half-open sub-streams
   to clean up. When we reconnect, we reconnect the whole session and
   re-establish the inner KEM from scratch.

## What we lose

- **Per-flow backpressure.** HTTP/2 gives per-stream flow control; if
  one inner TCP flow stalls, it cannot stall the rest of the session
  (within HTTP/2's flow window). With one stream per session, all
  flows share the same HTTP/2 flow window. Mitigation: HTTP/2 default
  window is 16–65 KiB per stream and gRPC has its own flow control on
  top; the actual bottleneck for a typical user workload is the inner
  AEAD, not gRPC backpressure. If a future workload needs per-flow
  isolation, revisit this ADR and add a sub-multiplex layer (with a
  flow-id byte on each frame).

- **0-RTT replay safety for individual endpoints.** With one stream,
  we cannot selectively resend a single packet on reconnect without
  replaying the whole session. We don't currently use 0-RTT
  (TODO §"Protocol switching (gRPC ↔ QUIC)": "0-RTT must never carry
  inner-AEAD-protected tunnel packets"), so this is moot. If 0-RTT is
  ever enabled, it carries only the check-IP / control-plane messages,
  not inner-AEAD packets.

## Consequences

- The `transport.Tunnel` interface (§internal/contract/transport) is
  exactly one stream. There is no `Tunnel.SubStream` or `Tunnel.OpenFlow`.

- The codec carries a single ordered sequence of records on each
  direction. Order is preserved; an out-of-order packet inside the
  same stream is a sequencing bug, not a feature.

- Future sub-multiplex (if ever needed) is an additive change: a new
  frame tag for "flow-fragment" plus a 1-byte flow-id prefix on packet
  records. This ADR can be superseded with a new one at that time.

## Alternatives considered

- **One gRPC stream per TCP/UDP flow inside the tunnel.** Highest
  overhead, no benefit (HTTP/2 already multiplexes streams on one TCP
  connection), complicates the inner-KEM binding.

- **One stream for handshake, one for data.** Splits the lifecycle
  state machine into two; the data stream still needs the inner KEM,
  so the handshake stream only carries auth material. No benefit.

- **One stream for control, one stream for packets.** Splits the
  session into two; requires two AEAD instances (or two nonce spaces
  inside one AEAD), two replay windows, two inner-KEM transcripts.
  Complexity without benefit.

## Cross-references

- ADR 0001 (raw framing; this ADR assumes that framing).
- ARCH §4.1 (the `Session` goroutine per Tunnel, one stream).
- ARCH §7.1 (the inner-KEM transcript signature is per-session, not
  per-flow).
- ARCH §7 (AEAD nonce space is per-session, not per-flow).
