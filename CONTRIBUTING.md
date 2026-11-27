# Contributing to VektorWeb

Thank you for your interest in contributing to **VektorWeb**!

## Development Transparency

VektorWeb is developed transparently with AI-assisted coding tools alongside human architectural direction and live testing. Contributors are welcome to use either manual or AI-assisted workflows, provided all submitted code is understood, clean, and tested against a live `vektorweb-relay` and `vektorweb-client` build before opening a pull request.

## Architectural Principles

To keep VektorWeb fast, portable, and easy to self-host, please respect these core constraints:

1. **Keep the Relay Stateless & Lightweight:** `vektorweb-relay` coordinates Ed25519 authentication, presence/pose updates, spatial voice packets, chat, and prop metadata. Do not add heavy physics engines, media transcoding, or mandatory external databases to the relay.
2. **Preserve the P2P-Hybrid Asset Split:** `.glb` avatars and props must remain content-addressed by SHA-256, validated through `internal/glbguard`, and transferred peer-to-peer with local client caching.
3. **Strict Client Sandboxing:** `world.lua` scripts execute untrusted remote code on the client. Never expose `os`, `io`, `package`, `debug`, or raw network sockets to the Lua VM.
4. **Minimal Dependencies:** Avoid adding third-party Go modules unless strictly necessary.

## Local Testing Checklist

1. Build both binaries cleanly:
   ```bash
   ./build.sh
   ```
2. Start a local relay:
   ```bash
   ./vektorweb-relay -addr 127.0.0.1:7777 -dir ./world -placement ephemeral
   ```
3. Launch two local client profiles (`-nomic` on the second instance) to verify multiplayer state sync, P2P `.glb` prop/avatar transfers, chat, and portals:
   ```bash
   ./vektorweb-client -world http://127.0.0.1:7777/world.toml
   ./vektorweb-client -world http://127.0.0.1:7777/world.toml -profile peer2 -nomic
   ```