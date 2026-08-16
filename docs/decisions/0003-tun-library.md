# ADR 0003 — TUN device implementation (stdlib `x/sys/unix`, no gvisor)

Status: **Accepted** (2026-08-15, revised 2026-08-16).

## Context

TODO §"Client (Ubuntu/Debian)" asks us to create a TUN device via
`/dev/net/tun`. The original candidate set:

- **`gvisor.dev/gvisor/pkg/tcpip/link/tun`** — the TUN/TAP sub-package
  of gVisor's userspace TCP/IP stack. License: Apache-2.0 / BSD-3 /
  MIT.
- **`github.com/sagernet/sing-tun`** — used by sing-box / Xray.
  License: **GPL-3.0-or-later**.
- **`songgao/water`** — unmaintained since 2020-03.

## Decision

The TUN device is opened via a direct syscall on `/dev/net/tun` +
`TUNSETIFF` ioctl using stdlib `syscall` and `golang.org/x/sys/unix`.
No third-party TUN library is imported.

- The abstract `Device` interface lives in
  `internal/contract/tun/tun.go`.
- The concrete implementation lives in
  `internal/tunnel/tun/tun_linux.go` (gated by `//go:build linux`).
- `internal/tunnel/tun/tun_other.go` is the non-Linux stub that
  returns a clear error from `Open` so cross-compile stays green.
- The TUN fd is wrapped in an `*os.File` (so Close is idempotent via
  the kernel's close-on-exec and our `atomic.Bool` guard).
- `MTU()` is queried with a separate `SIOCGIFMTU` ioctl on an
  `AF_INET` control socket — read at call time, not cached, because
  the operator may change it via `ip link set tun0 mtu ...`.

## Rationale

The first version of this ADR (2026-08-15) committed to using the
gvisor TUN sub-package. While implementing the wrapper, the actual
open path turned out to be:

1. `unix.Open("/dev/net/tun", O_RDWR, 0)` — one syscall.
2. Build a `struct ifreq { name[16]; flags; _ }` and call
   `SYS_IOCTL(fd, TUNSETIFF, &ifreq)` — one syscall.
3. `unix.SetNonblock(fd, true)` — one syscall.

That's three syscalls. The gvisor package wraps the same three
syscalls in a 30-line function (`tun.open` in
`pkg/tcpip/link/tun/tun_unsafe.go`) but pulls in gvisor's entire
dependency closure: `pkg/buffer`, `pkg/tcpip` (NetworkProtocolNumber
etc.), `pkg/waiter`, the `stateify` codegen annotations, and a much
larger `go.sum`. Re-examining the dependency surface after seeing
the actual code, importing gvisor for three syscalls is the wrong
trade — the licensing and transitive-bloat arguments from the
original ADR still hold, but a third option beats both candidates:

**Just call the syscall directly, using `x/sys/unix`.**

That gives us:

1. **License cleanliness.** Apache-2.0 / BSD-3 / MIT — the same set
   as the rest of the binary. No GPL transitive surface, no gvisor
   module at all.
2. **Dependency surface.** `golang.org/x/sys` is a foundational
   Go module that the rest of the binary pulls in anyway (for
   `unix.Socket`, `unix.IoctlSetPointer`, `unix.SOCK_*`, etc., used
   in upcoming netlink / nftables wrappers). One indirect-free
   require line, ~600 KB of checked-in stdlib-adjacent code.
3. **Code review surface.** The full TUN open + ioctl is 30 lines
   of Go, readable in one screen, with no `unsafe` tricks beyond
   the one `unsafe.Pointer(&req)` the ioctl needs. Gvisor's wrapper
   is the same 30 lines plus an additional layer of `buffer.View`
   / `stack.Stack` integration that we don't use.
4. **No drift.** A future gvisor refactor that renames or restructures
   `pkg/tcpip/link/tun` cannot break our build.

The `gvisor.dev/gvisor` module is therefore **not required** for
the TUN path. It remains a permissible choice for the future
userspace netstack (§5 of ARCH, where it would replace the kernel
TCP/IP stack on the server) — but that is a different decision and
this ADR does not lock it in.

## What we lose

- **No cross-platform TUN out of the box.** We are Linux-only per
  ARCH §1, so this is moot. A Windows / macOS port would re-evaluate
  the dependency at that time, behind the same `contract/tun`
  interface.
- **No upstream niceties from gvisor.** The gvisor wrapper has the
  same Read/Write/MTU surface; the broader gvisor stack has features
  we don't need (packet batching, ICMP error forwarding). These do
  not belong in our data path.

## Consequences

- The `internal/tunnel/tun` package MUST be `//go:build linux`.
  Cross-compile to a non-Linux GOOS skips the implementation and
  picks up the stub `tun_other.go`, which returns a typed error
  from `Open`.
- The contract interface in `internal/contract/tun` keeps the
  *os.File / atomic.Bool / linux-specific code out of every other
  package's import graph; callers depend on the interface, not the
  concrete type. This lets the rest of the codebase be tested
  without `/dev/net/tun` and lets a future Windows port swap in a
  different implementation without touching the consumers.
- The build needs `CAP_NET_ADMIN` to perform the `TUNSETIFF` ioctl
  on the fd; the runtime check (TODO §"Run with CAP_NET_ADMIN") is
  the single source of truth for that requirement.
- `/dev/net/tun` MUST be available at startup; the package's `Open`
  surfaces the underlying syscall error so the caller can switch on
  it: `EPERM` = no caps, `ENXIO` = no `/dev/net/tun` (kernel module
  `tun` not loaded), `EBUSY` = name taken.

## Alternatives considered

- **`github.com/sagernet/sing-tun`.** Cross-platform and actively
  maintained, but GPL-3.0-or-later (would propagate to the whole
  static binary) and a much larger transitive dependency tree
  (six extra Go modules). Rejected at iter 5 (this ADR's prior
  version) and remains rejected.
- **`gvisor.dev/gvisor/pkg/tcpip/link/tun`.** Apache-2.0/BSD-3/MIT,
  Linux-only, no CGO — but for the three-syscall open path we
  need, pulling in gvisor's netstack is overkill. Rejected at iter 6
  after seeing the actual open code (~30 lines of `unix.Open` +
  `unix.IoctlSetPointer`).
- **`songgao/water`.** Unmaintained since 2020-03. Explicitly
  excluded by TODO §"Client (Ubuntu/Debian)".
- **Hand-rolled TUN wrapper.** The original worry was that we'd
  duplicate a well-tested code path. The path is three syscalls
  and a struct ifreq — re-using `x/sys/unix` means we are
  re-using the upstream Go-flavoured syscall bindings, not
  duplicating them.

## Cross-references

- ARCH §5 (module / package layout: `internal/tunnel/tun`).
- ARCH §6 (dependency policy: "stdlib / minimal third-party surface").
- ARCH §11 / TODO §"Run with CAP_NET_ADMIN" (capability requirement
  for `TUNSETIFF`).
- TODO §"Client (Ubuntu/Debian)" (parent item; this ADR resolves
  the library-choice sub-checkbox and the dep-pin + minimal-package
  sub-checkbox).