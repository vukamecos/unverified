# Handshake and authentication

> Companion to [`docs/ARCH.md`](ARCH.md) §4, §7, §7.1, §7.2 and
> [`docs/TODO.md`](TODO.md) §"Protocol".
> The per-step sequence diagram is in
> [`docs/diagrams/client-sequence.puml`](diagrams/client-sequence.puml)
> and [`docs/diagrams/server-sequence.puml`](diagrams/server-sequence.puml).

This document is the **single source of truth** for the order of
operations that brings a client and a server from "I have a TCP socket"
to "I have a Tunnel ready to forward IP packets." Code conforms to this
document; this document conforms to ARCH.md (when the two disagree,
ARCH wins).

## Overview

Five phases, in order. None may be skipped. If any phase fails, the
session is aborted and the gRPC stream is closed without sending any
inner-AEAD-protected traffic.

```
   ┌─────────�                                       ┌─────────┐
   │ client  │                                       │ server  │
   └────┬────┘                                       └────┬────┘
        │                                                 │
        │  1. TCP open                                    │
        │ ──────────────────────────────────────────────► │
        │                                                 │
        │  2. TLS 1.3 handshake (mTLS, hybrid KEM)        │
        │ ◄──────────────────────────────────────────────►│
        │   - ClientHello / ServerHello                   │
        │   - Certificate + CertificateVerify (Ed25519)   │
        │   - Finished                                    │
        │                                                 │
        │  3. gRPC: open Tunnel bidi stream               │
        │ ──────────────────────────────────────────────► │
        │                                                 │
        │  4. inner KEM + transcript signature           │
        │   - ControlHello → ControlSession               │
        │   - Rekey (X25519 + ML-KEM-768 publics)         │
        │   - Transcript signature (ML-DSA-65)            │
        │   - Hybrid-cert chain validation (app layer)    │
        │ ◄──────────────────────────────────────────────►│
        │                                                 │
        │  5. AEAD keys derived, session established      │
        │   - Client may now send / receive packets       │
        │                                                 │
        │  ControlKeepalive / ControlRekey / ControlClose │
        │ ◄──────────────────────────────────────────────►│  (lifetime)
        │                                                 │
```

The kill switch (§8.1.1 of ARCH) is up **before phase 2** on the
client, so an attempt to leak outside the tunnel during the handshake
is dropped at the kernel layer.

## Phase 1 — TCP open

The client opens a TCP connection to the server's listener address.
ALPN is not yet negotiable; both sides wait for TLS to speak first.

**Client side.** Refuse to proceed if:
- The destination IP is not in the kill-switch allow-list (TODO
  §"Kill switch").
- The local clock has jumped backwards by more than `max_clock_jump`
  (default 24 h, TODO §"Threat model documentation").

**Server side.** The TCP accept is rate-limited per source IP at
XDP/TC (§7.3 of ARCH) before reaching userspace. Connections beyond
the per-IP / per-ASN budget are dropped with a `BUSY` error and a
`Retry-After` hint.

## Phase 2 — TLS 1.3 handshake (mTLS, hybrid PQC KEM)

This is the transport-layer authentication. Both sides speak standard
TLS 1.3 (RFC 8446) with:

- **Cipher suite:** `TLS_AES_256_GCM_SHA384` or
  `TLS_CHACHA20_POLY1305_SHA256` (the two TLS 1.3 suites; the
  transport-layer AEAD is unrelated to the inner AEAD — the inner
  AEAD is what protects tunnel contents).
- **Key share:** `X25519MLKEM768` (Go 1.26 `crypto/tls` default).
  This is the hybrid KEM that gives post-quantum forward secrecy at
  the transport layer.
- **Signatures:** The certificate chain carries a hybrid Ed25519 +
  ML-DSA-65 public key per cert. The TLS-level signature path (the
  `CertificateVerify` message) uses the **Ed25519 half only** because
  `crypto/tls` in Go 1.26 cannot yet negotiate a hybrid signature
  algorithm — `golang/go#78888` tracks the upstream change. The
  ML-DSA-65 half is verified at the app layer (phase 4).

### 2.1 Server side

The server presents its cert chain. The chain is built by
`internal/contract/pki/hybrid` and validated by our own chain validator
(see §7.2 of ARCH), not by `crypto/x509`:
- The cert bytes are parsed by the hybrid parser.
- Both signatures (Ed25519 + ML-DSA-65) on every cert must verify
  against the issuer's corresponding public key.
- Expiry, EKU (`serverAuth`), `pathlen:0`, `nameConstraints`,
  `cRLDistributionPoints`, `authorityInformationAccess` (OCSP URL) are
  checked.
- The peer cert's SPKI hash is compared against the pinned value from
  the client config (TODO §"Transport hardening": "Pin server
  certificate / public key on the client").

### 2.2 Client side

The client presents its cert chain (mutual TLS). The server validates
the client cert with the same hybrid parser / validator. Expiry, EKU
(`clientAuth`), `pathlen:0`, `nameConstraints`, CRL/OCSP are checked.
A failing client cert chain is a hard reject.

### 2.3 `InsecureSkipVerify` + `VerifyPeerCertificate`

The TLS config in `internal/transport/tls` sets
`InsecureSkipVerify: true` and supplies a `VerifyPeerCertificate`
callback that does the hybrid chain validation described above. This
is the deliberate carve-out noted in ARCH §13.1; it is the only place
a `//nolint:gosec` for G402 is permitted.

### 2.4 CRL / OCSP

Both sides check revocation on every cert in the chain:

- **CRL:** `thisUpdate` is in the past and within `max_clock_skew`
  (default 24 h) of local time, `nextUpdate` is present and in the
  future, signature verifies (both Ed25519 and ML-DSA-65 halves), and
  the serial being checked falls within the CRL's scope. A CRL that
  fails any of these checks is treated as **no CRL** — the cert is
  treated as not revocation-checked, and the connection is refused
  (fail-closed). This blocks the "freeze-the-CRL" attack (TODO §"PKI
  storage & backend").
- **OCSP:** responses are signature-verified (both halves) and
  freshness-checked (`producedAt` within skew). Cached responses
  expire at `nextUpdate` and never outlive it.

### 2.5 Out of phase 2

If phase 2 completes successfully, the TCP socket now carries an
authenticated TLS session between two identities whose long-term keys
are bound to certs in the operator's PKI. The inner-AEAD-protected
tunnel contents are **not** yet safe — a TLS compromise at handshake
time (rogue CA, stolen server key at this exact moment) means the
inner handshake below is also MITM-able. Phase 4 closes that hole.

## Phase 3 — gRPC: open Tunnel bidi stream

The client opens a gRPC bidi stream against the abstract
`Tunnel` RPC (one stream, per ADR 0002). The gRPC client library
hands back a `Stream` that satisfies the
`internal/transport/grpc.Stream` interface (`io.ReadWriter`).

The stream is empty at this point. Nothing has been written.

## Phase 4 — Inner KEM + transcript signature

This is the **inner** key exchange. It is independent of the TLS key
exchange: an ephemeral X25519 + ML-KEM-768 key pair on each side,
combined with a transcript signature bound to the TLS handshake. The
resulting shared secret feeds the inner AEAD (phase 5).

### 4.1 Sequence

1. **Client → Server: `ControlHello`** (codec tag `0x10`)
   - `protocol_version`: must match server's accepted range (currently
     `1`).
   - `client_id`: the CN from the client cert.
   - `cipher_suite`: `0` for AES-256-GCM, `1` for ChaCha20-Poly1305.
     The server may downgrade to ChaCha20 if AES-NI is unavailable on
     either side.

2. **Server validates the Hello.** It checks:
   - Client cert chain has been validated (phase 2.2 done).
   - Client identity (`client_id`) matches the cert CN (prevents a
     compromised client from impersonating another client_id).
   - Requested cipher suite is on the allow-list.

3. **Server allocates an IP from IPAM** (TODO §"Server / Per-client
   IP assignment / subnet management"). Lease is bound to the
   authenticated identity.

4. **Client ↔ Server: `ControlRekey`** (codec tag `0x13`) — both
   directions exchange their **ephemeral** inner-KEM public keys:
   - 32-byte X25519 public key.
   - 1184-byte ML-KEM-768 public key (FIPS 203 §8 size).
   - `activate_at_seq`: 0 (we activate immediately, before any inner
     AEAD traffic).

   The Rekey message is itself protected by nothing — it is the
   handshake — so it MUST be sent before the inner AEAD is active.

5. **Both sides derive the inner shared secret.**
   - X25519: ECDH → 32-byte shared secret.
   - ML-KEM-768: encapsulate → 32-byte shared secret.
   - **HKDF-SHA-512** over the concatenation `client_ephemeral_pub ||
     server_ephemeral_pub || x25519_ss || mlkem_ss || transcript`
     yields a single 32-byte input keying material (IKM). HKDF info
     string: `"unvfd/v1/inner-kem"` (versioned, per ARCH §7.1's
     domain-separation rule).

6. **Both sides sign the transcript** (the **MUST** in ARCH §7.1):
   - Transcript = canonical concatenation of:
     - Client inner-KEM public key (32 + 1184 bytes)
     - Server inner-KEM public key (32 + 1184 bytes)
     - TLS `Finished` value from phase 2
     - Negotiated cipher suite
     - Protocol version
   - The signature input is prefixed with the domain-separation
     string `"unvfd/v1/inner-kem-transcript"` (per ARCH §7.1).
   - The signature is over the canonicalised transcript using the
     **ML-DSA-65 half of the long-term hybrid key**. The Ed25519 half
     is what signed TLS `CertificateVerify`; using the ML-DSA-65 half
     here gives an inner-layer proof-of-possession that is independent
     of the TLS layer's signature.
   - The signature is sent over the stream as a `ControlRekey` follow-up
     (a future ADR will allocate a dedicated frame; for now it travels
     inside the codec's reserved range). Signature verification MUST
     succeed before any inner-AEAD traffic is accepted. A failed
     verification closes the stream.

7. **Server → Client: `ControlSession`** (codec tag `0x11`)
   - `assigned_ipv4` / `assigned_ipv6`: the leased IPs.
   - `dns_servers`: the per-client DNS resolvers.
   - `mtu`: the tunnel MTU (default 1420).

   This message travels under the same handshake protection (none, in
   the codec) — it does not yet carry inner-AEAD traffic.

### 4.2 What phase 4 proves

After phase 4 completes, both sides know:
- The peer holds the **Ed25519 private key** (TLS `CertificateVerify`).
- The peer holds the **ML-DSA-65 private key** (transcript signature).
- Both keys are bound to the same identity (the hybrid cert is
  well-formed).
- The two inner-KEM ephemeral public keys are the ones that produced
  the shared secret — a MITM cannot have replaced either side's
  ephemeral key without invalidating the transcript signature.

A successful phase 4 therefore bounds the TLS-key-compromise scenario
of ARCH §2.3: a passive TLS-key compromise is fully covered (the inner
KEM is independent); an **active** TLS-key compromise is bounded to
the moment of compromise and the moment of transcript-signature
verification, because the transcript signature proves the peer was the
holder of the long-term ML-DSA-65 key at that moment.

### 4.3 What phase 4 does NOT cover

- A compromised server at any time (ARCH §12.1): the server sees
  plaintext in both encryption layers; no client-side trick recovers
  that.
- A compromised CA that mints a cert the client trusts (the client
  pins the server's SPKI in config; a new server install with the
  same hostname but a new key does not get accepted).
- A compromised client process.

## Phase 5 — AEAD keys derived, session established

The HKDF in step 4.5 yielded an IKM. From that IKM, HKDF-SHA-512
extracts two 32-byte AEAD keys:

```
info_send_c2s = "unvfd/v1/aead/c2s/" + cipher_suite_name
info_send_s2c = "unvfd/v1/aead/s2c/" + cipher_suite_name
```

(The `/v1/` segment bumps on any canonicalised change to this scheme,
so that a v1 AEAD key cannot be replayed against a v2 protocol.)

Per-direction keys mean a single-nonce reuse never crosses directions
(GCM and ChaCha20-Poly1305 are both catastrophic on nonce reuse
across encryptions). Per ARCH §7 the AEAD is:

- **AES-256-GCM** if AES-NI is available on the current CPU.
- **ChaCha20-Poly1305** otherwise (ARM, mobile).

The inner AEAD's AAD binds `protocol_version || cipher_suite ||
direction || session_id || seq` (ARCH §7.1). The first byte after
phase 5 is a 96-bit nonce starting at zero, incremented per packet
record, with explicit rekey on wraparound (TODO §"Key rotation (PFS)":
2^28 packets or 1 hour, whichever comes first).

After phase 5:
- The client may send `TagIPv4` / `TagIPv6` packet records.
- The server may send `TagIPv4` / `TagIPv6` packet records.
- Either side may send `ControlKeepalive` to drive the heartbeat
  (default every 10 s; three missed keepalives = reconnect).
- Either side may send `ControlRekey` to schedule a new inner-KEM
  epoch (the new key activates at `activate_at_seq > 0`, with a
  smooth handover that runs both old and new keys for a transition
  window).

## Phase 6 — Steady-state and shutdown

Steady state is just packet pumping + keepalive + occasional rekey.

A `ControlClose` frame (tag `0x14`) ends the session. Either side may
send it. The receiving side MUST treat the stream as closed and not
send further records. The TLS session is torn down after the gRPC
stream completes.

A peer that vanishes without sending `ControlClose` is treated as a
crash: the kill switch stays up (client side), the IPAM lease is
reclaimed (server side, after a short grace period), and the audit log
records the unexpected close.

## Cross-references

- ARCH §4.1 (`Session` goroutine lifecycle).
- ARCH §7 (two-layer encryption summary).
- ARCH §7.1 (transcript signature MUST, domain separation strings).
- ARCH §7.2 (hybrid cert parser / validator, the only cert validator).
- ARCH §7.3 (DoS defences: per-IP / per-ASN rate limit, half-open cap,
  ClientHello fingerprint, QUIC Retry tokens, CPU budget).
- ARCH §8.1.1 (kill switch persistence, on before phase 2).
- ADR 0001 (packet framing).
- ADR 0002 (one stream per session).
