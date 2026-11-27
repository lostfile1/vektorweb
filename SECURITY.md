# Security Policy

## Security Boundaries

Because VektorWeb loads untrusted remote worlds (`world.toml`, `environment.glb`, `world.lua`) and receives peer-to-peer `.glb` assets, security is enforced across four layers:

1. **Identity & Challenge-Response (`internal/identity`):** Clients authenticate with local Ed25519 keypairs signed against a per-connection random nonce, timestamp, and relay domain.
2. **Binary Asset Guard (`internal/glbguard`):** All `.glb` world, avatar, and prop files are verified for glTF 2.0 header structure, buffer bounds, URI restrictions (no external `http://` or `file://` URI fetches inside `.glb` JSON), and byte-size limits before loading into Raylib.
3. **Subresource Integrity (SRI):** `world.toml` supports `environment_sha256` and `script_sha256` checks so clients reject tampered world assets.
4. **Sandboxed Lua VM (`internal/luavm`):** `world.lua` runs with standard dangerous libraries stripped and a strict `15ms/frame` execution timeout.

## Reporting a Vulnerability

If you discover a crash, parser bypass in `glbguard`, sandbox escape in `luavm`, or denial-of-service vector in `vektorweb-relay`, please open a GitHub Security Advisory or contact the maintainer directly with reproduction steps before publishing exploit details publicly.