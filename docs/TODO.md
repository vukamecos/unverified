# TODO

Roadmap for the VPN tunnel over gRPC.

Target platform: **Ubuntu / Debian (Linux)**.

## Dependencies (Debian packages)

- [ ] `golang-go` (or install Go from the upstream tarball — **≥ 1.26**, latest stable as of 2026-08; ships `crypto/mlkem`, `crypto/hkdf`, `crypto/sha3`, `crypto/pbkdf2` and enables the `X25519MLKEM768` hybrid in `crypto/tls` by default)
- [ ] `protobuf-compiler` (`protoc`)
- [ ] Go gRPC plugins: either install via `go install google.golang.org/protobuf/cmd/protoc-gen-go google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`, or install the Debian package `protoc-gen-go-grpc` (bookworm+).
- [ ] `iproute2` (for the `ip` command — TUN/TAP and routing)
- [ ] `nftables` (for NAT / forwarding rules on the server — `nft` command)
- [ ] `build-essential` (for CGO if the TUN library needs it)
- [ ] System capability: `/dev/net/tun` available, kernel module `tun` loaded
- [ ] eBPF support: Linux kernel **≥ 5.4 minimum, ≥ 5.10 recommended for stable XDP**. Below 5.4 you can still load basic BPF programs but CO-RE, BTF relocations, and many of the helpers we rely on are missing.
- [ ] **BTF (BPF Type Format):** debug-info metadata exported by the kernel at `/sys/kernel/btf/vmlinux`. BTF was added in **kernel 5.2** and `CONFIG_DEBUG_INFO_BTF` is on by default in **Ubuntu 20.10+ / Debian 11+**; Ubuntu 20.04 only got it on later point releases (not the originals), so verify per host. Required for **CO-RE (Compile Once – Run Everywhere)**: BPF programs compiled with BTF relocations are auto-patched at load time against the running kernel's struct layouts, so the same `.o` works across kernel versions without recompilation. BTF is also consumed by the in-kernel verifier to type-check accesses to kernel structs. Check availability with `bpftool btf dump file /sys/kernel/btf/vmlinux | head`. Kernels without vmlinux BTF (older releases, some cloud images) need a fallback (e.g. BTFHub prebuilt BTF tarballs). Without BTF, BPF objects must be compiled per host (no portability) or hardcode struct offsets (brittle).
- [ ] `libbpf-dev`, `clang`, `llvm` (for compiling eBPF programs)
- [ ] CAP_BPF, CAP_PERFMON, CAP_NET_ADMIN (required to load eBPF programs)
- [ ] QUIC: `github.com/quic-go/quic-go` — no system package needed (pure Go), but kernel UDP buffer tuning (`sysctl net.core.rmem_max`). (There is no `libpquic` Go binding in current use; do not pull in anything that name-resolves to a non-existent project.)
- [ ] PQC library: Go stdlib — `crypto/mlkem` for ML-KEM-768/1024, `crypto/tls` hybrid X25519+MLKEM768 enabled by default on Go 1.24+, **and `crypto/mldsa` (FIPS 204) for ML-DSA-65, available since Go 1.26.0**. The signature path therefore does **not** depend on `github.com/cloudflare/circl`; circl is no longer required for ML-DSA and we keep it out of the dependency surface. (`crypto/x509` and `crypto/tls` integration of ML-DSA — needed for the stdlib TLS path to negotiate a hybrid signature — is still tracked as a proposal at golang/go#78888, and the binary's own hybrid-cert validator in `internal/contract/pki/hybrid` is what bridges that gap; see ARCH §7.2.)

## Protocol

- [x] Define the `.proto` schema for tunnel messages (packet framing, control channel)
- [ ] Decide on packet encoding (raw IPv4/IPv6 vs. TLV)
- [ ] Decide on stream multiplexing (one stream per session vs. per connection)
- [ ] Document handshake / authentication flow

## Client (Ubuntu/Debian)

> End-to-end flow: [`docs/diagrams/client-sequence.puml`](diagrams/client-sequence.puml).

- [ ] Create TUN interface via `/dev/net/tun`. Default choice: `gvisor.dev/gvisor/pkg/tcpip/link/tun` (already used by gVisor, broadly tested) or `github.com/xtls/sing-tun` (actively maintained, used by Xray/sing-box). `songgao/water` is **unmaintained** since 2020-03 and is not used here; the listing above is kept only as a historical reference and the only candidates in the design are gvisor and sing-tun.
- [ ] Bring interface up with `ip link set ... up` and assign address
- [ ] Read IP packets from TUN, hand off to gRPC stream
- [ ] Configure routing (`ip route add ... dev tun0`)
- [ ] DNS handling (`/etc/resolv.conf` rewrite or systemd-resolved stub)
- [ ] Run with CAP_NET_ADMIN (setcap on binary or run as root)

### Kill switch (client-side)

- [ ] Install `nftables` rules that drop all outbound traffic not going through the TUN interface
- [ ] Allow-list: loopback, TUN interface, gRPC server IP/port (so the client can reach the server)
- [ ] Activate kill switch BEFORE establishing the tunnel
- [ ] **Persistence model is "reconcile, not rollback":** the canonical ruleset is a signed blob under `/var/lib/unvfd/killswitch/` loaded by a `systemd` pre-start unit (or `tmpfiles.d` + `ExecStartPre=`) **before** userland fully starts, so the rules survive `unvfd` crashes and host reboots. `unvfd` on startup computes a diff between the live ruleset and the canonical blob and applies the minimal delta — it does not "remove on shutdown and re-add on start" because that opens a leak window. The only way to remove the rules is `unvfd fw disable --confirm`, recorded in the audit log.
- [ ] **Default-deny LAN / RFC 1918 / link-local:** mDNS (`224.0.0.251`, `ff02::fb`), LLMNR, SSDP, `169.254.0.0/16`, `fe80::/10`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` — all blocked unless explicitly added to the allow-list. This is opt-in, not opt-out.
- [ ] Ruleset blob is signed by the audit signing key; the pre-start loader refuses to apply an unsigned or modified file.
- [ ] Configurable: enable / disable via CLI flag or config (disabling the kill switch is the `unvfd fw disable --confirm` operator action above)

### eBPF packet handling (client & server)

- [ ] Use the `cilium/ebpf` Go library for loading programs and maps
- [ ] Build pipeline: `clang -target bpf` → embedded `.o` via `go:embed`
- [ ] Decide on hook points: XDP (ingress), TC egress/ingress, cgroup/sock_ops, cgroup/sock_msg
- [ ] **Client kill switch (eBPF path):**
  - [ ] TC egress program: drop all packets not destined for the TUN interface
  - [ ] Allow-list via eBPF map: server IP, server port, loopback
  - [ ] Fast-path: XDP drop on physical interface as a second line of defence
- [ ] **Client fast path (optional):**
  - [ ] XDP/TC redirect: captured packets are redirected directly to the TUN interface (bypassing kernel network stack overhead)
  - [ ] Userspace side (gRPC stream) reads from TUN as before
- [ ] **Server fast path (optional):**
  - [ ] XDP on the public interface to capture incoming gRPC-encapsulated traffic directly
  - [ ] TC ingress on the LAN side to capture return traffic destined for tunnel clients
- [ ] **Statistics & control:**
  - [ ] Per-client eBPF maps: bytes in/out, packet counters, last-seen timestamp
  - [ ] Control map to push runtime config (allowed subnets, kill-switch on/off)
- [ ] **Lifecycle:**
  - [ ] Pin programs to `bpffs` for persistence
  - [ ] Detach / unload programs cleanly on shutdown
  - [ ] Fallback to pure `nftables` kill switch if eBPF is unavailable (old kernel, restricted container)
- [ ] **Security:**
  - [ ] Verify BPF Type Format (BTF) is available for CO-RE / portability
  - [ ] Set RLIMIT_MEMLOCK for older kernels

## Server (Ubuntu/Debian)

> End-to-end flow: [`docs/diagrams/server-sequence.puml`](diagrams/server-sequence.puml).

- [ ] Accept gRPC streams from clients
- [ ] Decapsulate packets, forward to upstream network
- [ ] Handle return traffic (inbound packets → stream → client)
- [ ] Per-client IP assignment / subnet management
- [ ] Enable IP forwarding: `sysctl -w net.ipv4.ip_forward=1`
- [ ] Configure `nftables` rules: MASQUERADE for outbound traffic, forwarding chains
- [ ] Run as a systemd service (unit file)

### Firewall (in-tunnel packet filtering)

#### Where it runs

- [ ] **Client-side (egress filter):** which traffic the client sends into the tunnel at all
  - [ ] Per-process filtering (cgroup/sock_ops hook → we know which process sent it)
  - [ ] Per-destination / per-port / per-protocol
  - [ ] Application-aware: domain allowlist via SNI/DoH inspection
- [ ] **Server-side (ingress filter):** what the server forwards into the network
  - [ ] Per-client ACL (by client cert / assigned IP)
  - [ ] Layer-3 (subnets, IPs), Layer-4 (ports, protocols), Layer-7 (SNI, HTTP host)
  - [ ] Stateful connection tracking (conntrack) — allow return traffic

#### Architecture (front-end + back-end)

- [ ] **Front-end:** our own DSL for describing rules (declarative)
  - [ ] Format: TOML/YAML config + optional REPL/CLI for interactive editing
  - [ ] Rules compile to an IR (intermediate representation)
  - [ ] Diff-based updates: when the config changes, compute a minimal delta and reload only the changed rules
- [ ] **Back-end (choice still open — native engine vs. ready-made):**
  - [ ] **Option A — our own engine:**
    - [ ] Userspace packet classifier in Go using the same IR
    - [ ] Light hot path, full control over semantics
    - [ ] Telemetry and audit built in
  - [ ] **Option B — nftables as the back-end:**
    - [ ] Generate the `nft` ruleset from the IR
    - [ ] Pro: proven in-kernel engine; con: limited DSL
  - [ ] **Option C — eBPF as the back-end:**
    - [ ] TC/XDP programs with the same rules, compiled to BPF C
    - [ ] Maximum performance
  - [ ] **Back-end decision** — open question (TBD), see discussion in README/docs
- [ ] **Hot-reload** of rules without breaking connections (where possible)
- [ ] **Default deny** policy: if no rule matches — packet is dropped and logged

#### DSL capabilities (draft)

```
# Example: allow the client only DNS and HTTPS to the office subnet
allow client "alice" tcp from any to 10.0.0.0/8 port 443
allow client "alice" udp from any to 1.1.1.1 port 53
allow client "alice" tcp from any to any port 443   # HTTPS in general
deny client "alice" all                             # default deny
log all
```

- [ ] Match on: client identity, source/dest IP/CIDR, port, protocol, interface
- [ ] Actions: `allow`, `deny`, `drop`, `reject`, `log`, `count`, `redirect`
- [ ] State: `established`, `related`, `new`, `invalid`
- [ ] Comments in rules (for audit)
- [ ] Dry-run mode: show what a rule would do, without applying it

#### Connection tracking

- [ ] Conntrack table (per-client) with TTL
- [ ] Statistics export: open connections, top talkers
- [ ] Cleanup by signal or by timeout

#### Telemetry and audit

- [ ] Counter per rule: how many times it fired, how many bytes passed/dropped
- [ ] Structured log on every deny/reject: rule, src, dst, proto, action
- [ ] Export to Prometheus / journald
- [ ] Audit log: who, when changed the rules (signed)

#### Self-test

- [ ] Run nftables/conntrack/rules via a smoke test before activating
- [ ] Fake packet through the classifier → expected verdict

#### CLI subcommands

- [ ] `unvfd fw check <config>` — validate DSL, show the resulting IR
- [ ] `unvfd fw apply <config>` — apply (with hot-reload if possible)
- [ ] `unvfd fw diff <old> <new>` — show diff between configs
- [ ] `unvfd fw show` — currently active rules, counters
- [ ] `unvfd fw test <rule>` — dry-run a rule against a test packet set

### Server endpoints (HTTP + gRPC on the same port)

- [ ] Same TCP listener serves both plain HTTP and gRPC via connection multiplexer (`elastic/gmux`, or split into two listeners — see cri-o#9969 for the rationale):
  - [ ] Sniff HTTP/2 preface (`PRI * HTTP/2.0`) and `Content-Type: application/grpc*` → gRPC handler
  - [ ] Everything else → HTTP handler
- [ ] **Check-IP endpoint** (`GET /ip`):
  - [ ] Plain HTTP/1.1 + HTTP/2 compatible
  - [ ] Returns the client's public IP as seen by the server (after any MASQUERADE / after tunnel decapsulation)
  - [ ] Response formats: plain text (`Your IP: 1.2.3.4`), JSON (`{"ip":"1.2.3.4"}`), opt-in HTML page
  - [ ] Honor `X-Forwarded-For` ONLY if behind a trusted reverse proxy (config flag)
  - [ ] No tracking, no cookies, no JS, no third-party assets (privacy-first)
- [ ] Health endpoint (`GET /healthz`): liveness/readiness for load balancers
- [ ] Optional landing page (`GET /`) with project info + link to docs
- [ ] TLS: same cert serves both HTTP and gRPC (SNI optional; ALPN `h2` covers both)
- [ ] Rate-limit `/ip` to prevent abuse (token bucket per source IP)

## Transport

- [ ] gRPC bidirectional streaming setup
- [ ] Flow control and backpressure handling
- [ ] Reconnect logic with exponential backoff
- [ ] TLS / mTLS for encryption in transit

### Protocol switching (gRPC ↔ QUIC)

- [ ] **Mode A — gRPC only** (current): server speaks gRPC over HTTP/2 over TCP
- [ ] **Mode B — gRPC over QUIC** (custom transport on top of `quic-go` + manual HTTP/2 framing): same protocol, UDP transport; better on lossy / mobile networks.
- [ ] **Mode C — QUIC native** (custom RPC over QUIC, no gRPC framing): lowest overhead, more code
- [ ] **Mode D — dual-stack on one endpoint**: server accepts both `grpc+h2` (TCP) and `grpc+quic` (UDP) on the same host:port pair
  - [ ] Client config: `transport = "grpc" | "quic" | "auto"`
  - [ ] `auto`: client tries gRPC first, falls back to QUIC (or both in parallel, pick the first to authenticate)
  - [ ] Server: bind both TCP and UDP on the same port; per-connection routing by ALPN (`h2` → grpc, `h3` → grpc-over-quic) and content-type sniffing
  - [ ] `elastic/gmux` (the actively-maintained fork) for splitting TCP into HTTP (check-IP) vs gRPC on the same port — see cri-o#9969 for the rationale. (`soheil/cmux` is no longer maintained and is **not** used.)
- [ ] ALPN negotiation: advertise both `h2` and `h3`, pick by client preference
- [ ] 0-RTT handshake: **off by default**. Enabling it requires an explicit `--allow-0rtt` flag AND a per-enable audit-log entry recording the operator's intent. When 0-RTT is on, the docs must say — and the code must enforce — that any data sent in 0-RTT (early data) is *not* protected by the inner KEM's forward secrecy: a later compromise of the resumption PSK or pre-shared ML-KEM key decrypts that early data. Use cases (e.g. "just give me the IP of the server") must be safe to replay; 0-RTT must never carry inner-AEAD-protected tunnel packets.

## Configuration

- [ ] Config file format (YAML / TOML)
- [ ] CLI flags for client and server
- [ ] Server address, credentials, subnet settings
- [ ] Environment variable overrides

## CLI (entry points / subcommands)

- [ ] CLI framework: `urfave/cli` (v3)
- [ ] **Single binary `unvfd`** — all roles and commands in one executable file
- [ ] Subcommands are dispatched via `urfave/cli` by the first positional argument
- [ ] The same file is used both on the server and on the client — role chosen via subcommand (`server` / `client`)
- [ ] All CA/cert/ACME/self-test commands live inside the same binary, nothing to install separately
- [ ] `unvfd` with the following commands:

### Core commands

- [ ] `unvfd server` — run the VPN server (HTTP + gRPC + check-IP, on the configured port)
- [ ] `unvfd client` — run the VPN client (connect to server, set up TUN, kill switch, eBPF)
- [ ] `unvfd version` — print version, commit, build time
- [ ] `unvfd completion` — shell completion (bash/zsh/fish)

### PKI / certificate management

- [ ] `unvfd ca init` — bootstrap the internal CA
  - [ ] Generate root key (Ed25519 by default, RSA-4096 as option)
  - [ ] Self-signed root certificate (10y default lifetime)
  - [ ] Store in `/etc/unvfd/ca/` (0600 for key, 0644 for cert)
  - [ ] Print fingerprint for audit pinning
- [ ] `unvfd ca info` — show CA cert subject, fingerprint, expiry, serial
- [ ] `unvfd cert issue server <name>` — issue a server certificate signed by the internal CA
  - [ ] SANs: configured FQDN(s) + IP(s)
  - [ ] EKU: `serverAuth`
  - [ ] Lifetime: 90d default, configurable
- [ ] `unvfd cert issue client <name>` — issue a client certificate signed by the internal CA
  - [ ] SANs: client identifier (UUID or CN)
  - [ ] EKU: `clientAuth`
  - [ ] Lifetime: 24h default (short-lived), configurable up to 90d
  - [ ] Optional hardware binding: include TPM EK / YubiKey pub in SAN:otherName
- [ ] `unvfd cert revoke <serial>` — mark a certificate as revoked (maintain local CRL)
  - [ ] Reason code (unspecified, keyCompromise, etc.)
  - [ ] Regenerate CRL
- [ ] `unvfd cert renew <name>` — issue a new cert replacing an existing one
- [ ] `unvfd cert list` — list all known certs (issued by this CA) with status, expiry
- [ ] `unvfd cert show <name|serial>` — full cert details, fingerprints (SHA-256, SHA-1)
- [ ] `unvfd cert verify <cert-file>` — check signature against local CA, expiry, EKU
- [ ] `unvfd cert pin <name>` — print SubjectPublicKeyInfo hash for client-side pinning

### ACME (public certificate for server)

- [ ] `unvfd acme obtain` — obtain a publicly trusted certificate (Let's Encrypt / other ACME CA)
  - [ ] HTTP-01 or DNS-01 challenge (DNS-01 preferred for wildcard)
  - [ ] Use as fallback / supplement to internal CA cert on the public listener
- [ ] `unvfd acme renew` — auto-renew before expiry
- [ ] `unvfd acme import` — import an externally obtained cert + key
- [ ] Storage in `/etc/unvfd/acme/` with proper file modes

### Operational commands

- [ ] `unvfd config check` — validate config file, test reachability, dry-run rules
- [ ] `unvfd config show` — print effective config (with secrets redacted)
- [ ] `unvfd status` — show running sessions, TUN state, kill-switch state
- [ ] `unvfd logs` — tail structured logs (journald or file)
- [ ] `unvfd self-test` — verify nftables/eBPF/TUN/cert chain before going live
- [ ] `unvfd fw disable --confirm` — remove the client-side kill switch. **The only sanctioned way** to take the kill switch down. Records the operator's identity and timestamp in the audit log *before* removing the rules; refuses to run without `--confirm`. (See ARCH §8.1.1.)
- [ ] `unvfd audit verify <log>` — re-walk an audit log, re-verify every signature using the public audit key passed in (`--audit-pubkey <path>`), print the first inconsistency if any, exit non-zero on failure. (See ARCH §11.1.)
- [ ] `unvfd lint logs` — static check that scans the source tree for logger calls passing forbidden field types (payload bytes, destination IP/port, DNS query names, raw session keys). Runs in CI; rejects a PR that adds a leaky call site. (See ARCH §11.1.)

### Audit signer (separate process)

- [ ] `unvfd-audit-signer` — small standalone binary in `cmd/unvfd-audit-signer/`. Holds the audit signing key (file on disk, mode `0600`, user `_unvfd-signer`; or PKCS#11 if HSM available). Listens on `/run/unvfd/audit-signer.sock`, peer-credentials-checked. No network, no TUN, no BPF, no nftables; `CapabilityBoundingSet=` is empty. Supervised by its own `unvfd-audit-signer.service` with `Restart=on-failure`. (See ARCH §11.1.)

### PKI storage & backend

- [ ] CA key encrypted at rest (passphrase from env, kernel keyring, or TPM)
- [ ] Optional HSM backend (PKCS#11) for the CA key in production
- [ ] File backend by default, pluggable interface for future backends (SQL, Vault)
- [ ] CRL distribution: server exposes `GET /crl.pem`; clients check it
- [ ] **CRL validation on the client is mandatory and explicit:** the client verifies (a) the CRL signature with the issuing CA's public key (hybrid Ed25519 + ML-DSA-65 — both must verify), (b) `thisUpdate` is in the past and within an operator-configured `max_clock_skew` (default 24h) of local time, (c) `nextUpdate` is present and in the future, (d) the serial being checked falls within the CRL's scope. A CRL that fails any of these checks is treated as *no CRL* (i.e. fail-closed: the cert is treated as not revoked-checked, and the connection is refused). This blocks the "freeze-the-CRL" attack where an adversary pins an old CRL on a client so a just-revoked cert remains accepted.
- [ ] OCSP responder (optional): `unvfd ocsp start` for fast revocation checks. OCSP responses are also signature-verified and freshness-checked (`producedAt` within skew). Cached OCSP responses expire at `nextUpdate` and never outlive it.

## Cryptography — post-quantum (PQC)

### Goals

- [ ] End-to-end encryption between client and VPN server over QUIC/TLS
- [ ] The packet stays encrypted **all the way to egress on the server side** (true E2E, double encryption: transport + application layer)
- [ ] Defence against **harvest now, decrypt later** — intercepting traffic today must not enable decryption by a quantum adversary in the future

### Key exchange (hybrid KEM)

- [ ] Hybrid scheme: **X25519 + ML-KEM-768** (Kyber768)
  - [ ] Combine the two shared secrets via HKDF (`crypto/hkdf` + SHA-512; package shipped in Go 1.24)
  - [ ] If one of the KEMs is compromised, the other still holds
- [ ] Library: `github.com/cloudflare/circl` (audited, pure Go)
- [ ] KEM runs at the transport layer (gRPC/QUIC) at every handshake
- [ ] Additional **inner KEM at the app layer** — an independent session on top of transport:
  - [ ] Client and server generate ephemeral X25519 + ML-KEM keys
  - [ ] Inner KEM is used for **app-layer AEAD**, separate from TLS
  - [ ] Even if TLS session keys are compromised, traffic remains protected by the inner layer

### Symmetric encryption (app-layer AEAD)

- [ ] AEAD chosen by hardware capabilities:
  - [ ] **AES-256-GCM** if AES-NI is available
  - [ ] **ChaCha20-Poly1305** otherwise (ARM, mobile)
- [ ] Per-session key derived from the hybrid KEM (HKDF-derived)
- [ ] 96-bit nonce, incremental counter with explicit wraparound handling → rekey
- [ ] AAD (Additional Authenticated Data) includes: **protocol version, negotiated cipher suite, session ID, direction, packet sequence**. Binding version and cipher suite into the AAD prevents cross-version and cross-cipher confusion during algorithm-agility transitions; without it, an attacker who could downgrade negotiation could feed a frame from one epoch into another.

### Replay protection

- [ ] Sliding window over a 64-bit packet sequence number
- [ ] Drop packets with seq ≤ window_low or seq > window_high
- [ ] Separate windows per direction
- [ ] Log replay attempts (security telemetry)

### Key rotation (PFS)

- [ ] Rekey every 2^28 packets or 1 hour, whichever comes first
- [ ] Rekey = new ephemeral KEM without revealing long-term keys
- [ ] Smooth rekey: new key activated simultaneously on both sides via an explicit rekey message
- [ ] On rekey — new nonce counter (reset)

### Signatures (cert chain)

- [ ] **ML-DSA-65** (Dilithium3) for certificate digital signatures
- [ ] Hybrid signature (Ed25519 + ML-DSA-65) in certificates for compatibility
- [ ] **CA certificate constraints** (issued by `unvfd ca init`, enforced on every cert the CA mints):
  - `pathlen:0` on the issuing CA cert so a compromised intermediate cannot mint a sub-CA.
  - `keyUsage critical` with only `keyCertSign` and `cRLSign` set.
  - `nameConstraints` on the issuing CA, scoped to the operator's chosen DNS / IP / SRV / directoryName namespaces — limits blast radius if the CA private key is stolen.
  - `cRLDistributionPoints` and `authorityInformationAccess` (OCSP URL) populated on every issued cert.
- [ ] **Offline / air-gapped root option:** the *root* CA can live on a machine that is never online (hardware token, dedicated laptop in a safe). A separate *intermediate* CA signs the 24h client / 90d server certs and is online. The root is brought out only to sign the intermediate or to issue a new CRL. This is the standard "online intermediate / offline root" tiering pattern.
- [ ] **Dedicated audit signing key** (Ed25519, optional HSM/PKCS#11 backend) — *not* the CA key. Used only to sign audit log records (see ARCH §11.1) and the kill-switch ruleset blob (see ARCH §8.1.1). Compromise of the CA does not enable an attacker to forge audit history, and compromise of the audit key does not enable cert issuance.
- [ ] Cert chain verification: validate both algorithms

### Algorithm agility

- [ ] Protocol version in every message (for forward compatibility)
- [ ] Configurable algorithm set via CLI/config:
  - [ ] `--kem=x25519-mlkem768` (default)
  - [ ] `--sig=ed25519-mldsa65` (default)
  - [ ] `--aead=aes256gcm | chacha20poly1305`
- [ ] Fallback to classics (`x25519-only`, `ed25519-only`) if PQC is unavailable — **only with a warning, disabled by default**
- [ ] Self-test at startup: round-trip KEM + AEAD before accepting connections

### PQC performance

- [ ] Benchmark ML-KEM-768 vs. X25519 (latency, throughput)
- [ ] Pre-generated ephemeral keys pool to reduce handshake latency
- [ ] 0-RTT with PQC: pre-shared ML-KEM keys (for reconnect), with explicit "0-RTT used" flag in logs
- [ ] CPU profiling (ML-KEM is noticeably heavier than X25519)

### PQC CLI subcommands

- [ ] `unvfd pqc bench` — benchmark PQC vs. classics on the current CPU
- [ ] `unvfd pqc self-test` — round-trip check of the hybrid KEM
- [ ] `unvfd pqc show` — list supported algorithms and their versions
- [ ] `unvfd ca init --pqc` — initialize CA with PQC keys
- [ ] `unvfd cert issue ... --pqc` — issue a cert with hybrid signature

### Standards & audit

- [ ] Compliance with NIST FIPS 203 (ML-KEM), FIPS 204 (ML-DSA), FIPS 205 (SLH-DSA)
- [ ] **FIPS 140-3 mode (Go 1.26+):** the `crypto/fips140` package plus the `GOFIPS140=1` env var or build tag put the stdlib crypto providers (`crypto/mldsa`, `crypto/mlkem`, `crypto/tls` hybrid groups, `crypto/ecdsa`, `crypto/rsa`, `crypto/aes`, …) into the FIPS-140-3 validated code path. `unvfd` MUST be buildable in this mode for deployments that require FIPS validation (`.deb-fips` flavor or `--tags fips140` build target). The mode is opt-in because the FIPS-approved algorithms are a strict subset of what the binary otherwise supports; classic-only and hybrid-without-ML-DSA builds remain available for non-FIPS deployments. Verify at startup that the negotiated primitives are FIPS-approved when the mode is on.
- [ ] Use only audited circl versions, pin them in `go.sum` (only needed if circl is pulled in at all — for the signature path itself we use stdlib `crypto/mldsa`, see ARCH §6)
- [ ] Document the threat model for PQC (what we protect against, what we don't — e.g. side-channel attacks on the implementation)
- [ ] Penetration testing focus: handshake downgrade, classical MITM with swap to classical-only

## Operations

- [ ] Logging (structured, levels)
- [ ] Metrics (bytes in/out, active sessions, errors)
- [ ] Graceful shutdown (SIGINT/SIGTERM)
- [ ] systemd integration (service unit, journal logging)

## Packaging

### Single-binary distribution

- [ ] **One static binary `unvfd`** does everything (server, client, all CLI subcommands)
- [ ] **CGO disabled** for build: `CGO_ENABLED=0` — eliminates glibc/musl dependency
- [ ] Fully static linking: `go build -ldflags '-s -w -extldflags "-static"'`
- [ ] **PIE binary** (`-buildmode=pie`) for ASLR on the long-running root process; CI explicitly checks the ELF type and fails the build if the binary is not `ET_DYN`. `go build -buildmode=pie` is the default for `cmd`-style main packages on Go 1.18+ amd64/arm64, but the Makefile pins it and CI verifies it — toolchain or vendor changes occasionally flatten it.
- [ ] `-trimpath` flag: removes local filesystem paths from the binary (reproducibility)
- [ ] **Embedded assets (no external files):**
  - [ ] eBPF object files (`.o`) embedded via `go:embed`
  - [ ] Generated protobuf code (`*.pb.go`) compiled in
  - [ ] Default config templates (TOML) embedded
  - [ ] Static web assets for the `/ip` endpoint embedded
- [ ] **Build metadata injected via `-ldflags "-X ..."`:**
  - [ ] version (semver)
  - [ ] git commit hash
  - [ ] build timestamp
  - [ ] build host (for forensics)
- [ ] **Version output:** `unvfd version` prints all metadata
- [ ] **No runtime dependencies** beyond the Linux kernel itself
- [ ] **Distribution channels:**
  - [ ] Direct download: GitHub Releases tarball with checksum (`sha256sums.txt`)
  - [ ] `.deb` package wrapping the static binary
  - [ ] Optional: install script that fetches the right arch from GitHub Releases

### Build & release pipeline

- [ ] `Makefile` for build / test / lint / release
- [ ] Cross-compile matrix: linux/amd64, linux/arm64
- [ ] CI pipeline (lint, test, build) on every PR
- [ ] Reproducible builds (deterministic output, byte-for-byte identical for the same source)
- [ ] Release artifacts: `.tar.gz` + `.deb` per arch
- [ ] systemd unit files for client and server
- [ ] Checksums and signatures (Sigstore / GPG) on every release

## Testing

- [ ] Unit tests for proto encoding/decoding
- [ ] Integration test: client ↔ server loopback on Ubuntu
- [ ] End-to-end test: real internet connectivity through the tunnel
- [ ] CI on Ubuntu runner (GitHub Actions)

## Documentation

- [ ] Installation guide (apt repo or `.deb`)
- [ ] Usage examples (server + client invocation)
- [ ] systemd setup
- [ ] Troubleshooting guide (TUN permissions, ip_forward, nftables)
- [ ] Performance notes / tuning

## Paranoid security (zero-trust, leak-proof)

### Transport hardening

- [ ] Mandatory mTLS for all gRPC connections (no plaintext fallback)
- [ ] Pin server certificate / public key on the client (cert pinning) to prevent MITM with a rogue CA
- [ ] **Hybrid PQC KEM** at transport: X25519 + ML-KEM-768 (Kyber), combined via HKDF
- [ ] **Inner app-layer KEM** (X25519 + ML-KEM-768) — independent session on top of TLS, double end-to-end encryption
- [ ] AEAD encryption of tunnel payloads (ChaCha20-Poly1305 or AES-256-GCM) with replay protection (nonce window / sliding window)
- [ ] PFS (Perfect Forward Secrecy): rekey via new ephemeral PQC KEM periodically
- [ ] Pad packets to fixed bucket sizes to mitigate traffic-analysis / fingerprinting
- [ ] **Measurable side-channel discipline on the data path:** AEAD-verify (or AEAD-open) is the *first* action on a frame; no other handling happens until verify succeeds. All decrypt failures produce a single uniform response (a single log line at a single level, a single metric counter, a single upstream action). No code path distinguishes "bad tag", "wrong seq", "unknown session", "stale version", or any other failure mode by timing, log wording, or external behaviour. The threat-model document calls out that this is an *anti-oracle* property, not a claim of constant-time down to the cycle — a claim of constant-time is not enforceable in Go and the docs must not pretend it is.
- [ ] PQC signatures in cert chain (ML-DSA-65 / Dilithium, hybrid with Ed25519)

### Authentication & authorization

- [ ] Mutual authentication: client cert + server cert
- [ ] Short-lived client certificates (e.g. 24h), renewable via ACME-like flow or out-of-band
- [ ] Per-client identity bound to a hardware key (TPM 2.0 / YubiKey) where available
- [ ] Server-side ACL: per-client allowed subnets, rate limits, max concurrent sessions
- [ ] Strong KDF for any stored credentials (Argon2id, not bcrypt/PBKDF2 with weak params)
- [ ] No password auth over the network — only cert-based or token-based (and tokens never reused)

### Leak prevention

- [ ] IPv6 leak: block IPv6 outside the tunnel (kill switch applies to IPv6 too)
- [ ] DNS leak: force all DNS through the tunnel (override `/etc/resolv.conf`, disable systemd-resolved upstream fallback)
- [ ] WebRTC / STUN leak prevention notes in docs (out of our scope but must be documented)
- [ ] Block traffic during handshake (no traffic flows until the tunnel is fully established)
- [ ] Block traffic during reconnect (queue or drop, configurable)
- [ ] DHCP leak: prevent raw DHCP packets escaping the tunnel
- [ ] ICMP leak: decide policy — block all ICMP outside the tunnel, or allow only specific types
- [ ] mDNS / link-local (169.254.0.0/16, fe80::/10) handling policy: **deny by default**, opt-in allow. The default kill switch blocks mDNS multicast (`224.0.0.251`, `ff02::fb`), LLMNR (`224.0.0.252`, `ff02::1:3`), and SSDP (`239.255.255.250`) as well, because they are common covert-exfiltration channels (Airdrop, Bonjour, printers). Operators explicitly add an allow-list entry if they need them.

### Process & runtime hardening

- [ ] Drop all capabilities except the minimum required. **`CAP_SYS_ADMIN` is *not* in the steady-state set** — the daemon runs with only:
  - `CAP_NET_ADMIN` + `CAP_NET_RAW` for TUN (`ioctl(TUNSETIFF)` + read/write). `CAP_SYS_ADMIN` is *not* required for TUN; the persistent rumour that it is comes from the historical `ioctl(TUNSETIFF)` flag combinations that asked for elevated privileges, all of which are no-ops on modern kernels.
  - `CAP_BPF` + `CAP_PERFMON` + `CAP_NET_ADMIN` for eBPF networking programs on Linux ≥ 5.8 (CAP_BPF + CAP_PERFMON were split out of CAP_SYS_ADMIN in 5.8). On kernels < 5.8 the eBPF path needs `CAP_SYS_ADMIN` instead of `CAP_BPF`+`CAP_PERFMON`; the runtime probes and selects the right set.
  - `CAP_SYS_ADMIN` is required *only* for mounting `bpffs` itself; that step happens in a one-shot privileged helper before exec (see ARCH §11), and the long-running daemon never holds it.
- [ ] Run as a dedicated unprivileged user (`nobody` or `_vpn`) after the setup phase
- [ ] `NoNewPrivileges=true` in the systemd unit
- [ ] `ProtectSystem=strict`, `PrivateTmp=true`, `ProtectHome=true`, `ProtectKernelTunables=true`, `ProtectKernelModules=true`
- [ ] `ProtectClock=true` (no `clock_settime` / `adjtimex` from the runtime)
- [ ] `ProtectKernelLogs=true` (no read access to `/dev/kmsg` — defeats dmesg-peeking after a leak)
- [ ] `ProtectControlGroups=true`, `ProtectHostname=true`, `LockPersonality=true`
- [ ] `PrivateDevices=true` with an explicit `DeviceAllow=` whitelist: `/dev/net/tun rw` and the BPF map / kfunc surfaces needed by the loader; nothing else. (`PrivateDevices=true` alone hides all devices, but we still need `/dev/net/tun` and any pinned bpffs nodes.)
- [ ] `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK` — drop AF_PACKET, AF_KEY, AF_NETROM, etc. the daemon does not need
- [ ] `SystemCallArchitectures=native` — block ia32 / x32 syscalls on amd64
- [ ] `UMask=0077` — files created by the daemon do not leak via group/world read
- [ ] `RestrictNamespaces=true`, `RestrictRealtime=true`, `RestrictSUIDSGID=true`
- [ ] `SystemCallFilter=@system-service` + explicit deny list (e.g. `@clock`, `@reboot`, `@module`)
- [ ] `MemoryDenyWriteExecute=true` where feasible (mitigates some ROP, breaks some eBPF loaders — evaluate)
- [ ] Lock config file (mode 0600, owned by root), refuse to start if world-readable
- [ ] Refuse to run if `kernel.dmesg_restrict=1` is not set (anti info-leak)
- [ ] Refuse to run if `kernel.kptr_restrict=2` is not set (hide kernel pointers)
- [ ] Disable coredumps (`RLIMIT_CORE=0`) and strip debug symbols from release builds
- [ ] The systemd pre-start unit that loads the kill switch (§8.1.1) runs with its own minimal hardening: `Type=oneshot`, `CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW`, all the protections above, and exits 0 only after `nft -f` succeeds and the ruleset is verified by a follow-up `nft list ruleset | sha256sum` check against the blob's hash.

### Network hardening (server side)

- [ ] Bind gRPC server to a dedicated interface or socket (no `0.0.0.0` if unnecessary)
- [ ] Optional obfuscation: pad gRPC frames, randomize inter-packet timing
- [ ] Drop traffic from unknown / blacklisted IPs at XDP before it hits userspace
- [ ] Rate-limit per source IP / per client cert at XDP / TC
- [ ] Disable ICMP echo response on the public interface (or rate-limit heavily)
- [ ] Strict reverse-path filtering on the server (`rp_filter=1`)

### Build & supply chain

- [ ] Reproducible builds (deterministic via `-trimpath`, buildinfo normalization)
- [ ] Signed releases (Sigstore / minisign / GPG) with public key pinned in the repo
- [ ] SBOM (SPDX / CycloneDX) attached to every release
- [ ] Dependency pinning (`go.mod` with explicit versions, `go.sum` verified in CI)
- [ ] `govulncheck` in CI on every PR
- [ ] Static analysis: `gosec`, `staticcheck`, `golangci-lint` with security linters enabled
- [ ] Run CI on isolated runners (no shared cache), review third-party actions
- [ ] Fuzzing harness for protocol decoder (native Go fuzzing via `testing.F` + `go test -fuzz=...`, available since Go 1.18)
- [ ] Constant-time cryptography primitives only (`crypto/subtle` or audited libs)

### Audit & monitoring

- [ ] Audit log (signed / append-only) for: connection events, config changes, kill-switch triggers, cert rotations
- [ ] Optional remote attestation: client reports its eBPF/nftables state hash to server on connect; server refuses if mismatched. **Honest framing:** the heartbeat / attestation mechanism detects *configuration drift* on the client — e.g. an admin or a careless upgrade changed the ruleset, an eBPF map is in an unexpected state, the kill-switch ruleset hash differs from the canonical blob. It is **not** a defence against a malicious client process: a compromised client can lie about its own state. The threat-model document says so plainly.
- [ ] Self-test on startup: verify kill switch is active, TUN is up, server is reachable BEFORE removing previous state
- [ ] Continuous self-test (heartbeat): drop tunnel if rules appear tampered with
- [ ] Tamper detection: detect if `nftables`/`tc`/`/dev/net/tun` were modified under us by an adversary
- [ ] Alerting on suspicious patterns (rapid reconnects, unexpected source IP, config drift)

### Anti-forensics

- [ ] Disable swap (`swapoff -a` or refuse to run with swap enabled)
- [ ] **Two distinct log channels** — see ARCH §11.1. The security-audit log (connection events, config changes, kill-switch triggers, cert lifecycle) is persistent, append-only, hash-chained, and signed by a dedicated audit signing key. The operational log (start/stop, handshake diagnostics, rekey, BPF errors) is an in-memory ring buffer drained by `unvfd logs` and journald on shutdown. **No** log line on either channel may contain a payload byte, a destination IP/port of a tunneled packet, a DNS query name, or a session key. A `unvfd lint logs` CI check scans the codebase for logger calls and rejects those that pass forbidden field types.
- [ ] Wipe sensitive material from memory after use: **explicit `Zeroize()` call** at every code path that handles keys/nonces, with `runtime.KeepAlive` to prevent premature collection. **Do not rely on `runtime.SetFinalizer`** for zeroization — finalizers are not guaranteed to run, the GC may move objects (losing the finalizer, golang/go#2559), and zeroize-after-finalize is racy. Buffers holding key material are `mlock(2)`-pinned via `syscall.Mlock` where supported, so they are not swapped to disk. **Threat-model caveat:** Go's runtime can still leave copies of the data in compiler-introduced temporaries, escape-analysis moves, and GC-managed heap; this is a known language limitation and is documented as such in the threat model (not a bug in the application).
- [ ] No telemetry, no remote reporting endpoints

### Threat model documentation

- [ ] Document what we protect against (network observer, malicious Wi-Fi, **specific** sub-cases of a compromised server — see ARCH §12.1: client/CA long-term keys not held in the server's runtime, per-server scoped identity, append-only signed audit log; a generic "compromised server" is plaintext-equivalent by design and is not promised against) and a compromised client process. **Not** a generic "compromised server" adversary — the server terminates both encryption layers and sees plaintext; that case is not in scope.
- [ ] Document what we DO NOT protect against (kernel-level rootkit on client, side channels like power/EM, physical access)
- [ ] Document assumptions (trusted kernel modules, trusted hardware)
- [ ] **Clock dependency:** cert validity, CRL `thisUpdate` / `nextUpdate`, OCSP `producedAt`, replay windows, and the audit log's timestamps all rely on the local clock being roughly correct. The runtime requires (a) an authenticated time source — NTS-secured NTP, or Roughtime — running before `unvfd` starts, and (b) on startup, `unvfd` samples the current time, compares it to the last-seen monotonic timestamp in persistent state, and refuses to start if the wall clock has jumped backwards by more than the configured `max_clock_jump` (default 24h). A backward jump is a classic revocation-bypass primitive (a stale CRL looks fresh again).
- [ ] **Fuzzing harness coverage (extended):** native Go fuzzing (`testing.F` + `go test -fuzz=...`) must cover the **self-rolled** parsers and verifiers — these are the dangerous surfaces, not the ones already battle-tested by stdlib. Targets: (a) protocol frame decoder (TUN packet framing, TLV codec, gRPC payload unmarshal), (b) in-tunnel firewall DSL parser and IR, (c) config file parser (TOML), (d) inner-KEM handshake state machine (including transcript signature path), (e) hybrid-certificate parser and chain validator (the most dangerous — a malformed-hybrid-cert corpus must ship with the test suite, see ARCH §7.2). Fuzzing runs in CI on a schedule (nightly) with a corpus, and `go test -fuzz` is run on developer workstations before pushing protocol changes.
