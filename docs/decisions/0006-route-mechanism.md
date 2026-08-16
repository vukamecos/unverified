# ADR 0006 — Route-config mechanism (`os/exec` to `ip route`)

Status: **Accepted** (2026-08-16).

## Context

TODO §"Client (Ubuntu/Debian)" contains the unchecked parent
item "Configure routing (`ip route add ... dev tun0`)". After
the TUN device is opened (ADR 0003) and brought `UP` with an
assigned address (ADR 0004), the daemon must point the host's
routing table at the tunnel. The canonical install on a
client is a default route (`0.0.0.0/0`) over `tun0`, replacing
— or coexisting with — the host's existing default gateway.
On disconnect / shutdown the operation is reversed.

Two candidate mechanisms exist on Linux:

- **`github.com/vishvananda/netlink`** — pure Go, no CGO, talks
  `NETLINK_ROUTE` directly. License: Apache-2.0. Maintained.
  Pulls one transitive dependency (`mdlayher/netlink`).
- **`os/exec` shell-out to the `ip` binary** — `iproute2` is
  already a hard dependency of the daemon (TODO
  §"Dependencies (Debian packages)": `iproute2`). No new Go
  module. No netlink socket held in steady state.

ADR 0004 already selected `os/exec` for the link/address
side of this work; this ADR extends that decision to the
routing side and surfaces one rule that the Link ADR did not
have to deal with: default-route manipulation is a host-wide
sentinel operation that creates a leak window if it lands
before the kill switch is active.

## Decision

The daemon configures the host's IPv4 routing table by
shelling out to `ip route add` / `ip route del` via `os/exec`.
No Go-side netlink library is imported.

- An abstract `Route` interface lives in
  `internal/contract/route/route.go` (Add, Del; idempotent;
  typed errors with stable Reason strings mirroring
  `contracttun.PreflightError` and `contractroute.LinkError`).
  IPv6 routing is out of scope for this ADR (separate ADR
  if/when needed).
- A concrete `ip`-backed implementation lives in
  `internal/tunnel/route/route_linux.go` (gated by `//go:build
  linux`). It builds argv arrays, calls `ip route add default
  dev <name>` and `ip route del default dev <name>`, and
  checks exit codes / stderr. The `ip` binary path is
  configurable (default `/sbin/ip`) so non-standard installs
  and the test suite can override it.
- A non-Linux stub `route_other.go` returns
  `ReasonUnsupportedPlatform` from every method, mirroring the
  `link_other.go` pattern from ADR 0004.
- Default-route operations are a privileged sentinel:
  `Route.Add` for `0.0.0.0/0` runs only after the kill-switch
  pre-start unit has confirmed the deny-by-default egress
  ruleset is loaded; the runtime records the prior default
  gateway at install time and either restores it on shutdown
  or, if it cannot, fails-closed (leaves the new route in
  place and refuses to unwind rather than silently
  blackholing the host).

## Rationale

Five properties decide this:

1. **No new dependency.** ARCH §6: prefer stdlib over
   third-party. `os/exec` + `fmt` + `net.ParseCIDR` does the
   job. Adding `vishvananda/netlink` (plus its transitive
   `mdlayher/netlink`) gives us ~2 modules and ~5k lines of
   checked-in code that the daemon would not exercise beyond
   "add a route / delete a route".
2. **iproute2 is already a hard runtime dependency.** TODO
   §"Dependencies (Debian packages)" lists `iproute2`. ADR
   0003 needs it for the MTU lookup (`ip link show ...`),
   ADR 0004 needs it for `link set ... up/down` and
   `addr add ...`, this ADR needs it for `route add/del`.
   We are not adding a new dependency; we are extending the
   use of one already required.
3. **No netlink socket in steady state.** A netlink-based
   implementation holds a long-lived `NETLINK_ROUTE` socket
   for the daemon's lifetime. With `os/exec`, the daemon
   forks a short-lived `ip` child per command and the
   kernel-side netlink socket closes when the child exits.
   The `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
   AF_NETLINK` filter in TODO §"Process & runtime hardening"
   still applies — `AF_NETLINK` is permitted — but the
   binary's *own* netlink surface stays empty.
4. **Auditable.** Every route change is a discrete child
   process visible in `ps` / `journald`; the operator can
   replay the sequence from the structured log alone. A
   long-lived netlink socket hides its activity from
   process listing.
5. **Argument structure is simple.**
   `ip route add default dev <name>` and
   `ip route del default dev <name>` are two-flag invocations
   with no flags that vary by Linux distribution. The
   escaping risk is bounded: the only user-controlled input
   is the interface name (already validated by the kernel
   at TUNSETIFF and re-validated against the captured-device
   regex in the Link constructor) and the destination CIDR
   (parsed by `net.ParseCIDR` before the call). No string
   interpolation into a shell — we use
   `exec.Command(name, args...)`, not `sh -c`, so the shell
   metacharacter attack surface is zero.

## Sentinel operation: the default route

The decision to use `os/exec` is unremarkable; what is
remarkable is that *changing the host's default route is
system-wide and creates a leak window if it lands before the
kill switch is up*. The naive sequence
(`AddAddress → ip route add default dev tun0 → start pump`)
is unsafe: between `ip route add` and the kill-switch unit
verifying its ruleset, default-routed traffic from any
process on the host would exit via `tun0` *into the
unencrypted net*, silently bypassing the kill switch that
is supposed to drop all outbound traffic not destined for
the tunnel.

The runtime therefore orders the install as:

1. **ADR 0003** — `Open("/dev/net/tun", O_RDWR)` + `TUNSETIFF`.
2. **ADR 0004** — `ip link set tun0 up` +
   `ip addr add 10.x.y.z/24 dev tun0`.
3. **`unvfd-killswitch.service`** (TODO §"Persistence model is
   reconcile, not rollback"; ARCH §8.1.1) — verify the
   pre-start unit has loaded the canonical ruleset and the
   `nft list ruleset | sha256sum` matches the blob's hash.
4. **Only then:** `ip route add default dev tun0` (this ADR).
5. **Only then:** start the pump (ADR 0005).

On shutdown the order reverses:

1. **Stop the pump.**
2. `ip route del default dev tun0` (this ADR).
3. Restore the prior default gateway (captured at install
   time, persisted in `/var/lib/unvfd/routes.json`).
4. `ip link set tun0 down` (ADR 0004).
5. **`unvfd-killswitch.service` removal** (explicit
   `unvfd fw disable --confirm`, audit-logged).

The fail-closed rule on step 3 of shutdown: if we cannot
restore the prior default gateway (file missing,
permission denied, address-family unsupported), the
runtime **refuses to remove the new tunnel route** and
leaves it in place. Blackholing the host is preferable to
silently leaking traffic to the wrong default gateway
during shutdown; the operator can recover manually.

This ordering is expressed as the route package's contract:
`Route.Add` does not enforce kill-switch readiness on its
own (the package is reusable in non-VPN contexts); the
runtime orchestrator enforces it. The contract documents
the requirement and provides a `KillSwitchReady` callback
the runtime passes in so the failure mode surfaces a typed
`RouteError{Reason: ReasonRouteAddFailed,
Cause: "kill switch not verified"}` instead of silently
landing a default-route change.

## What we lose

- **No bulk operations.** A future item might want to query
  the routing table or list route attributes via netlink. We
  add that via `ip -json route` (also pure `os/exec`) when
  needed; the per-command cost is small enough that it does
  not justify a netlink socket.
- **Slightly slower.** Each command forks ~1 ms. We do at
  most a handful of these per tunnel-up event — not per
  packet.
- **Relies on `iproute2` output parsing.** We currently only
  inspect exit codes and stderr; a future item that needs
  to parse `ip -j route` will need to handle JSON schema
  drift between iproute2 versions. Mitigated by pinning the
  supported iproute2 version range in packaging
  (`debian/control`: `Depends: iproute2 (>= 5.10.0-3)`);
  the JSON schema has been stable since iproute2 5.10
  (released 2021).
- **Idempotency has to be detected, not queried.** Unlike
  netlink (which returns a structured error code), `ip
  route add dev foo` on an already-present route returns
  `RTNETLINK answers: File exists` to stderr. The runtime
  parses that string to distinguish "already there" from a
  real failure. The string is not part of the public
  iproute2 contract, so the detection logic tolerates a
  missing match (treats the unknown string as a real
  failure) and matches on the well-known substring.

## Consequences

- The package `internal/tunnel/route` MUST be `//go:build
  linux` for the concrete impl; cross-compile picks up
  `route_other.go`.
- The contract interface in `internal/contract/route` keeps
  the `*exec.Cmd` / argv-building / shell-out code out of
  every other package's import graph; callers depend on the
  interface, not the concrete type.
- CIDR strings MUST be validated with `net.ParseCIDR` before
  any `exec` call — both for safety (no shell injection
  even though we use `exec.Command` directly) and for
  catching typos before the kernel rejects them.
- The runtime MUST persist the prior default gateway at
  install time and pass it to `Route.Del` (or a higher-level
  orchestrator) on shutdown, with the fail-closed rule
  above.
- IPv6 routing is out of scope for this ADR; a future ADR
  extends the same pattern to `ip -6 route`.
- The default-route operations receive the same kill-switch
  readiness check the Link operations receive in ADR 0004;
  this is enforced at the orchestrator layer, not in the
  `Route` contract itself (the package is reusable).

## Alternatives considered

- **`github.com/vishvananda/netlink`.** Apache-2.0, pure Go,
  no CGO, actively maintained. Rejected because (a) it adds
  a dependency for one or two operations, (b) it holds a
  long-lived netlink socket in the daemon, (c) `iproute2`
  is already required, (d) `os/exec` is auditable
  per-command, (e) netlink does not solve the kill-switch
  ordering problem either — that ordering is a runtime
  concern, not a mechanism choice.
- **`github.com/mdlayher/netlink` directly.** Lower-level
  than `vishvananda/netlink`. Same objections, plus more
  boilerplate per operation.
- **Hand-rolled `NETLINK_ROUTE` socket.** Maximum dependency
  purity (only `golang.org/x/sys/unix`), but ~600 lines of
  netlink attribute marshalling code per operation — much
  worse than shelling out to `ip` for one or two simple
  commands. Revisit only if `os/exec` proves a bottleneck
  (it will not for at most a handful of calls per
  tunnel-up event).
- **Direct `ioctl(SIOCADDRT)` / `SIOCDELRT`.** Historic
  RTnetlink predecessor. Deprecated since kernel 2.2; do
  not use on modern kernels.
- **`ip route add default dev tun0 metric <low>`** as a
  silent alternates-with-prior approach — keep both default
  routes and let the kernel pick. Rejected because (a) the
  kernel's metric-resolution rules are version-dependent
  and surprise-prone, (b) the operator cannot tell which
  interface a packet went out by looking at the routing
  table, (c) it makes kill-switch enforcement ambiguous:
  the kill switch catches egress by interface name, and
  with two default routes, the deny-by-default ruleset
  would need to allow-list either interface to be sure the
  packet is dropped, defeating the purpose.

## Cross-references

- ARCH §5 (module / package layout: `internal/tunnel/route`).
- ARCH §6 (dependency policy: "stdlib / minimal third-party
  surface").
- ARCH §8.1.1 (kill switch persistence model).
- ARCH §11 / TODO §"Run with CAP_NET_ADMIN" (the `ip
  route` invocations also require `CAP_NET_ADMIN`; the
  preflight check covers both TUNSETIFF and netlink).
- ARCH §13.1 (mandatory testing policy: tests ship with
  every `internal/` change; mocks are `moq`-generated, never
  hand-written; table-driven style; `testify` assertions).
- TODO §"Dependencies (Debian packages)" (`iproute2`).
- TODO §"Client (Ubuntu/Debian)" (parent item; this ADR
  closes the first sub-checkbox).
- ADR 0003 (companion ADR for the TUN open path; both
  ADRs share the same `//go:build linux` + non-Linux stub
  structure).
- ADR 0004 (companion ADR for link/address; this ADR
  extends the same `os/exec` + `ip` shell-out pattern to
  routing with a sentinel-operation carve-out).
