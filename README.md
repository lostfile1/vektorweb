# VektorWeb

![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat-square&logo=go)
![Raylib](https://img.shields.io/badge/Renderer-Raylib-lightgrey?style=flat-square)
![License](https://img.shields.io/badge/Architecture-P2P%20Hybrid-blue?style=flat-square)
![AI Assisted](https://img.shields.io/badge/Development-AI%20Assisted-purple?style=flat-square)

**VektorWeb** is a lightweight, decentralized 3D virtual world client and stateless relay built in Go and Raylib. Worlds are self-hosted over standard HTTP(S) (`world.toml`, `environment.glb`, `world.lua`), players authenticate with local Ed25519 cryptographic keypairs, and 3D `.glb` avatars and props transfer peer-to-peer with local client caching and `glbguard` validation.

> **Transparency & Development Note:** VektorWeb is a hobbyist open-source project built using an AI-assisted coding workflow. Architecture design, protocol rules, feature direction, security constraints, and live deployment testing are human-directed, while Go implementation and refactoring were written collaboratively with AI assistance. All code is tested on real hardware and live relays before release.

---

## Architecture Overview

- **Stateless Relay (`vektorweb-relay`):** Handles Ed25519 challenge-response authentication, player presence/pose forwarding, spatial proximity voice (`35m`), chat, prop transform sync, and optional `/directory.json` world discovery.
- **P2P Asset Swarm (`vektorweb-client`):** Transfers `.glb` avatars and props directly between connected peers in 48 KB chunks, verifies SHA-256 hashes, enforces `glbguard` size/geometry limits (`5 MB` props / `15 MB` avatars / `50 MB` worlds), and caches validated assets locally in `~/.vektorweb-client/cache/`.
- **Sandboxed World Scripting (`world.lua`):** Runs per-client inside a restricted Lua VM with zero filesystem, OS, or network access and a strict `15ms/frame` CPU budget.
- **Cryptographic Asset Licensing (`VEKTORWEB_LICENSE_V1`):** Creators can mint Ed25519-signed vendor-locked or transferable licenses when trading avatars and props in-world.
- **Companion Creation Suite (`VektorWeb Studio`):** Asset and world generation is kept in a separate standalone application (`vektorweb-studio`) so the runtime client and relay remain lean.

---

## Quick Start

### 1. Build Client & Relay

```bash
chmod +x build.sh
./build.sh
```

### 2. Run a World Relay

```bash
./vektorweb-relay -addr 0.0.0.0:7777 -dir ./world -placement ephemeral
```

On first launch, `vektorweb-relay` automatically initializes `./world/` with a starter `world.toml`, `environment.glb`, `world.lua`, `bans.txt`, and `blocked_items.txt`.

#### Relay CLI Flags

| Flag | Default | Description |
| :--- | :--- | :--- |
| `-addr` | `0.0.0.0:7777` | Listen address (`host:port`) |
| `-domain` | *(auto)* | Public domain or IP bound to Ed25519 challenges |
| `-dir` | `./world` | World directory containing `world.toml`, `environment.glb`, and `world.lua` |
| `-http` | `true` | Serve world files, `/status.json`, and `/directory.json` over HTTP |
| `-placement` | `ephemeral` | Prop placement policy: `forbid` \| `ephemeral` \| `admin_only` \| `persistent` |
| `-trust-proxy` | `false` | Trust `X-Real-IP` / `X-Forwarded-For` from Nginx or Caddy |
| `-announce` | `""` | Optional federated directory URL to POST live status pings every 60s |
| `-admin` | `""` | Comma-separated 64-char hex Ed25519 admin public keys |

### 3. Launch the Client

```bash
# Connect to a local or remote world.toml (supports http://, https://, or vektorweb://):
./vektorweb-client -world http://127.0.0.1:7777/world.toml
```

Live demo node: `vektorweb://vektor.whitenode45.ddns.net/world.toml`

---

## Relay Operator Console

When `vektorweb-relay` is started in an interactive terminal (TTY), it provides a built-in operator console on standard input (`stdin`). When running detached in the background (such as under `systemd`, `rc.d`, or `nohup`), the console automatically disables itself so it will not consume CPU or exit on EOF.

### Console Commands

| Command | Description |
| :--- | :--- |
| `help` | Print available operator console commands |
| `status` | Display world title, online player count, active prop count, placement mode, and uptime |
| `reload` | Immediately reload `world.toml`, `bans.txt`, and `blocked_items.txt` from disk (also triggered by `kill -USR1 <pid>`) |
| `say <message>` | Broadcast a server-wide announcement to all connected players as `[SERVER]` |
| `kick <nick\|pubkey>` | Disconnect an online player by nickname or Ed25519 public key prefix |
| `ban <nick\|pubkey>` | Disconnect an online player (or ban an offline 64-char hex pubkey) and append their key to `bans.txt` |
| `clearprops` | Remove all placed props from the world and broadcast `PROP_REMOVE` to all clients |

### In-Game Admin Commands

Operators whose 64-char Ed25519 public key is listed in `-admin` or `world.toml` (`rules.admins`) can also run moderation commands directly from the client chat bar (`/`):

- `/setspawn` or `/setspawn <x> <y> <z>` — Update and save the world's spawn coordinates in `world.toml`
- `/kick <nick|pubkey>` — Kick a player from the relay
- `/ban <nick|pubkey>` — Ban a player and persist their key to `bans.txt`
- `/clearprops` — Clear all placed props in the world

---

## Client Controls & Shortcuts

| Key / Input | Action |
| :--- | :--- |
| `WASD` + Mouse | Walk / Look (`[Shift]` Sprint, `[Space]` Jump, `[F]` Toggle Walk / Free-Fly) |
| `[C]` / `[R]` | Toggle 3rd-Person Camera (Scroll wheel zooms) / Return to Spawn |
| `[I]` | Open Local Inventory (`[U]` Upload `.glb`, `[O]` Open folder, `[R]` Transfer signed item) |
| `Drag & Drop .glb` | Import `.glb` avatar or prop directly into your client |
| `[G]` / `[Esc]` | Toggle / Exit Ghost Build Mode (`[B]` Grid snap, `[N]` Next prop) |
| `[Q/E]` `[Z/X]` `[PgUp/Dn]` | Rotate Yaw / Tilt Pitch & Roll / Adjust Height (`[+/-]` Scale, `[P]` Place) |
| Look-At Target | `[E]` Interact with prop / Offer trade to peer, `[C]` Clone own prop, `[Del]` Remove |
| `[V]` / `[M]` / `[U]` | Hold Push-to-Talk (`35m`) / Mute incoming voice / Mute world radio stream |
| `[Tab]` | Open World Directory (`[N]` Connect to new URL, `[F]` Pin #1, `[Del]` Remove, `[R]` Refresh) |
| `[T]` or `[/]` | Open Chat (`/help`, `/uploadavatar`, `/outfit`, `/exportworld`, `/clearmyprops`, `/nick`, `/bookmark`) |
| `[F1]` / `[F2]` / `[F3]` / `[F12]` | Controls Help / Client Settings / Online Peers List / Save Screenshot to `exports/` |

---

## Contributing

Bug reports, security reviews, and pull requests are welcome. Please keep external dependencies minimal and preserve the stateless relay / P2P asset split.