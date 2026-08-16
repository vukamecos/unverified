# ADR 0003 — TUN device library (`gvisor.dev/gvisor/pkg/tcpip/link/tun`)

Status: **Accepted** (2026-08-15).

## Context

TODO §"Client (Ubuntu/Debian)" asks us to create a TUN device via
`/dev/net/tun`. The candidates were:

- **`gvisor.dev/gvisor/pkg/tcpip/link/tun`** — the TUN/TAP sub-package of
  gVisor's userspace TCP/IP stack. Thin Linux wrapper around `/dev/net/tun`
  and the `TUNSETIFF` ioctl. License: Apache-2.0 / BSD-3 / MIT.
- **`github.com/sagernet/sing-tun`** — the TUN/TAP layer used by sing-box
  and Xray. Cross-platform (Linux / Windows / macOS / iOS), broader API
  surface, drags in gVisor's netstack, netlink, nftables, nfqueue, and
  the `sing` family of packages. License: **GPL-3.0-or-later**.
- **`songgao/water`** — unmaintained since 2020-03, explicitly excluded
  from the design (kept in the TODO only as a historical reference).

The choice affects the dependency surface, the binary's licence, and
what the `internal/tunnel/tun` package will look like.

## Decision

Use **`gvisor.dev/gvisor/pkg/tcpip/link/tun`**.

A thin `internal/tunnel/tun` package wraps the gvisor `*tun.Device` in a
small interface (`Open(name string) (*Device, error)`, `Read() ([]byte, error)`,
`Write([]byte) (int, error)`, `Close() error`, `Name() string`,
`MTU() (uint32, error)`), gated by `//go:build linux` so the package does
not attempt to compile on non-Linux GOOS values. The interface lives in
`internal/contract/tun` so the rest of the codebase can be tested
without a real `/dev/net/tun`.

## Rationale

1. **License.** `gvisor` is Apache-2.0 / BSD-3 / MIT, the same permissive
   set the rest of the dependency surface uses. `sing-tun` is
   **GPL-3.0-or-later**, which would propagate to the whole binary: any
   code statically linked with a GPL-3.0 library is, in the conservative
   reading, itself subject to GPL-3.0. The project ships a single static
   binary (TODO §"Single-binary distribution") and is intended to be
   re-distributable as a `.deb`; pulling in GPL-3.0 transitively would
   change the licensing story for the whole binary. Not acceptable.

2. **Dependency surface.** `gvisor.dev/gvisor/pkg/tcpip/link/tun` is
   pulled in by gvisor's own `pkg/buffer` and `pkg/tcpip` types — the
   same package gVisor's userspace netstack is built on. There is no
   extra transitive surface beyond gvisor itself. `sing-tun` pulls in
   `github.com/sagernet/gvisor`, `github.com/sagernet/netlink`,
   `github.com/sagernet/nftables`, `github.com/sagernet/sing`,
   `github.com/mdlayher/netlink`, and `github.com/florianl/go-nfqueue/v2`
   — at least six transitive Go modules we would otherwise not need.
   ARCH §6 prefers stdlib / minimal third-party surface; gvisor wins.

3. **Platform.** We are Linux-only on both the server and the client
   (ARCH §1: "Linux only (Ubuntu / Debian)"). `sing-tun`'s
   cross-platform support (Linux / Windows / macOS / iOS) is dead weight
   for this project and a maintenance liability: a Windows-specific
   bug filed against the dependency is a CVE clock we don't need to
   inherit.

4. **API surface.** `tun.Device` exposes the low-level primitives we
   actually need: a `*tun.Device` that wraps the fd, a `Read()` that
   hands back a `*buffer.View` (the raw IP packet from the kernel), a
   `Write([]byte)` for injecting packets, and `MTU()`. Anything beyond
   that — packet batching, ICMP error forwarding, traceroute, TCP NAT
   — is upstream feature work that we don't want, because our data path
   is "IP packet in, AEAD frame out" (ARCH §7) and the kernel handles
   TCP/IP semantics on both sides of the TUN.

5. **No CGO.** `gvisor`'s TUN package is pure Go + `syscall` + `unsafe`
   for the `TUNSETIFF` ioctl. ARCH §6 mandates `CGO_ENABLED=0`; sing-tun
   does not require CGO either but pulls more non-Go code into the
   dependency closure (netlink / nfqueue C bindings), which we do not
   want for a minimal-surface decision.

6. **Activity.** gVisor's `pkg/tcpip/link/tun` was released on
   2026-08-15 (the day this ADR is filed) and remains part of an
   actively-developed codebase. sing-tun is also active, but the
   activity surface is irrelevant — the decision is blocked by (1) and
   settled by (2) before activity matters.

## What we lose

- **Cross-platform TUN.** If we ever want a Windows / macOS client
  (we do not — ARCH §1), we would need to revisit this ADR and either
  vendor sing-tun's per-OS implementations behind our
  `internal/contract/tun` interface or fork a Windows-specific
  helper. Mitigation: the interface is small, and a future port is
  additive — the Linux path is unchanged.
- **Upstream niceties.** sing-tun ships packet-batching helpers,
  ICMP-error forwarding, and a traceroute path. None of these belong
  in our data path; if a future feature needs them, the right place is
  a new package built on top of our interface, not a different TUN
  library.

## Consequences

- The `internal/tunnel/tun` package MUST be `//go:build linux`. Any
  cross-compile to a non-Linux GOOS will skip this package cleanly.
- The contract interface in `internal/contract/tun` keeps `*tun.Device`
  out of every other package's import graph; callers depend on the
  interface, not the concrete type. This lets the rest of the codebase
  be tested without `/dev/net/tun` and lets a future port swap in a
  different implementation without touching the consumers.
- The build needs `CAP_NET_ADMIN` to perform the `TUNSETIFF` ioctl on
  the fd; the runtime check (TODO §"Run with CAP_NET_ADMIN") is the
  single source of truth for that requirement.
- `/dev/net/tun` MUST be available at startup; the package's `Open`
  returns a typed error on `ENOENT` / `ENXIO` / `EACCES` that the
  caller maps to a startup-failure log line.

## Alternatives considered

- **`github.com/sagernet/sing-tun`.** Cross-platform and actively
  maintained, but GPL-3.0-or-later (license incompatibility — see (1))
  and a much larger transitive dependency tree (six extra Go modules
  — see (2)).
- **`songgao/water`.** Unmaintained since 2020-03. Explicitly excluded
  by TODO §"Client (Ubuntu/Debian)".
- **Roll our own TUN wrapper.** The whole library is roughly 400 lines
  of Go: open `/dev/net/tun`, `ioctl(fd, TUNSETIFF, &ifr)`, `EpollCtl`,
  raw `Read` / `Write` on the fd. ARCH §6 prefers stdlib / minimal
  dependencies, and the gvisor package is the smallest possible
  dependency that does this correctly. A hand-rolled wrapper would
  duplicate a well-tested code path and lose the upstream security
  fixes. **Deferred:** if the gvisor dependency becomes a burden (e.g.
  a forced upgrade for an unrelated change), revisit.

## Cross-references

- ARCH §5 (module / package layout: `internal/tunnel/tun`).
- ARCH §6 (dependency policy: `gvisor.dev/gvisor/pkg/tcpip/link/tun`
  is the named default).
- ARCH §11 / TODO §"Run with CAP_NET_ADMIN" (capability requirement
  for `TUNSETIFF`).
- TODO §"Client (Ubuntu/Debian)" (parent item; this ADR resolves
  the library-choice sub-checkbox).