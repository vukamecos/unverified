# VPN tunnel over gRPC

A VPN tunnel implementation that tunnels IP traffic over a gRPC connection.

## Overview

This project provides a virtual private network (VPN) tunnel that encapsulates network packets inside gRPC streams. By leveraging gRPC as the transport layer, the tunnel benefits from features such as multiplexing, bidirectional streaming, flow control, and can traverse networks that allow HTTP/2 traffic.

## Features

- **gRPC-based transport** — encapsulates IP packets inside gRPC bidirectional streams
- **Bidirectional streaming** — simultaneous traffic in both directions over a single connection
- **Cross-platform** — written in Go (single static binary deployment)
- **HTTP/2 friendly** — works through proxies and firewalls that allow HTTP/2 traffic

## Planned Architecture

```
┌─────────────────┐         gRPC/HTTP2          ┌─────────────────┐
│  VPN Client     │ ◄─────────────────────────► │  VPN Server     │
│  (TUN interface)│   encapsulated IP packets   │  (forwarding)   │
└─────────────────┘                              └─────────────────┘
```

- **Client** — creates a local TUN interface, reads IP packets from it, and ships them through a gRPC stream to the server.
- **Server** — receives the packets, decapsulates them, and forwards them to the target network (and vice versa for return traffic).

## Status

🚧 **Under development** — the project is in its early stages. Source files, configuration, and build instructions will be added as the implementation progresses.

## License

MIT — see [LICENSE](./LICENSE).
