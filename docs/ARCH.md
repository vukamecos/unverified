# Architecture

> Companion document to [`README.md`](../README.md) and [`TODO.md`](TODO.md).
> TODO lists what to build; this document explains how it fits together and how the code is laid out.
> End-to-end flows live in [`diagrams/client-sequence.puml`](diagrams/client-sequence.puml) and [`diagrams/server-sequence.puml`](diagrams/server-sequence.puml).

## 1. Goals and non-goals

**Goals**

- A VPN that tunnels IP packets over gRPC streams, optionally over QUIC.
- Two independent layers of encryption: TLS (transport) and an inner app-layer AEAD (double end-to-end).
- Hybrid post-quantum key exchange (X25519 + ML-KEM-768), with Ed25519 + ML-DSA-65 for certificate signatures.
- Single static Go binary (`unvfd`) that covers server, client, CA, ACME and operational subcommands.
- Linux only (Ubuntu / Debian), eBPF + nftables for the enforcement plane.
- Reproducible build, dependency pinning, SBOM, signed releases.

**Non-goals**

- Multi-platform clients (Windows / macOS / mobile) — Linux only on both ends.
- Layer-7 deep inspection beyond SNI / HTTP host (no DPI of arbitrary protocols).
- Side-channel resistance beyond constant-time crypto primitives — power/EM attacks and kernel-level rootkits are out of scope.
- Acting as a generic mesh VPN — there is one server, many clients, no client-to-client.

## 2. Architectural principles

1. **Single binary, many roles.** `unvfd {server,client,ca,cert,acme,fw,config,...}` — no separate daemons.
2. **Process drops privileges after setup.** Capabilities are acquired at start, the rules are installed, then the process runs as an unprivileged user with only the minimum capability set.
3. **Two layers of encryption, by design.** Even a complete TLS break does not leak tunnel contents. The inner KEM (see §7) is **authenticated** at the app layer via a transcript signature (§7.1, MUST), so a passive TLS-key compromise is not the only case covered — an active TLS break is also bounded.
4. **eBPF + nftables, not one or the other.** eBPF for hot-path enforcement on the client (kill switch) and on the server (XDP rate-limit); nftables for the persistent forwarding/MASQUERADE rules that survive crashes.
5. **Fail closed.** If any pre-flight check fails (capabilities, BTF, CRL, cert chain), the process refuses to start; if the tunnel dies, the kill switch stays up.
6. **Deterministic builds.** `-trimpath`, embedded assets, no CGO, pinned dependencies, `govulncheck` in CI.

## 3. High-level topology

![VPN tunnel topology](diagrams/topology.svg)

The component-level view is in [`diagrams/topology.puml`](diagrams/topology.puml) (render to PNG/SVG in CI; the `.svg` is generated, the `.puml` is the source of truth and is what you read in a working copy). Two endpoints, one cloud on the far side of the server:

- **Client** owns the TUN, the kill switch (nftables + eBPF), the inner KEM/AEAD, and the TLS transport.
- **Server** owns the listener, XDP rate-limiting, nftables forwarding + MASQUERADE, the inner KEM/AEAD, IPAM, CRL/OCSP, and the HTTP endpoints (`/ip`, `/healthz`, `/crl.pem`).
- **Upstream network** is whatever the server forwards tunneled traffic into.

See [`diagrams/client-sequence.puml`](diagrams/client-sequence.puml) and [`diagrams/server-sequence.puml`](diagrams/server-sequence.puml) for the per-step lifecycle.

## 4. Process model



Two runtime modes, one binary:

### 4.1 `unvfd server`

- One main goroutine + per-connection goroutine tree.
- `Listener` accepts TCP (h2) and UDP (h3/QUIC) on the same port.
- Per accepted connection:
  - TLS handshake (hybrid KEM via `crypto/tls` stdlib).
  - CRL/OCSP check.
  - `gRPC` server dispatches the `Tunnel` bidi stream to a `Session` goroutine.
  - `Session` performs the inner KEM, leases an IP from `IPAM`, and pumps packets.
- A small `Heartbeat` goroutine periodically verifies eBPF maps, nftables ruleset, and process state.
- A `Metrics` goroutine scrapes per-client counters and emits Prometheus.
- Signal handler drains sessions on SIGTERM.

### 4.2 `unvfd client`

- Single long-lived process, three concurrent activities:
  - **Pump:** `TUN` ↔ `gRPC stream`, encrypt/decrypt via inner AEAD.
  - **Transport:** TLS + QUIC state machine with rekey.
  - **Control plane:** reload config, rotate certs, respond to `unvfd status` over a Unix socket.

### 4.3 Lifecycle

Every mode follows: **load config → self-test → acquire caps → setup (nftables, eBPF, TUN, listener) → drop caps → serve → drain on signal → teardown → exit**.

The setup phase is short and privileged; the steady-state phase runs as `_unvfd` user with the minimum capability set.

## 5. Module / package layout

Single Go module `github.com/vukamecos/unverified`, single package root for the binary, internal packages grouped by concern.

```
.
├── cmd/unvfd/                  # main package — wires CLI to subcommands
│   └── main.go
├── internal/
│   ├── config/                 # TOML loader, env overrides, validation
│   ├── logging/                # structured logger (slog → journald / file)
│   ├── version/                # -X injected build metadata
│   │
│   ├── contract/               # interfaces only — the seam between
│   │   ├── firewall/           #   Backend (Compile / Apply / Diff), Program
│   │   ├── pki/                #   Storage, hybrid cert format spec + parser + validator
│   │   │   ├── hybrid/         #     the only place that knows the cert bytes
│   │   │   └── signer/         #     the only place that calls Sign/Verify on long-term keys
│   │   ├── crypto/             #   KeySource, Signer, Verifier, AEAD
│   │   ├── audit/              #   Event schema (typed struct, no []byte / string leakage)
│   │   └── transport/          #   Tunnel (gRPC boundary), Listener, AttestationSink
│   │
│   ├── transport/
│   │   ├── grpc/               # generated *.pb.go + Tunnel server/client wrapper
│   │   ├── quic/               # quic-go wrapper, h3 transport
│   │   ├── tls/                # stdlib tls.Config builder, PQC KEM, cert pinning,
│   │   │                       #   InsecureSkipVerify + VerifyPeerCertificate (§7.2)
│   │   ├── aead/               # inner AEAD (AES-256-GCM / ChaCha20-Poly1305),
│   │   │                       # HKDF, nonce window, Zeroize, rekey
│   │   └── replay/             # sliding-window over 64-bit seq
│   │
│   ├── tunnel/
│   │   ├── packet/             # IP packet framing, TLV codec
│   │   ├── tun/                # /dev/net/tun wrapper (gvisor or sing-tun)
│   │   ├── route/              # ip route/addr manipulation via netlink
│   │   ├── dns/                # /etc/resolv.conf rewrite + restore
│   │   └── session/            # client↔server session state machine,
│   │                           # inner-KEM transcript signature (§7.1, MUST)
│   │
│   ├── netfilter/
│   │   ├── nft/                # nftables ruleset generation, atomic apply/rollback
│   │   └── kill_switch/        # client-side kill switch policy + persistence
│   │
│   ├── ebpf/
│   │   ├── c/                  # BPF C source (clang -target bpf)
│   │   ├── obj/                # prebuilt .o for the pinned kernel (fallback)
│   │   ├── loader/             # cilium/ebpf wrapper, CO-RE relocations
│   │   ├── maps/               # typed wrappers around BPF maps
│   │   └── pin/                # bpffs pinning + cleanup
│   │
│   ├── firewall/
│   │   ├── dsl/                # parser for the in-tunnel firewall DSL
│   │   ├── ir/                 # intermediate representation
│   │   ├── backend_nft/        # nftables backend (default) — implements contract/firewall.Backend
│   │   ├── backend_ebpf/       # eBPF backend (optional, fast path)
│   │   ├── conntrack/          # per-client conntrack table
│   │   └── audit/              # signed append-only audit log writer
│   │                           #   (key held by cmd/unvfd-audit-signer, see §11.1)
│   │
│   ├── pki/
│   │   ├── ca/                 # internal CA (Ed25519 + ML-DSA-65 hybrid)
│   │   ├── cert/               # issue / renew / revoke / CRL
│   │   ├── acme/               # ACME client (Let's Encrypt, DNS-01 / HTTP-01)
│   │   ├── storage/            # file backend (0600/0644) — implements contract/pki.Storage
│   │   └── hsm/                # PKCS#11 backend (optional) — implements contract/pki.Storage
│   │
│   ├── ipam/                   # lease/release, DNS server assignment
│   ├── httpapi/                # /ip, /healthz, /crl.pem, ALPN splitting
│   │
│   ├── audit_signer/           # the cmd/unvfd-audit-signer daemon
│   │                           #   (separate process, holds the audit key — §11.1)
│   │
│   └── capability/             # libcap wrappers, setcap parsing, drop-after-setup
│
├── cmd/
│   ├── unvfd/                  # main binary (server, client, ca, cert, acme, fw, config, audit, lint)
│   └── unvfd-audit-signer/     # separate process holding the audit signing key (§11.1)
│
├── proto/                      # .proto schema, checked in
├── web/                        # embedded static assets for /ip landing page
├── packaging/
│   ├── deb/                    # .deb spec for the static binary
│   └── systemd/                # unvfd-server.service, unvfd-client.service,
│   │                           # unvfd-killswitch.service (§8.1.1),
│   │                           # unvfd-audit-signer.service (§11.1)
│
├── docs/                       # documentation (English)
│   ├── ARCH.md                 # this file
│   ├── TODO.md                 # roadmap
│   └── diagrams/
│       ├── client-sequence.puml
│       ├── server-sequence.puml
│       └── topology.puml
│
├── scripts/                    # build, release, SBOM generation
├── Makefile
├── go.mod
├── go.sum
├── .golangci.yml
└── README.md
```

## 6. Dependency policy

- **Go:** ≥ 1.26 (current stable). `crypto/mlkem`, `crypto/hkdf`, `crypto/sha3`, `crypto/pbkdf2`, hybrid X25519+MLKEM768 in `crypto/tls` — all stdlib.
- **QUIC:** `github.com/quic-go/quic-go` (pure Go, no system dep).
- **TUN:** `gvisor.dev/gvisor/pkg/tcpip/link/tun` (default) or `github.com/xtls/sing-tun`. `songgao/water` is unmaintained since 2020-03 and is **not** used; it is listed here only for historical reference and the design candidates are gvisor and sing-tun.
- **eBPF:** `github.com/cilium/ebpf` for userspace, BPF C source built with `clang -target bpf` and embedded via `go:embed`.
- **Multiplexer:** `github.com/elastic/gmux`, or split into two listeners.
- **PQC signatures:** ML-DSA-65 (FIPS 204) lives in stdlib as `crypto/mldsa` starting with Go 1.26.0; we use it directly. What stdlib does **not** yet provide is the *integration* of ML-DSA into `crypto/x509` and `crypto/tls` (cert chain validation, signature algorithm negotiation) — that is still tracked as a proposal at golang/go#78888. For the hybrid Ed25519 + ML-DSA-65 X.509 certificate format we therefore ship our own validator (see §7.2) rather than relying on `crypto/x509`; `circl` is **not** required for signatures as such, and we avoid it on the signature path to keep the dependency surface small.
- **Config:** `github.com/pelletier/go-toml/v2`.
- **CLI:** `github.com/urfave/cli/v3`.
- **Test tooling:** `github.com/stretchr/testify` (`require` for fatal preconditions, `assert` for independent checks) as the standard assertion library; `github.com/matryer/moq` — mock generation for `internal/contract/*` interfaces (see §13.1: hand-written mocks are forbidden).
- **Build:** `CGO_ENABLED=0`, `-trimpath`, `-ldflags '-s -w -extldflags "-static"'`.

All third-party deps pinned in `go.mod`, verified in CI via `govulncheck` and `gosec`.

## 7. Cryptographic architecture

Two independent layers, each with its own key schedule.

| Layer | Purpose | Algorithm | Key source | Rekey |
|-------|---------|-----------|------------|-------|
| Transport TLS | Authenticate endpoints, protect transport | TLS 1.3 with `X25519MLKEM768` (stdlib); AEAD as defined by RFC 8446 (TLS 1.3 cipher suites — `TLS_AES_256_GCM_SHA384`, `TLS_CHACHA20_POLY1305_SHA256`) | Server / client certs (Ed25519 + ML-DSA-65 hybrid), ephemeral KEM | Per connection (forward secrecy by design) |
| Inner app AEAD | Protect tunnel contents end-to-end | AES-256-GCM if AES-NI, else ChaCha20-Poly1305 | HKDF over ephemeral `X25519 + ML-KEM-768` performed AFTER the TLS handshake | Every 2^28 packets or 1h, whichever comes first |

Properties:

- A **passive** TLS key compromise (e.g. recorded session keys leaked later) does not yield tunnel contents — the inner AEAD, fed by an independent ephemeral inner KEM, still holds. This is the **honest** guarantee: it assumes the TLS endpoints were authentic at the time of the handshake.
- An adversary recording traffic today cannot decrypt it later if either X25519 or ML-KEM-768 holds (post-quantum forward secrecy).
- AEAD nonces are 96-bit, monotonically incremented per direction, with explicit rekey on wraparound.
- Replay protection is a 64-bit sliding window per direction, enforced **after** AEAD open, **before** the packet is handed to the kernel.

### 7.1 Inner handshake authentication (MUST)

The inner KEM alone does **not** authenticate the peer. An active on-path adversary who has broken TLS (e.g. a rogue CA trusted by the client, a stolen server private key at handshake time) can perform MITM on the inner handshake as well, because the inner ephemeral DH exchange carries no signature tied to a long-term identity. This makes the §2.3 promise of "TLS break does not leak tunnel contents" unsound without an app-layer binding — therefore the following is **mandatory** for every connection, not a deployment-time option:

- **Transcript signature.** After the inner KEM completes, both sides sign a canonical transcript containing the inner KEM public keys + TLS Finished value + ciphersuite + protocol version, using the **ML-DSA-65 half** of the long-term hybrid key. (The Ed25519 half is what already signed TLS `CertificateVerify`; using the ML-DSA-65 half here gives the inner layer a proof-of-possession that is independent of the TLS layer's signature.) The signature is verified before any inner-AEAD payload is accepted. The inner KEM then provides a binding between the authenticated identity and the inner session keys.
- **Domain separation.** The signature input is prefixed with a fixed, versioned context string `"unvfd/v1/inner-kem-transcript"` (constant per protocol version; the `/v1/` segment is bumped if the canonicalised transcript format ever changes). This is the same defence TLS 1.3 uses in `CertificateVerify` (RFC 8446 §4.2.3): without it, a signature produced for one protocol layer can be replayed against another. In our case, the rule is that **the ML-DSA-65 key is used in exactly two places** — TLS-level ML-DSA-65 verification (post-quantum hybrid, future work tracked in golang/go#78888) and the inner-KEM transcript signature with this context string. No other use is allowed; the linter in `internal/contract/pki/signer` rejects any other call site at code-review time.
- **PSK bootstrap (optional, additive).** Operator-provisioned PSK (random 256+ bits) mixed into the inner KEM via HKDF, kept in a config file with `0600`. PSK adds an *additional* secret to the inner KDF on top of the transcript-signed ML-DSA-65 binding — it does not replace it. Useful for air-gapped deployments where PKI alone is judged insufficient; PSK leakage means the inner layer is no stronger than the PSK itself, and the PSK must be treated as a top-secret.
- The per-frame AAD binds `protocol_version || cipher_suite || direction || session_id || seq` so that algorithm-agility changes cannot be confused across protocol versions or ciphersuites (see §10.2).

### 7.2 Hybrid signatures in X.509

Hybrid (Ed25519 + ML-DSA-65) signatures in certificates are **not** understood by `crypto/x509` chain validation, **and they cannot drive the stdlib TLS 1.3 handshake** either. This subsection specifies both halves: the cert-format rules and the TLS-integration mechanism.

**Why the stdlib path is not an option.** `crypto/tls` in Go 1.26 negotiates `signature_algorithms` from the cert's public key; for an Ed25519+ML-DSA-65 hybrid, the cert's `SubjectPublicKeyInfo` is itself a custom structure that `crypto/x509` cannot parse. If we hand stdlib a hybrid cert, it either rejects the cert outright or, worse, falls back to whichever half of the key it happens to recognise — silent downgrade. The same problem applies to `CertificateVerify`: stdlib builds it from the cert's key, so a hybrid cert cannot produce a valid CertificateVerify through the normal path. golang/go#78888 tracks adding ML-DSA into `crypto/tls`; we do **not** wait for it.

**The chosen mechanism: `InsecureSkipVerify` + `VerifyPeerCertificate`, hybrid chain built in the app layer.** Concretely:

- The TLS server's `tls.Config.Certificates` carries a hybrid cert in a custom struct; the `KeyLogWriter` and other stdlib hooks are unchanged.
- The TLS client (and the server, on the client-cert path) sets `InsecureSkipVerify: true` and supplies `VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error`. The callback receives the raw cert bytes from the handshake, parses them with our hybrid parser, builds the hybrid chain (both signatures on each cert must verify, §7.2 below), and verifies `SPKI` against the pinned value from config. Only on success does the callback return `nil`; any failure terminates the handshake.
- `VerifyConnection` (or the post-handshake peer-cert hook on the server) repeats the check on the *peer* cert (the client's cert on the server side) using the same code path. The same hybrid parser/validator is the only cert validator in the binary.
- Proof-of-possession on the **TLS** layer is therefore carried by the **Ed25519 half** of the hybrid cert only: the cert's Ed25519 public key is the one that signs `CertificateVerify` (the canonical TLS 1.3 transcript hash). The ML-DSA-65 half is a binding witness — it is **not** used inside the TLS handshake. It is checked at the app layer (see below).
- ML-DSA-65 is used as a **proof-of-possession on the inner handshake** instead. The transcript signature from §7.1 is computed with the ML-DSA-65 half of the long-term key. This binds the inner KEM session keys to an identity that has the ML-DSA-65 private key, not just the Ed25519 private key.

**What the app layer verifies, end to end.** A successful connection therefore proves:

1. The peer holds the Ed25519 private key (TLS CertificateVerify, standard TLS 1.3).
2. The peer holds the ML-DSA-65 private key (inner-KEM transcript signature, §7.1).
3. Both public keys are bound to the same identity (hybrid cert is well-formed: both `subjectPublicKey` fields, the cert's signature carries both Ed25519 and ML-DSA-65, and the issuer chain verifies both).
4. The cert chain is otherwise valid (expiry, EKU, name constraints, pathlen, CRL/OCSP — all checked by our validator, not stdlib).

**The cert format rules.** Beyond the integration mechanism, the format itself has rules:

- The hybrid cert format is **specified as a stable document** (not just code) — the bit layout, the OIDs, the way the two signatures relate, and the way the issuer/serial are linked. The document is the source of truth; code conforms to it.
- Both signatures **must** verify against the issuer's corresponding public key, and both `subjectPublicKey` fields must be present and well-formed. If either is missing, malformed, or fails verification, the certificate is rejected.
- A **dedicated fuzzer** (see §13 testing note + TODO §"Fuzzing harness coverage (extended)") targets the hybrid-cert parser and validator; a separate code review by a reviewer with no implementation investment in the validator is required before the code is merged.
- A **negative test corpus** of malformed hybrid certs (truncated, swapped, single-signature, both-signatures-by-different-issuers, expired-with-valid-signature, Ed25519-valid-but-ML-DSA-invalid, ML-DSA-valid-but-Ed25519-invalid, etc.) ships with the test suite.
- **The cert-format doc, the parser, and the validator are all in the same package** (`internal/contract/pki/hybrid`); nothing outside that package is allowed to touch the cert bytes.

### 7.3 DoS on the PQC handshake

ML-KEM-768 key generation + decapsulation is several times the CPU cost of X25519 alone. Without a DoS shield, the cheapest way to take a server down is to flood it with half-open handshakes. Required defences:

- **Per-IP and per-ASN rate limit** on inbound TLS handshakes (token bucket; e.g. 10 handshakes/s/IP, 200/s/ASN24).
- **Half-open connection cap** (`nftables` `connlimit` match in the listener chain) on the listener port.
- **TLS ClientHello fingerprinting** at XDP/TC before userspace: the allow-list covers our own client build's fingerprint (so we do not rate-limit ourselves) and known-trusted client builds; raw-socket scanners, default-config `openssl s_client` / `curl` / `nmap`, and other generic fingerprints are throttled. The fingerprint of *our own* client is shipped in the binary and pinned, so an upgrade that changes the fingerprint is a deliberate, audited change — the allow-list is updated as part of the same release.
- **QUIC Retry tokens** (RFC 9000 §8) for the h3 path — every new client must prove reachability of its source address with a Retry token before the server invests a ML-KEM-768 decapsulation.
- **CPU budget per second** for handshake work; new handshakes beyond the budget are dropped with a `BUSY` error and a Retry-After hint.
- **Proof-of-work** option (e.g. hashcash-style) behind a feature flag for exposed public listeners, off by default.

## 8. Enforcement plane

### 8.1 Client side

- **`tun0`** carries the tunneled traffic; default route points at it.
- **`nftables` kill switch** blocks all egress from the physical interface unless destined for the gRPC server IP/port or loopback. Installed **before** the TUN comes up.
- **eBPF TC egress drop** mirrors the kill switch in-kernel for low overhead; the XDP program is a second line of defence on the physical interface.
- **eBPF map allow-list** is pushed at connect time and updated on reconnect.
- **Heartbeat** verifies that `nftables` rules and BPF programs are still attached; if anything is missing, the tunnel is dropped.

#### 8.1.1 Kill switch persistence (fail-closed)

The kill switch is the last line of defence: it must hold even when the process is dead, has crashed, or the host has just booted. A "rollback on startup" model is **not** acceptable because the gap between OS-boot and `unvfd` reaching its setup phase is a leak window. The persistence model is therefore:

- The canonical ruleset lives on disk as a versioned, signed blob under `/var/lib/unvfd/killswitch/<hash>.nft`. A small dedicated `systemd` unit (`unvfd-killswitch.service`, `Type=oneshot`, `RemainAfterExit=yes`) loads this ruleset. Because `ExecStartPre=` of the main service cannot run before networking is up — if the main service is the only thing supposed to bring the rules up, the rules are absent at boot — the killswitch unit orders itself with `Before=network-pre.target` and is pulled in by the main service via `Wants=` / `After=`. The result: at the moment the kernel brings up the default route, the deny-by-default egress ruleset is already in place. The unit runs with the minimum `CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW` and the standard hardening set (§11), exits 0 only after `nft -f` succeeds and the ruleset is verified by a follow-up `nft list ruleset | sha256sum` check against the blob's hash.
- On startup, `unvfd` does **not** "roll back" the rules — it **reconciles** them. It reads the active ruleset, computes the diff against the canonical blob, and applies the minimal delta. The reconciled state is the canonical state; there is no implicit "before-unvfd" state to fall back to.
- On clean shutdown, `unvfd` does **not** remove the rules. The kill switch stays up. A separate operator command (`unvfd fw disable --confirm`) is the only way to remove it, and that action is recorded in the audit log.
- The ruleset blob is signed by the audit signing key (see §11.1); the loader refuses to apply an unsigned or modified blob. An attacker with root who edits the file cannot disable the kill switch without invalidating the signature.
- `LAN / RFC 1918 / link-local` traffic is **denied by default**; only entries explicitly added to the ruleset are allowed. This includes mDNS (`224.0.0.251`, `ff02::fb`), LLMNR, and SSDP — these are common covert-exfiltration channels and must be opted into, not opted out of.

### 8.2 Server side

- **`ip_forward=1`** on the server host.
- **nftables forward chain** with `MASQUERADE` on egress; per-client ACLs compiled from the DSL.
- **eBPF XDP** on the public interface drops blacklisted source IPs and rate-limits the rest.
- **eBPF TC ingress** on the LAN side captures return traffic destined for tunnel clients.
- **conntrack** tracks per-client connections so the in-tunnel firewall can allow `established`/`related` traffic.

## 9. In-tunnel firewall (DSL)

A small declarative DSL compiled to an IR; the IR is emitted to one of three backends:

- **nftables** (default) — proven in-kernel engine.
- **eBPF** — TC/XDP programs compiled from the same IR for hot paths.
- **In-process Go engine** — for testing, audit, and environments where neither kernel feature is available.

Hot-reload computes a diff and applies the minimal delta; `default deny` policy if no rule matches; every deny/reject is logged to the signed audit log.

CLI: `unvfd fw {check,apply,diff,show,test}`.

## 10. Configuration and state

- Config: TOML, env overrides (`UNVFD_*`), flags win last. **Configuration files live under `/etc/unvfd/`** (operator-managed, version-controlled, mode 0644 for the file, 0600 for any `secret.*` referenced from it). This includes `client.toml` / `server.toml`, the CA and cert material, and the operator-provided PSK (§7.1) if used.
- **Mutable state lives under `/var/lib/unvfd/`.** This includes the IPAM lease table, the hash-chained audit log and the audit-signer socket peer info, the kill-switch ruleset blob, and any other state the daemon produces. Files there are mode `0600` (or `0644` for non-secret blobs) and owned by `_unvfd`. **Never** under `/etc`: `/etc` is for the operator, `/var/lib` is for the daemon.
- Runtime / volatile state (BPF pinned objects, listener sockets, the audit-signer Unix socket `/run/unvfd/audit-signer.sock`, the metrics Unix socket, the control socket) under `/run/unvfd/`, mode `0755` for the directory, `0660` for the sockets with group `_unvfd`.
- Hot reload: SIGHUP reloads config and the firewall IR; tunnels keep running.
- Secrets on disk: 0600 permissions, refused if world-readable.

## 11. Operational model

- **systemd unit** for both server and client; `Type=notify`, `NoNewPrivileges=true`, `ProtectSystem=strict`, hardened sandbox (`RestrictNamespaces`, `MemoryDenyWriteExecute`, `SystemCallFilter`).
- **Process drops caps** after setup; the systemd unit grants only `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_BPF` (the mount of `bpffs` happens in a small helper before exec).
- **Logs:** structured logging via stdlib `log/slog` (typed key-value records, no printf-style strings); two distinct channels — see §11.1.
- **Metrics:** Prometheus endpoint at `/metrics`. **Bound to `127.0.0.1` by default** (or `::1` if IPv6 loopback is enabled); when scrape-from-LAN is required, the runtime listens on a **separate Unix socket or a dedicated loopback port with `mTLS`** (config flag `metrics.listen = "unix:///run/unvfd/metrics.sock" | "tcp://127.0.0.1:9100" | "tcp://0.0.0.0:9100"`). The `tcp://0.0.0.0` form requires an explicit opt-in plus a separate client-cert; the default is loopback only. Per-client counters live in BPF maps, read via the dedicated `cilium/ebpf` iterator, not via shared maps.
- **Releases:** `.tar.gz` + `.deb` per arch, signed (Sigstore / minisign / GPG), with SBOM (SPDX).

### 11.1 Logs: audit vs operational

The threat model in §12 explicitly distinguishes events the operator needs after the fact to prove what happened (security audit) from events useful only for live debugging (operational logs). Treating them as the same channel produces one of two failures: a weak audit trail (logs lost, rotated, attacker-tampered) or a chatty operational log (every packet, every resolve) that itself becomes a leak surface. The two channels are therefore **separate** by design.

**Security audit log** (persistent, append-only, signed)

- Events: connection lifecycle (handshake start, identity, handshake result, session end, source IP/port), certificate lifecycle (issued, renewed, revoked, CRL/OCSP fetched), firewall rule changes, kill-switch triggers (engage, disengage, tamper detection), config reloads, capability drops, audit-log signing-key rotation.
- Storage: append-only file with hash-chained records; each record carries a signature by a dedicated **audit signing key** (Ed25519, separate from the CA key — see §13). On disk in `/var/lib/unvfd/audit/`, mode `0600`, rotated by a dedicated helper that is the only writer.
- **The signing key is not held by the long-running runtime process.** This is the central containment property: a runtime compromise cannot rewrite audit history because it cannot produce valid signatures. The mechanism is a small **separate process** — `unvfd-audit-signer` — that:
  - Owns the audit signing key (file on disk, mode `0600`, owned by user `_unvfd-signer`; or HSM/PKCS#11 if the deployment has one).
  - Listens on a Unix socket at `/run/unvfd/audit-signer.sock`, mode `0660`, group `_unvfd`. Peer-credentials (`SO_PEERCRED`) are checked on every accept: only the runtime's UID is allowed to connect.
  - Has no network sockets, no TUN, no BPF, no nftables handle. Its `CapabilityBoundingSet=` is empty. `NoNewPrivileges=yes`, `ProtectSystem=strict`, `PrivateDevices=true`, `RestrictNamespaces=true`, `RestrictAddressFamilies=AF_UNIX` only.
  - Speaks a one-request/one-response protocol: the runtime sends a canonicalised record (length-prefixed, no ambiguity), the signer replies with the Ed25519 signature over `chain-hash || record`. Runtime appends `signature` to the record and writes the record to disk.
  - If the signer is unreachable, the runtime **buffers up to N records in memory** (N is configurable; default 10 000) and applies back-pressure: a non-safety-critical operation that would produce an audit event is *blocked* (not silently dropped) until the signer is reachable again. Safety-critical events (kill-switch disengage, cert revocation by the operator, audit-disable) are mirrored to a small journald facility as a fail-safe.
  - Is supervised by its own systemd unit with `Restart=on-failure` and a watchdog; if the signer crashes, the runtime detects the broken pipe, reopens the socket, and replays any buffered records. The signer re-reads the log on start to learn the current chain head before accepting new records.
- Default **on** for both server and client. Cannot be disabled without an explicit config flag plus a console warning; disabling it is recorded in the very log it disables at the moment of disable (the disable event is sent through the signer *before* the signer's connection is closed, so the disable itself is signed).
- **No payload, no destination address, no plaintext** ever appears in this log. Field-level redaction enforced at the logger (compile-time check via a typed `audit.Event` struct with no string-or-`[]byte` fields that are not whitelisted).
- Verification tool: `unvfd audit verify <log>` — re-walks the chain, checks signatures (using the public half of the audit key, distributed out-of-band), prints the first inconsistency.

**Operational log** (in-memory, ring buffer)

- Events: start/stop, handshake diagnostic detail, rekey, conntrack churn, BPF map errors, capability warnings, log of the audit channel's own health.
- Storage: fixed-size ring buffer in process memory (e.g. 16 MiB). Drained by `unvfd logs` and by journald on shutdown. **Not** persisted to disk by default.
- Configurable level (debug / info / warn / error) via flag and SIGHUP.
- Same redaction rules as audit: no payload, no destination address, no plaintext.

**Common rule:** no log line on either channel may contain a payload byte, a destination IP/port of a tunneled packet, a DNS query name, or a session key. A linter (`unvfd lint logs`, run in CI) scans the code for logger calls and rejects any that pass forbidden field types.

## 12. Security threat model (summary)

**In scope:** passive network observer, malicious Wi-Fi / on-path adversary, compromised peer cert, replay, harvest-now-decrypt-later.

**Explicitly out of scope:** kernel rootkit on the host, side channels (power / EM), physical access to a running host, compromised build host, compromised server (see §12.1).

**Mitigations in scope:** mTLS + SPKI pinning, hybrid PQC KEM, inner AEAD with transcript-bound session keys, replay window, kill switch, capability drop, syscall filter, SBOM + signed releases, hash-chained signed audit log.

### 12.1 Compromised server — what we do and do not promise

The VPN **server** terminates both encryption layers and sees plaintext. A compromised server is therefore plaintext-equivalent by construction — no client-side trick can recover that. What the design **can** promise in that case is the following containment:

- The server never holds long-term **client private keys** or **CA private keys** in a form that can be exfiltrated from a single process compromise (e.g. CA key lives on a separate intermediate, HSM, or air-gapped host; see §13.3).
- A compromised server cannot silently mint a new client identity that would be trusted by a **freshly-reinstalled** server: client identities are bound to the issuing server's CA (each server has its own intermediate, see §13.3), and clients pin the server's SPKI — the SPKI pin is a separate config value, not derived from the cert. A new server install with the same hostname but a new key does not get accepted. (There is no multi-server mesh in scope; see §1 non-goals.)
- The audit log is **append-only and signed**; a compromised server cannot rewrite its own audit history. The signing key is held by the separate `unvfd-audit-signer` process (§11.1), which is reachable only via a peer-credentials-checked Unix socket; the runtime never holds the key in its address space.

The threat-model document does **not** list "compromised server" as a generic adversary, because the design cannot defend against it generically. It lists the three specific sub-cases above as in-scope containment properties.

## 13. Repository conventions

- Default branch: `main`. Feature branches: `feat/...`, `fix/...`. Conventional commits.
- CI on every PR: see §13.1 for the full static-analysis + security toolchain. Tests + build matrix `linux/amd64` + `linux/arm64`. Fuzz targets live in `*_test.go` files with `func FuzzXxx(f *testing.F)`.
- Release tags: `vX.Y.Z`, signed; SBOM and checksums attached.
- Documentation: English only (`docs/`). PlantUML for sequence diagrams; rendered to PNG/SVG in CI.
- `docs/private/` is personal scratchpad — never read, edited, or committed by automation.

### 13.1 Mandatory testing policy

Tests are **not optional** in this repository. A change without tests is an unfinished change.

- **Unit tests are mandatory** for every `internal/` package: every new exported behaviour ships with a `*_test.go` in the same PR. Table-driven tests are the default style. Assertions use `testify` (`github.com/stretchr/testify`): `require` for preconditions that must abort the test on failure, `assert` for independent checks — no hand-rolled `if got != want { t.Fatal(...) }` boilerplate.
- **Coverage gate:** CI fails the PR if the package being changed drops below the agreed coverage floor, and if `go build ./...`, `go vet ./...`, or `go test ./...` are not green.
- **Fuzz tests are mandatory** for every self-rolled parser, decoder, or verifier (see §7.2): the protocol frame codec, the firewall DSL parser, the TOML config loader, the inner-KEM handshake, and the hybrid-certificate validator. Native Go fuzzing only (`testing.F`, `go test -fuzz`), with a checked-in seed corpus including malformed/negative inputs.
- **Crypto paths:** any code touching keys, nonces, or signatures additionally gets round-trip tests (encrypt→decrypt, sign→verify, KEM encaps→decaps) and negative tests (wrong key, corrupted tag, replayed seq must fail loudly).
- **Regression tests:** every bug fix ships with a test that fails without the fix.
- **No flaky tests in CI:** a test that fails intermittently is treated as a bug and is fixed or removed, never re-run until green.
- **Mocks are generated, never hand-written.** Test doubles for `internal/contract/*` interfaces are produced by `moq` (`github.com/matryer/moq`) from the interface definition via `//go:generate` directives; the generated `*_mock.go` files are committed next to the interface's package. Hand-written or inline ad-hoc mocks are **forbidden**: a manual stub silently drifts from the interface and compiles green while testing the wrong contract. If an interface changes, the mock is regenerated (`go generate ./...`) in the same PR — never patched by hand. A CI lint step rejects any test file declaring a mock/fake/stub type without the generated-code marker.

### 13.2 Static analysis & security toolchain

The CI pipeline runs the same toolchain on every push to a PR and on every merge to `main`. A red build blocks merge. The pipeline is layered: a *fast* set runs on every PR (a few minutes, fail-fast on syntax/type/lint regressions); a *slow* set runs nightly on `main` and weekly across the dependency tree (fuzzers, full vuln DB, deep scans).

**Fast layer (per PR, required)**

- **`go build ./...`** and **`go vet ./...`** — compile and the stdlib's whole-program checker.
- **`golangci-lint run --timeout 10m`** with the linter set listed below. `golangci-lint` is the orchestrator; it runs a curated set of linters in parallel and is the only authoritative lint command in the repo. The repo pins a specific `golangci-lint` version in `Makefile` so a new linter release cannot silently change the build status.
- **`go test ./... -race -count=1 -shuffle=on`** with the `-race` detector and `-shuffle=on` to surface order-dependent test bugs.
- **`govulncheck ./...`** — the official Go vulnerability database scanner; only flags calls that are *actually reachable* from the binary's main packages, not "your deps mention a vulnerable function somewhere".
- **`gitleaks detect --source . --no-git`** (or **`trufflehog filesystem .`** as an alternative) — secret scanning on the working tree. Runs on every PR; historical secret scan runs weekly on all branches.
- **Build matrix `linux/amd64` + `linux/arm64`** with `-buildmode=pie`, `-trimpath`, `-ldflags '-s -w -extldflags "-static"'`, `CGO_ENABLED=0`. CI explicitly greps the resulting ELF header for `ET_DYN` and the absence of a fixed load address; a non-PIE build is a build failure (§3 hardening).

**Slow layer (nightly on `main`)**

- **`go test -fuzz=.`** on the targets in TODO §"Fuzzing harness coverage (extended)" — proto decoder, DSL parser, config parser, inner-KEM handshake, hybrid-cert parser/validator. Each target runs for 10 minutes; the corpus is committed under `fuzz/corpus/<target>/` and grows on every nightly run.
- **`gosec ./...`** with the rule set listed below. `gosec` overlaps with `golangci-lint`'s security linters, but it also has dedicated rules for things like weak random number generation and insecure HTTP listening — kept as a separate gate for that reason.
- **`nancy`** (or **`osv-scanner`**) — second-opinion dependency CVE scanner, cross-checked against `govulncheck`. Two independent databases catching two independent error classes.
- **`semgrep --config p/security-audit --config p/owasp-top-ten --config p/golang`** — semgrep's curated Go + security rule packs, run on the source tree. Catches patterns the Go-native linters miss (e.g. custom data-flow rules).
- **`trufflehog filesystem --only-verified`** — deep historical secret scan, including base64-encoded and split-across-commits secrets. Weekly.
- **`shellcheck` on `scripts/**.sh`** and **`hadolint` on any `Dockerfile`** — if/when container images are added.
- **`gofips140 validate`** (when running in FIPS mode, TODO §"Standards & audit") — verifies the stdlib providers are the FIPS-validated ones.

**`golangci-lint` linter set**

The `.golangci.yml` is checked in and explicitly enumerates `enable:`. The set is deliberately conservative (no "experimental" linters, no cosmetic ones) and biased toward security and correctness:

- `govet` — stdlib whole-program checker; reports shadowed variables, unreachable code, printf format mismatches, locking mistakes, and the `vet` security checks (`-vet=...` covers `atomic`, `bool`, `buildtag`, `directive`, `errorsas`, `ifaceassert`, `nilfunc`, `printf`, `stringintconv`).
- `gosec` (G306, G302, G304, G104, G404, G201/G202, G501, G401, G402/G404, etc.) — the Go security checker. Critical rules for us: `G104` (unhandled errors), `G304` (file path from variable, must not be tainted), `G306` (write to file with `0666`/`0600` should be explicit), `G404` (`math/rand` for security-sensitive code), `G501`/`G401` (MD5/SHA1 imports), `G402`/`G403` (insecure TLS config — we expect to see these, so the linter is configured to *allow* our specific exception in `internal/transport/tls` with a `//nolint:gosec` and a code-review-required reason).
- `staticcheck` — the most-cited non-stdlib linter; catches wrong `time.Duration` unit conversions, unused writers, struct field alignment, and a long tail of "this code does not do what you think it does" bugs.
- `govulncheck` is run as a separate step (not under `golangci-lint`), because it requires network access to the Go vuln DB.
- `ineffassign` — `x = x` where the assignment is ineffective; cheap, catches real bugs.
- `unused` (the new `unused` linter, formerly part of `staticcheck`) — unused functions, types, fields. Configured to allow `_test.go` files to use `//nolint:unused` for shared fixtures.
- `unconvert` — pointless conversions (`int(x)` where `x` is already `int`).
- `gocritic` — with the `diagnostic`, `style`, `performance` checks enabled; the `experimental` checks are off.
- `predeclared` — uses of Go predeclared identifiers as names (`len`, `cap`, `new` as variable names).
- `bodyclose` — `http.Response.Body` must be closed; matters for `/crl.pem`, `/ip`, `/healthz` handlers.
- `errorlint` — `errors.Is` / `errors.As` instead of string-equality; matters for our cert chain error types.
- `nilerr` — `return nil` after handling an error; the classic "I forgot to return the error" bug.
- `goconst`, `gocyclo`, `dupl` — kept on default settings; cyclomatic complexity is a soft gate, the per-function limit is `30` (justified by the protocol state machines, which are inherently branchy).
- `exportloopref`, `looppointer`, `copyloopvar` — catches the classic Go gotchas around loop variable capture.
- `misspell`, `gofmt`, `goimports` — cosmetic; the bar is "no diff".

**Linters we deliberately do *not* enable** (and why): `nlreturn` (style), `wsl` (style), `gofumpt` (we use `gofmt` + `goimports` and review the diff), `funlen` (the protocol parsers are intentionally long), `ifshort` (style), `mnd` (we use named constants for the magic numbers that matter; raw integers in test code are fine), `paralleltest` (too noisy on integration tests).

**Code-review enforced checks that linters cannot do alone**

- **Secret management.** No literal private keys, no `api_token=...` in code, no AWS / GCP credentials. The pre-commit hook and the `gitleaks` step both fail on a hit; reviewers are the second line.
- **Constant-time handling.** `gosec` flags `subtle.WithDataIndependentTiming`-style issues, but the *real* check is human: a PR that touches `internal/transport/aead`, `internal/contract/pki/hybrid`, or `internal/contract/pki/signer` requires a second reviewer with a comment confirming the constant-time argument.
- **Crypto-touching code.** A PR that touches `internal/contract/crypto`, `internal/transport/tls`, or `internal/contract/pki/{hybrid,signer}` requires a maintainer's explicit `/ok-crypto` comment before merge. This is the same gate the Go standard library uses for its own crypto code.
- **Audit-log field discipline.** `unvfd lint logs` (§11.1) is a custom in-tree linter, not a public `golangci-lint` plugin, and is the authoritative check for "no payload / no destination / no DNS" in any log call.
- **License and import-path discipline.** `go-licenses check` ensures every dependency has a recognised license (SPDX); `goimports` + a CI check rejects imports from outside the module's allow-list (no transitive surprises from a careless `go get -u`).
- **Reproducibility.** `diffoscope` on the `linux/amd64` build artifact between two CI runs (or between a release and its source tag) must be empty. The build invokes `go build` with the flags in §3 and embeds the version + commit hash via `-ldflags "-X ..."`.

**Why two overlapping tools (gosec and golangci-lint's gosec).** `gosec` is the source of truth for the rule set; `golangci-lint` is the source of truth for the *fast* multi-linter pipeline. Running `gosec` standalone in the slow layer catches rules the in-pipeline `gosec` linter may have skipped because of a config interaction, and gives us a separate diff in the nightly report. The rule files `.gosec.json` and the `golangci.yml` `gosec:` block are kept in lock-step.

## 14. Design principles (KISS, DRY, SOLID)

This section makes the design rules explicit so a new contributor can see, on reading the package layout, *why* it is shaped the way it is. Three families of principle: **KISS** (don't add complexity you don't need yet), **DRY** (one place per fact, one place per behaviour), and the five **SOLID** principles (with the caveat that Go is not classic OO — "class" maps to "package", "subclass" maps to "interface implementation").

**KISS — keep it small, keep it simple**

- Prefer stdlib over a third-party package when stdlib does the job (we use `crypto/mldsa`, `crypto/mlkem`, `crypto/tls`, `crypto/x509` and write our own hybrid-cert validator only because stdlib does not understand the hybrid format yet — golang/go#78888).
- No DI framework, no reflection, no init() magic. Wiring is done in `cmd/unvfd/main.go` and the per-subcommand constructors; the rest of the code receives ready-to-use values.
- No configuration language more powerful than TOML. The DSL in §9 is a deliberately small grammar, not a general-purpose expression language.
- "Two backends, one feature" is the rule, not "N backends, all features": the firewall has *three* backends because they target genuinely different environments (kernel nftables, kernel eBPF, in-process tests); we do not add a fourth backend speculatively.

**DRY — don't repeat yourself**

- One canonical state for the kill-switch ruleset (§8.1.1). Both the nftables table and the eBPF map are derived from the same signed blob, not maintained independently.
- One place where session keys are zeroed: a single `Zeroize()` helper in `internal/transport/aead` that every caller routes through. No hand-rolled `for i := range k { k[i] = 0 }` scattered through the codebase.
- One place where the audit-log record schema is defined: a typed struct in `internal/firewall/audit` consumed by every writer; the linter that enforces "no payload / no destination / no DNS" (§11.1) reads the same struct.
- Generated code from `.proto` is the *only* place where wire-format details live; the rest of the codebase operates on the abstract `Tunnel` interface.
- TOML config schema is declared once (struct + tags) and reused by the loader, the linter, and `unvfd config show`.

**SRP — single responsibility.** Each `internal/` package has one reason to change: `transport/aead` knows about AEAD primitives and nothing else; `transport/replay` knows about the sliding window and nothing else; `tunnel/session` orchestrates the per-connection state machine; `firewall/dsl` parses, `firewall/ir` holds the IR, `firewall/backend_nft` and `firewall/backend_ebpf` compile the IR to a target, `firewall/conntrack` and `firewall/audit` are siblings with their own jobs. A bug in the conntrack table does not require touching the DSL parser, and vice versa.

**OCP — open for extension, closed for modification.** New firewall backends, new key sources, new storage backends, new signers, and new audit sinks are added by *implementing an interface* in a new file, not by editing the consumers. Concretely:

- `internal/contract/firewall` defines `Backend { Compile(IR) (Program, error); Apply(Program) error; Diff(a, b) (Program, error) }`; `backend_nft`, `backend_ebpf`, and the in-process engine all conform. Adding a fourth backend is one new file.
- `internal/contract/pki` defines `Storage { Read(name) ([]byte, error); Write(name, []byte, error); List() ([]string, error) }`; `pki/storage/file` and (optionally) `pki/storage/pkcs11` both conform.
- `internal/contract/crypto` defines small interfaces — `KeySource`, `Signer`, `Verifier`, `AEAD` — each one method or two, so a new primitive (e.g. a future SLH-DSA fallback) drops in without touching callers.

**LSP — Liskov substitution.** All `Backend` implementations are interchangeable for the firewall IR compiler; the compiled `Program` type returned by `Compile` is the same shape regardless of backend, so a higher-level test harness can swap an in-process backend for a real kernel backend without changing the test. Same for `Storage` and `Signer`. The substitution rule is enforced at the type system: the interface return types are concrete enough that no backend can return a "more specific" `Program` that the consumer cannot use.

**ISP — interface segregation.** We deliberately do not define a fat `Crypto` interface that bundles AEAD + sign + verify + KEM; instead, callers depend on the smallest interface they actually use (`transport/tls` depends on a `Signer` for the cert chain, not on the whole crypto surface). The transport layer is split the same way: `transport/tls` knows about the TLS handshake, `transport/aead` knows about inner-layer encryption, `transport/replay` knows about the sliding window — three packages, three interfaces, three reasons to change.

**DIP — dependency inversion.** High-level orchestration code (`tunnel/session`, `firewall/Engine`, `cmd/unvfd/...`) depends only on the interfaces in `internal/contract/*`, never on a concrete nftables, eBPF, HSM, or filesystem type. Concrete implementations live in sub-packages and are wired in `main.go` based on config. This is what lets us run the same engine against an in-process backend in tests and against the real kernel in production, with no `if testing` branches in the engine itself.

**gRPC / proto boundary.** The generated `*.pb.go` code is the only place that knows the wire format. Everything else in the binary imports `internal/transport/grpc` (a thin wrapper exposing the abstract `Tunnel` server/client interface), not `google.golang.org/grpc` types directly. If we change `.proto`, only the wrapper and the generated code change; the rest of the codebase keeps the same `Tunnel` interface and is unaware.

**Where the line is.** SOLID is a tool, not a religion. A package that genuinely has one job does not need three interfaces; a function that is called from one place does not need to be parameterised. The acid test for adding an interface is "is there already a second implementation, or a second one planned within the next milestone?" If not, we pass the concrete value and move on.
