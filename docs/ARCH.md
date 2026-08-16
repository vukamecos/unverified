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
3. **Two layers of encryption, by design.** Even a complete TLS break does not leak tunnel contents.
4. **eBPF + nftables, not one or the other.** eBPF for hot-path enforcement on the client (kill switch) and on the server (XDP rate-limit); nftables for the persistent forwarding/MASQUERADE rules that survive crashes.
5. **Fail closed.** If any pre-flight check fails (capabilities, BTF, CRL, cert chain), the process refuses to start; if the tunnel dies, the kill switch stays up.
6. **Deterministic builds.** `-trimpath`, embedded assets, no CGO, pinned dependencies, `govulncheck` in CI.

## 3. High-level topology

![VPN tunnel topology](diagrams/topology.svg)

The component-level view is in [`diagrams/topology.puml`](diagrams/topology.puml). Two endpoints, one cloud on the far side of the server:

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
│   ├── transport/
│   │   ├── grpc/               # generated *.pb.go + Tunnel server/client
│   │   ├── quic/               # quic-go wrapper, h3 transport
│   │   ├── tls/                # stdlib tls.Config builder, PQC KEM, cert pinning
│   │   ├── aead/               # inner AEAD (AES-256-GCM / ChaCha20-Poly1305),
│   │   │                       # HKDF, nonce window, Zeroize, rekey
│   │   └── replay/             # sliding-window over 64-bit seq
│   │
│   ├── tunnel/
│   │   ├── packet/             # IP packet framing, TLV codec
│   │   ├── tun/                # /dev/net/tun wrapper (gvisor / songgao / sing-tun)
│   │   ├── route/              # ip route/addr manipulation via netlink
│   │   ├── dns/                # /etc/resolv.conf rewrite + restore
│   │   └── session/            # client↔server session state machine
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
│   │   ├── backend_nft/        # nftables backend (default)
│   │   ├── backend_ebpf/       # eBPF backend (optional, fast path)
│   │   ├── conntrack/          # per-client conntrack table
│   │   └── audit/              # signed append-only audit log
│   │
│   ├── pki/
│   │   ├── ca/                 # internal CA (Ed25519 + ML-DSA-65 hybrid)
│   │   ├── cert/               # issue / renew / revoke / CRL
│   │   ├── acme/               # ACME client (Let's Encrypt, DNS-01 / HTTP-01)
│   │   ├── storage/            # file backend (0600/0644), pluggable interface
│   │   └── hsm/                # PKCS#11 backend (optional)
│   │
│   ├── ipam/                   # lease/release, DNS server assignment
│   ├── httpapi/                # /ip, /healthz, /crl.pem, ALPN splitting
│   │
│   └── capability/             # libcap wrappers, setcap parsing, drop-after-setup
│
├── proto/                      # .proto schema, checked in
├── web/                        # embedded static assets for /ip landing page
├── packaging/
│   ├── deb/                    # .deb spec for the static binary
│   └── systemd/                # unvfd-server.service, unvfd-client.service
│
├── docs/
│   ├── ARCH.md                 # this file
│   ├── TODO.md                 # roadmap
│   └── diagrams/
│       ├── client-sequence.puml
│       └── server-sequence.puml
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
- **TUN:** `gvisor.dev/gvisor/pkg/tcpip/link/tun`, `songgao/water` fork, or `sing-tun`.
- **eBPF:** `github.com/cilium/ebpf` for userspace, BPF C source built with `clang -target bpf` and embedded via `go:embed`.
- **Multiplexer:** `github.com/elastic/gmux`, or split into two listeners.
- **PQC signatures:** ML-DSA-65 (FIPS 204) lives in stdlib as `crypto/mldsa` starting with Go 1.26.0; we use it directly. What stdlib does **not** yet provide is the *integration* of ML-DSA into `crypto/x509` and `crypto/tls` (cert chain validation, signature algorithm negotiation) — that is still tracked as a proposal at golang/go#78888. For the hybrid Ed25519 + ML-DSA-65 X.509 certificate format we therefore ship our own validator (see §7.2) rather than relying on `crypto/x509`; `circl` is **not** required for signatures as such, and we avoid it on the signature path to keep the dependency surface small.
- **Config:** `github.com/pelletier/go-toml/v2`.
- **CLI:** `github.com/urfave/cli/v3`.
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

### 7.1 Inner handshake authentication

The inner KEM alone does **not** authenticate the peer. An active on-path adversary who has broken TLS (e.g. a rogue CA trusted by the client, a stolen server private key at handshake time) can perform MITM on the inner handshake as well, because the inner ephemeral DH exchange carries no signature tied to a long-term identity. Two options exist:

- **Transcript signature (preferred).** After the inner KEM completes, both sides sign a canonical transcript containing the inner KEM public keys + TLS Finished value + ciphersuite + protocol version, using the same ML-DSA-65 / Ed25519 long-term keys that signed the TLS certs. The signature is verified before any inner-AEAD payload is accepted. The inner KEM then provides a binding between the authenticated identity and the inner session keys.
- **Pre-shared key (PSK) bootstrap.** Operator-provisioned PSK (random 256+ bits) mixed into the inner KEM via HKDF, kept in a config file with `0600`. Useful for air-gapped deployments where PKI is overkill; PSK leakage equals inner-layer compromise and must be treated as a top-secret.

Whichever mode is selected, the per-frame AAD binds `protocol_version || cipher_suite || direction || session_id || seq` so that algorithm-agility changes cannot be confused across protocol versions or ciphersuites (see §10.2).

### 7.2 Hybrid signatures in X.509

Hybrid (Ed25519 + ML-DSA-65) signatures in certificates are **not** understood by `crypto/x509` chain validation. The hybrid certificate format, extension layout, and chain validator are custom code, and a self-rolled X.509 validator is one of the most dangerous pieces of crypto to write from scratch. The rules:

- The hybrid cert format is **specified as a stable document** (not just code) — the bit layout, the OIDs, the way the two signatures relate, and the way the issuer/serial are linked. The document is the source of truth; code conforms to it.
- Both signatures **must** verify against the issuer's corresponding public key, and both `subjectPublicKey` fields must be present and well-formed. If either is missing, malformed, or fails verification, the certificate is rejected.
- A **dedicated fuzzer** (see §13) targets the hybrid-cert parser and validator; a separate code review by a reviewer with no implementation investment in the validator is required before the code is merged.
- A **negative test corpus** of malformed hybrid certs (truncated, swapped, single-signature, both-signatures-by-different-issuers, expired-with-valid-signature, etc.) ships with the test suite.

### 7.3 DoS on the PQC handshake

ML-KEM-768 key generation + decapsulation is several times the CPU cost of X25519 alone. Without a DoS shield, the cheapest way to take a server down is to flood it with half-open handshakes. Required defences:

- **Per-IP and per-ASN rate limit** on inbound TLS handshakes (token bucket; e.g. 10 handshakes/s/IP, 200/s/ASN24).
- **Half-open connection cap** (`nftables` connlimit / `ss` conntrack) on the listener port.
- **TLS ClientHello fingerprinting** at XDP/TC before userspace: rate-limit known-abusive fingerprints (e.g. raw-socket scanners, default Go clients with no settings).
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

- The canonical ruleset lives on disk as a versioned, signed blob under `/var/lib/unvfd/killswitch/<hash>.nft`. A small `systemd` pre-start unit (or a `tmpfiles.d` + `ExecStartPre=`) loads this ruleset **before** any user process starts a network. The rules therefore outlive both `unvfd` and the kernel's userland boot sequence.
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

- Config: TOML, env overrides (`UNVFD_*`), flags win last.
- Hot reload: SIGHUP reloads config and the firewall IR; tunnels keep running.
- Secrets on disk: 0600 permissions, refused if world-readable.
- Long-term state (CA, certs, CRL, IPAM leases) lives under `/etc/unvfd/` with mode 0600/0644.
- Runtime state (BPF pinned objects, listener sockets, Unix control socket) under `/run/unvfd/`.

## 11. Operational model

- **systemd unit** for both server and client; `Type=notify`, `NoNewPrivileges=true`, `ProtectSystem=strict`, hardened sandbox (`RestrictNamespaces`, `MemoryDenyWriteExecute`, `SystemCallFilter`).
- **Process drops caps** after setup; the systemd unit grants only `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_BPF` (the mount of `bpffs` happens in a small helper before exec).
- **Two distinct log channels** — see §11.1.
- **Metrics:** Prometheus endpoint at `/metrics`; per-client counters in BPF maps.
- **Releases:** `.tar.gz` + `.deb` per arch, signed (Sigstore / minisign / GPG), with SBOM (SPDX).

### 11.1 Logs: audit vs operational

The threat model in §12 explicitly distinguishes events the operator needs after the fact to prove what happened (security audit) from events useful only for live debugging (operational logs). Treating them as the same channel produces one of two failures: a weak audit trail (logs lost, rotated, attacker-tampered) or a chatty operational log (every packet, every resolve) that itself becomes a leak surface. The two channels are therefore **separate** by design.

**Security audit log** (persistent, append-only, signed)

- Events: connection lifecycle (handshake start, identity, handshake result, session end, source IP/port), certificate lifecycle (issued, renewed, revoked, CRL/OCSP fetched), firewall rule changes, kill-switch triggers (engage, disengage, tamper detection), config reloads, capability drops, audit-log signing-key rotation.
- Storage: append-only file with hash-chained records; each record carries a signature by a dedicated **audit signing key** (Ed25519, separate from the CA key — see §13). On disk in `/var/lib/unvfd/audit/`, mode `0600`, rotated by a dedicated helper that is the only writer.
- Default **on** for both server and client. Cannot be disabled without an explicit config flag plus a console warning; disabling it is recorded in the very log it disables at the moment of disable.
- **No payload, no destination address, no plaintext** ever appears in this log. Field-level redaction enforced at the logger (compile-time check via a typed `audit.Event` struct with no string-or-`[]byte` fields that are not whitelisted).
- Verification tool: `unvfd audit verify <log>` — re-walks the chain, checks signatures, prints the first inconsistency.

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
- A compromised server cannot silently mint a new client identity that would be trusted by **other** servers in a multi-CA deployment: identity is bound to a per-server CA, and clients pin the server's SPKI.
- The audit log is **append-only and signed**; a compromised server cannot rewrite its own audit history (the signatures are with a key not held by the server's runtime process).

The threat-model document does **not** list "compromised server" as a generic adversary, because the design cannot defend against it generically. It lists the three specific sub-cases above as in-scope containment properties.

## 13. Repository conventions

- Default branch: `main`. Feature branches: `feat/...`, `fix/...`. Conventional commits.
- CI on every PR: `go test ./...`, `go vet ./...`, `staticcheck`, `golangci-lint`, `govulncheck`, build matrix `linux/amd64` + `linux/arm64`.
- Release tags: `vX.Y.Z`, signed; SBOM and checksums attached.
- Documentation: English only (`docs/`). PlantUML for sequence diagrams; rendered to PNG/SVG in CI.
- `docs/private/` is personal scratchpad — never read, edited, or committed by automation.
