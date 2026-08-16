# ADR 0004 — Link-up + address-assignment mechanism (`os/exec` to `ip`)

Status: **Accepted** (2026-08-16).

## Context

TODO §"Client (Ubuntu/Debian)" contains the unchecked parent item
"Bring interface up with `ip link set ... up` and assign address".
After a TUN device is opened (ADR 0003), the daemon must:

1. Bring the interface to the `UP` state so the kernel accepts
   egress traffic on it. Default state after `TUNSETIFF` is `DOWN`.
2. Assign an IPv4 (and, in a future iteration, IPv6) address and
   prefix length so the kernel routes the configured subnet to the
   TUN.
3. (Future iteration; TODO §"Configure routing") install the
   default route.

Two candidate mechanisms exist on Linux:

- **`github.com/vishvananda/netlink`** — pure Go, no CGO, talks
  `NETLINK_ROUTE` directly. License: Apache-2.0. Maintained. Pulls
  one transitive dependency (`mdlayher/netlink`).
- **`os/exec` shell-out to the `ip` binary** — `iproute2` is
  already a hard dependency of the daemon (TODO §"Dependencies
  (Debian packages)": `iproute2` (for the `ip` command — TUN/TAP and
  routing)). No new Go module. No netlink socket held in steady
  state.

## Decision

The daemon configures link state and addresses by shelling out to
the `ip` command via `os/exec`. No Go-side netlink library is
imported.

- An abstract `Link` interface lives in
  `internal/contract/route/link.go` (Up, AddAddress, Down,
  idempotent; typed errors with stable Reason strings mirroring
  `contracttun.PreflightError`).
- A concrete `ip`-backed implementation lives in
  `internal/tunnel/route/link_linux.go` (gated by `//go:build
  linux`). It builds argv arrays, calls `ip link set ... up/down`,
  `ip addr add ... dev ...`, and checks exit codes / stderr.
- A non-Linux stub `link_other.go` returns `ReasonUnsupportedPlatform`
  from every method so cross-compile stays green (mirroring the
  pattern in `internal/tunnel/tun`).
- The `ip` binary path is configurable (default `/sbin/ip`) so the
  test suite and non-standard Linux installs can override it
  without code change.

## Rationale

Three properties decide this:

1. **No new dependency.** ARCH §6: prefer stdlib over third-party.
   `os/exec` + `fmt` + `strings` does the whole job. Adding
   `vishvananda/netlink` (plus its transitive
   `mdlayher/netlink`) gives us ~2 modules and ~5k lines of
   checked-in code that we would not exercise beyond the two
   operations in this ADR.
2. **iproute2 is already a hard runtime dependency.** TODO §"Dependencies
   (Debian packages)" lists `iproute2` (for the `ip` command — TUN/TAP
   and routing). The same binary is needed by ADR 0003's `Open` for
   the MTU lookup and by the future `Configure routing` item. We
   are not adding a new dependency; we are using one that is
   already required.
3. **No netlink socket in steady state.** A netlink-based
   implementation holds a long-lived `NETLINK_ROUTE` socket for
   the daemon's lifetime. With `os/exec`, the daemon issues a
   short-lived `ip` child process per command and the kernel-side
   netlink socket is closed when the child exits. The
   `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`
   filter in TODO §"Process & runtime hardening" still applies —
   `AF_NETLINK` is permitted — but the binary's *own* netlink
   surface stays empty.
4. **Auditable.** Every link-state change is a discrete child
   process visible in `ps`/`journald`; the operator can replay the
   sequence from the structured log alone. A long-lived netlink
   socket hides its activity from process listing.
5. **Argument structure is simple.** `ip link set <name> up` and
   `ip addr add <cidr> dev <name>` are two-flag invocations with
   no flags that vary by Linux distribution. The escaping risk is
   bounded: the only user-controlled input is the interface name
   (already validated by the kernel at TUNSETIFF) and the CIDR
   string (parsed by `net.ParseCIDR` before the call). No string
   interpolation into a shell — we use `exec.Command(name,
   args...)`, not `sh -c`, so the shell metacharacter attack
   surface is zero.

## What we lose

- **No bulk operations.** A future item might want to enumerate
  existing addresses or query link state via netlink. We add that
  via `ip -json ...` (also pure `os/exec`) when needed; the
  per-command cost is small enough that it does not justify a
  netlink socket.
- **Slightly slower.** Each command forks ~1 ms. We do at most a
  handful of these per tunnel-up event (UP + AddAddress + future
  `ip route add`), not per packet.
- **Relies on `iproute2` output parsing.** We currently only
  inspect exit codes and stderr; a future item that needs to parse
  the output of `ip -j ...` will need to handle JSON schema drift
  between iproute2 versions. Mitigated by pinning the supported
  iproute2 version range in packaging (`debian/control`
  `Depends: iproute2 (>= 5.10.0-3)`); the JSON schema has been
  stable since iproute2 5.10 (released 2021).

## Consequences

- The package `internal/tunnel/route` MUST be `//go:build linux`
  for the concrete impl; cross-compile picks up `link_other.go`.
- The contract interface in `internal/contract/route` keeps the
  `*exec.Cmd` / argv-building / shell-out code out of every other
  package's import graph; callers depend on the interface, not the
  concrete type.
- The runtime check that `iproute2` is installed happens in the
  preflight layer (TODO §"Verify `/sbin/ip` is available" is a
  future addition; for now we surface `exec.ErrNotFound` as a
  typed error and the operator sees the failure).
- CIDR strings MUST be validated with `net.ParseCIDR` before any
  `exec` call — both for safety (no shell injection even though
  we use `exec.Command` directly) and for catching typos before
  the kernel rejects them with a non-obvious error.
- Every call uses absolute argv (`exec.Command("/sbin/ip", "link",
  "set", name, "up")`), never `sh -c ...`, so command-line
  injection is structurally impossible regardless of input
  validation.

## Alternatives considered

- **`github.com/vishvananda/netlink`.** Apache-2.0, pure Go, no
  CGO, actively maintained. Rejected because (a) it adds a
  dependency for two operations, (b) it holds a long-lived
  netlink socket in the daemon, (c) `iproute2` is already
  required, (d) `os/exec` is auditable per-command.
- **`github.com/mdlayher/netlink` directly.** Lower-level than
  `vishvananda/netlink`. Same objections, plus more boilerplate
  per operation.
- **Hand-rolled `NETLINK_ROUTE` socket.** Maximum dependency
  purity (only `golang.org/x/sys/unix`), but ~600 lines of
  netlink attribute marshalling code per operation — much worse
  than shelling out to `ip` for two simple commands. Revisit only
  if `os/exec` proves a bottleneck (it will not for two calls
  per tunnel-up event).
- **Direct `ioctl(SIOCSIFADDR)` etc.** Historic RTnetlink
  predecessor. Deprecated since kernel 2.2; do not use on modern
  kernels.

## Cross-references

- ARCH §5 (module / package layout: `internal/tunnel/route`).
- ARCH §6 (dependency policy: "stdlib / minimal third-party
  surface").
- ARCH §11 / TODO §"Run with CAP_NET_ADMIN" (the `ip` invocations
  also require `CAP_NET_ADMIN`; the preflight check covers both
  TUNSETIFF and netlink).
- ARCH §13.1 (mandatory testing policy: tests ship with every
  `internal/` change; mocks are `moq`-generated, never
  hand-written; table-driven style; `testify` assertions).
- TODO §"Dependencies (Debian packages)" (`iproute2` is listed).
- TODO §"Client (Ubuntu/Debian)" (parent item; this ADR closes
  the first sub-checkbox).
- ADR 0003 (companion ADR for the TUN open path; both ADRs share
  the same `//go:build linux` + non-Linux stub structure).
