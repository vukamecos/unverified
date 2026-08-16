# ADR 0005 — Packet-pump mechanism (TUN ↔ gRPC stream)

Status: **Accepted** (2026-08-16).

## Context

TODO §"Client (Ubuntu/Debian)" contains the unchecked parent item
"Read IP packets from TUN, hand off to gRPC stream". After a TUN
device is opened (ADR 0003) and brought up with an address (ADR 0004),
the daemon must pump packets in both directions:

- **TUN → wire**: every IP packet the kernel writes to the TUN fd
  must be framed (`tag byte + 2-byte length + packet bytes`, ADR 0001)
  and sent on the [transport.Tunnel] bidi stream as one
  `tunnelpb.Frame` (`Tag: 0x04 | 0x06`, `Packet: raw IP`).
- **Wire → TUN**: every frame received from the peer must be
  written to the TUN fd so the kernel can deliver it to the
  application's network stack.

This is the runtime data path. Every byte that crosses the
tunnel goes through it.

## Decision

The pump is a small package `internal/tunnel/session` (the
"session state machine" already named in ARCH §5) with one
contract surface and one concrete impl. The session package owns
the goroutines and the MTU-aware buffer pool; it does NOT own
the inner-AEAD or inner-KEM logic (those are layered on top of
the [transport.Tunnel] interface in later iterations).

**Concrete shape:**

- `internal/contract/session/Pump.go` (new package) defines one
  abstract interface:

  ```go
  type Pump interface {
      Run(ctx context.Context, dev contracttun.Device, tun transport.Tunnel) error
  }
  ```

  `Run` returns nil on graceful shutdown (peer EOF, ctx cancel) and
  a non-nil error on any other failure (TUN read error, TUN write
  error, frame decode error, tunnel send error). Single contract
  method, no configuration knobs in the interface — everything
  that varies by environment is on a separate `Options` struct
  (matching the ADR 0004 `link.Options` pattern).

- `internal/tunnel/session/pump_linux.go` (`//go:build linux`)
  implements the contract with the shape described below.
- `internal/tunnel/session/pump_other.go` (`//go:build !linux`)
  returns `ReasonUnsupportedPlatform` from `Run` so cross-compile
  stays green (mirrors `tun/tun_other.go`, `route/link_other.go`).

**Goroutine shape:**

Two goroutines, one per direction. The orchestrator is stdlib
`golang.org/x/sync/errgroup` (single transitive dep, already used
by similar projects). `Run` calls `g, gCtx := errgroup.WithContext(ctx)`
and launches `g.Go(func() error { return pumpUp(gCtx, dev, tun) })` /
`g.Go(func() error { return pumpDown(gCtx, dev, tun) })`. `Run`
returns `g.Wait()`.

- **Direct loop, no channel** between the two pump directions.
  Each direction's loop reads from one source and writes to one
  sink; a channel between them would add a per-packet allocation
  and a sync point with no benefit, because both ends are already
  blocking on each other.
- **MTU-aware buffer pool**: `sync.Pool` of `[]byte` slices sized
  to `dev.MTU() + 32` (32 bytes headroom for the IP header the
  TUN prepends on read). On `TUN.Read`, `pool.Get().([]byte)`,
  slice down to the read length, hand to `tunnel.SendFrame`,
  return to the pool. The pool is a per-Pump field, not global —
  two concurrent sessions must not share buffers.
- **Shutdown symmetry**: a failure in either goroutine cancels the
  errgroup context (`errgroup.WithContext` does this
  automatically), which the partner goroutine observes via its
  read/write context. The partner then returns the *original*
  error, not the ctx-cancellation error, so the operator sees the
  root cause in the log.

**Error taxonomy (per direction):**

| Source | Error                                | Pump action                                                                                |
| ------ | ------------------------------------ | ------------------------------------------------------------------------------------------ |
| TUN    | `nil`                                | Continue loop.                                                                             |
| TUN    | `contracttun.ErrClosed`              | Return `nil` (clean local shutdown; partner will see ctx cancel).                           |
| TUN    | other `error`                        | Return `fmt.Errorf("pump: read tun: %w", err)`; partner ctx-cancels.                       |
| Tunnel | `io.EOF`                             | Return `nil` (clean peer shutdown); partner ctx-cancels.                                    |
| Tunnel | `transport.ErrClosed`                | Return `nil` (we already called Close); partner ctx-cancels.                                |
| Tunnel | other `error`                        | Return `fmt.Errorf("pump: send frame: %w", err)`; partner ctx-cancels.                      |
| ctx    | `context.Canceled` / `DeadlineExceeded` | Return `nil` (clean shutdown); partner ctx-cancels.                                       |

The asymmetry: TUN/Tunnel "closed cleanly" errors map to `nil`
because the session is over and there is nothing to log. Anything
else is an error worth surfacing.

**Tag dispatch (TUN → wire):**

The first byte of the IP packet (IPv4: `0x4*`, IPv6: `0x6*`) selects
the `Frame.Tag`. A leading byte that matches neither is a kernel
bug or a malicious write to the fd (TUN is root-only, so the
former is more likely, but we still fail closed rather than feed
garbage to the peer). The check is the single-byte form of the
classification in ADR 0001 — no fancier parser.

## Rationale

1. **No new runtime dependency.** `golang.org/x/sync/errgroup`
   is the only third-party module added by this ADR, and it is
   already pulled in transitively by `golang.org/x/sys` (see
   `go.sum`). The rest is stdlib (`sync.Pool`, `context`,
   `errors`).
2. **errgroup is the standard idiom for "fan-out N goroutines,
   return the first error, cancel the rest".** Re-implementing
   this with `sync.WaitGroup` + a `chan error` is ~30 lines that
   do not earn their place.
3. **No channel between pump directions.** Channels are the
   right primitive when you have N producers / M consumers or
   when you want to decouple backpressure from the producer's
   pace. Here the producers and consumers are 1:1 with the
   kernel / gRPC stream — the natural rate limiter on each side
   is the kernel's read syscall and gRPC flow control on the
   `SendFrame` call. Inserting a channel would (a) add a per-
   packet allocation for the channel send, (b) introduce a
   single-point-of-failure lock-free queue, and (c) NOT
   decouple anything because the two ends are already
   synchronously blocked.
4. **sync.Pool for read buffers.** `TUN.Read` returns one packet
   per call (no aggregation). Allocating a fresh `[]byte` per
   packet stresses the GC at line-rate. A per-Pump `sync.Pool`
   of MTU-sized slices keeps the steady-state allocation count
   at zero. The pool is per-Pump, not global, so two concurrent
   sessions don't fight over the same buffers — but the
   `sync.Pool` cost is small enough that we could revisit
   (`go vet` will warn about per-goroutine pools, but per-Pump
   is fine).
5. **MTU headroom.** The Linux TUN driver prepends a 4-byte
   `struct tun_pi` header (if TUNSETIFF was opened with
   `IFF_NO_PI` clear) or returns the packet verbatim (with
   `IFF_NO_PI` set). ADR 0003 opens the device with `IFF_NO_PI`,
   so the buffer is sized exactly to MTU. The "+32 headroom"
   in the description above is a belt-and-braces margin for
   drivers that ignore `IFF_NO_PI`; the code reads into the
   buffer up to `cap(buf)` and slices to the returned count.
6. **errgroup cancels on first error, but we still return the
   root cause.** The partner goroutine must see ctx-cancel and
   return cleanly; the orchestrator returns `g.Wait()` which
   surfaces the first non-nil error from any goroutine, so the
   operator sees the *real* failure (a TUN read error, say)
   rather than `context.Canceled` (the consequence of the
   failure).

## What we lose

- **No batching of multiple TUN packets into one wire frame.**
  The TUN driver delivers one packet per `read(2)` call, and
  the codec frames one packet per `Frame`. A future iter could
  batch N packets into one `Frame` (with N sub-length prefixes)
  to amortise gRPC framing overhead, but the marginal benefit
  is small (gRPC + HTTP/2 already coalesce) and the added
  complexity (out-of-order delivery within a batch, reorder
  window on the read side) is not worth it today.
- **No fast-path eBPF XDP redirect.** TODO §"eBPF packet
  handling (client & server)" lists an optional XDP/TC redirect
  that bypasses the userspace read loop. That is a separate,
  additive item; the pump does not preclude it. When the XDP
  path lands, the `contracttun.Device` interface may grow a
  second method, or a sibling interface will be introduced; the
  pump is the right place to dispatch between them.
- **No read-side zero-copy.** `sync.Pool` is reuse, not
  zero-copy. A future kernel-bypass path (`AF_XDP` /
  `io_uring`) would require a different buffer model entirely;
  revisit only when profiling shows the userspace read is the
  bottleneck (it almost certainly will not be at 1 Gbps).

## Consequences

- The package `internal/tunnel/session` MUST be `//go:build linux`
  for the concrete impl; cross-compile picks up `pump_other.go`.
- The contract interface in `internal/contract/session/Pump.go`
  is tiny (one method) — the moq rule (ARCH §13.1) does not even
  apply for a one-method interface, but the package still
  ships a `pump_mock.go` for symmetry with `internal/contract/route`
  (moq is always used for *contract* interfaces; the size
  carve-out was an earlier mistake corrected in iter 9).
- `golang.org/x/sync` is added to `go.mod` as a direct dep
  (currently transitive via `golang.org/x/sys`).
- The pump does NOT touch inner-AEAD or inner-KEM. Those are
  layered on top of the [transport.Tunnel] interface in a later
  iteration; the pump just sees an already-secured stream.
- This ADR splits the TODO parent item into N sub-items
  (mechanism, contract interface, concrete impl, non-Linux
  stub, contract tests via moq, integration test). This iter
  closes the first.

## Alternatives considered

- **Two separate methods (`PumpUp`, `PumpDown`) on the
  contract.** Rejected — callers (session package) always want
  both directions, and exposing them as separate methods would
  invite callers to start one without the other, leaving the
  pump half-alive. One `Run` method enforces the invariant.
- **Single goroutine with `select { case tun.Read; case
  tunnel.RecvFrame }`.** Rejected because the two operations
  have different blocking patterns and one slow direction
  should not stall the other. The TUN direction is limited by
  the local network; the tunnel direction is limited by the
  remote peer. Two goroutines let the OS scheduler interleave
  them, and errgroup keeps the shutdown logic simple.
- **`chan []byte` between the two directions.** Rejected — see
  rationale point 3.
- **One long-lived global buffer pool.** Rejected — two
  concurrent sessions would fight over the same buffers, and
  `sync.Pool`'s per-P (processor) stealing semantics make
  accounting harder. Per-Pump is correct.
- **Manual `sync.WaitGroup` + `chan error` instead of
  errgroup.** Rejected — re-implements errgroup poorly.
- **No buffer pool, allocate per packet.** Rejected — wastes
  GC at line-rate. Trivial to add later if profiling shows
  the pool is hot.

## Cross-references

- ARCH §5 (module / package layout: `internal/tunnel/session`,
  `internal/contract/session`).
- ARCH §6 (dependency policy: stdlib + minimal third-party;
  `golang.org/x/sync/errgroup` is the only addition).
- ARCH §7.1 (the inner-AEAD layer is *not* part of this pump;
  the pump sees a secured stream).
- ARCH §11 / TODO §"Process & runtime hardening" (the pump
  runs in the long-lived root-capable process; it does not
  open new fds beyond the TUN and the existing gRPC stream).
- ARCH §13.1 (mandatory testing policy: the contract mock for
  `Pump` is `moq`-generated; table-driven style; `testify`).
- ADR 0001 (packet encoding: tag byte + 2-byte length + packet).
- ADR 0003 (TUN open path; the pump consumes the
  `contracttun.Device` interface from there).
- ADR 0004 (link-up + address-assignment; runs *before* the
  pump starts).