# VektorWeb

**VektorWeb** is a lightweight, zero-database, web-hosted 3D social protocol and desktop client/relay suite written in Go. Instead of heavy proprietary game engines, multi-gigabyte SDKs, or complex federated databases, VektorWeb treats a 3D world like a static web directory (`world.toml` + `.glb` + `world.lua`) paired with a single-port HTTP + WebSocket relay (`vektorweb.v1`) and client-side Ed25519 cryptographic keys.

---

## Live Public Demo Server

Join the official **VektorWeb Nexus Plaza** demo world hosted on `vektor.whitenode45.ddns.net`:

* **Join URL:** `https://vektor.whitenode45.ddns.net/world.toml` (or `vektorweb://vektor.whitenode45.ddns.net/world.toml`)
* **Relay Endpoint:** `wss://vektor.whitenode45.ddns.net/`
* **Live Status Endpoint:** `https://vektor.whitenode45.ddns.net/status.json`

Connect directly from the client:
```bash
./vektorweb-client -world https://vektor.whitenode45.ddns.net/world.toml
```

---

## Core Architecture

* **Web-Hosted Worlds (`world.toml`):** Any static web server—or the relay's built-in HTTP file server—can host a world folder containing `world.toml`, `environment.glb`, and an optional sandboxed `world.lua` script.
* **Single-Binary Relay (`vektorweb-relay`):** Pure Go binary with zero CGO or external database dependencies. Serves both HTTP world assets and the `vektorweb.v1` WebSocket protocol on a single port, plus an unauthenticated `/status.json` health/population endpoint and optional directory ping (`-announce`).
* **Desktop 3D Client (`vektorweb-client`):** Built with `raylib-go`, featuring walk/fly physics, 1st-person and 3rd-person orbit cameras, spatial push-to-talk voice chat (`35m` falloff), live prop building, drag-and-drop `.glb` imports, and peer-to-peer asset transfers.
* **Client-Side Ed25519 Identity & Signed Asset Licenses:** Automatically generates a local Ed25519 keypair (`identity.key`) for passwordless challenge-response authentication (`grid-auth`) and creator-signed `VEKTORWEB_LICENSE_V1` ownership certificates (supporting both vendor-locked and transferable/resellable `.glb` items), with `Nick#pubkey6` display disambiguation.
* **Hardened Asset & Script Sandboxing:** Validates incoming `.glb` geometry and buffer bounds (`glbguard`), verifies Subresource Integrity (`SHA-256`) hashes, and executes `world.lua` inside a stripped Lua VM (`luavm`) with zero OS/filesystem/network access and a strict `15ms/frame` CPU budget.

---

## Quick Start

### 1. Build
```bash
./build.sh
```
This compiles `vektorweb-relay` and `vektorweb-client` for your current platform (Linux, FreeBSD, OpenBSD, or Windows).

### 2. Start a Local Relay
```bash
./vektorweb-relay
```
On first run, `./vektorweb-relay` automatically generates a starter `./world/` directory (`world.toml`, `environment.glb`, `world.lua`, `bans.txt`, `blocked_items.txt`) and listens on `0.0.0.0:7777`.

**Relay CLI Flags:**
* `-addr 0.0.0.0:7777` — Listen host and port
* `-domain 127.0.0.1` — Domain/IP bound to Ed25519 auth challenges
* `-dir ./world` — World directory to serve
* `-http=true` — Serve world files over HTTP on the same port (set `-http=false` behind Nginx/Caddy)
* `-placement ephemeral` — Prop mode: `forbid` | `ephemeral` | `persistent`
* `-trust-proxy` — Honor `X-Real-IP` / `X-Forwarded-For` behind a reverse proxy
* `-announce <url>` — Optional public directory HTTP(S) URL to `POST` status pings every 60s
* `-admin <pubkey1,pubkey2>` — Comma-separated 64-char hex admin Ed25519 public keys

### 3. Launch the Client
```bash
./vektorweb-client -world https://vektor.whitenode45.ddns.net/world.toml
```
Or connect to your local relay:
```bash
./vektorweb-client -world http://127.0.0.1:7777/world.toml
```
To test multiplayer locally on the same machine, launch a second profile in another terminal:
```bash
./vektorweb-client -profile peer2 -nomic -nick Guest2
```

---

## Deploying to a Debian VPS (Nginx + Certbot)

Use [`setup_vps.sh`](./setup_vps.sh) to install `vektorweb-relay` as a hardened `systemd` service behind Nginx with automatic Let's Encrypt TLS (`wss://`):
```bash
sudo bash setup_vps.sh vektor.whitenode45.ddns.net 7777
```
Reload configuration, `world.toml`, `bans.txt`, or `blocked_items.txt` without dropping connected players:
```bash
sudo systemctl reload vektorweb-relay
```

---

## Client Controls & Slash Commands

### Keybinds
| Key | Action |
| :--- | :--- |
| **`WASD` + Mouse** | Move and look (`[Shift]` Sprint, `[Space]` Jump, `[Esc]` Unlock/Lock cursor) |
| **`[F]`** | Toggle Walk + Ground Collision / Free-Fly camera |
| **`[C]`** | Toggle 1st-Person / 3rd-Person camera (Mouse wheel zooms distance) |
| **`[R]` / `[H]`** | Return to world spawn point (reset prop in Build Mode) / Toggle Spawn Waypoint HUD |
| **`[1]`–`[9]`** | Switch embedded glTF animation clip on your equipped avatar |
| **`[I]`** | Open Local Inventory & Key-Signed Transfers (`[R]` to transfer/sell selected item) |
| **Drag & Drop `.glb`** | Equip `.glb` as live avatar or import into prop inventory |
| **`[G]` / `[Esc]`** | Toggle / Exit 3D Build Mode |
| **`[B]` / `[N]`** | Toggle Grid Snapping (`1m` / `45°`) / Cycle next prop in inventory |
| **`[Q]` / `[E]` / `[PgUp]` / `[PgDn]`** | Rotate prop yaw and adjust vertical offset (`[+]`/`[-]` scale) |
| **`[P]` / `[X]` / `[Del]`** | Place prop / Undo last placed prop / Delete targeted prop you own |
| **`[F4]`** | Set world spawn point to your current position (in Build Mode) |
| **`[V]` / `[M]`** | Hold Push-to-Talk spatial voice (`35m`) / Mute incoming voice |
| **`[T]` or `[/]`** | Open chat input (`[Up]`/`[Down]` command history, `Ctrl+V` paste) |
| **`[Tab]`** | Open World Portal Directory with live `/status.json` counts (`[F]` pins favorite to #1) |
| **`[F1]` / `[F2]` / `[F3]`** | Controls Help / Client Settings (`FOV`, sensitivity, fog) / Online Peers list |
| **`[F12]`** | Save screenshot to `~/.vektorweb-client/exports/` |

### Chat & Admin Commands
* `/help` — Print available commands and hotkeys in chat
* `/me <action>` — Broadcast an emote action (`* Nick#pubkey6 <action>`)
* `/outfit [save <slot>|list|<slot>]` — Save or switch local `(nick, avatar)` presets in `settings.toml`
* `/clear` — Clear the on-screen chat buffer
* `/clearmyprops` — Remove all props you placed in the current world
* `/nick <name>` — Set and persist your display nickname in `settings.toml`
* `/bookmark [name]` — Save the active world URL to your `[Tab]` World Directory (`worlds.toml`)
* `/say <message>` — Send local proximity chat (`35m` radius)
* `/w <nick|pubkey> <message>` — Send a private whisper to an online peer
* `/portal <url>` / `/back` / `/history` — Hop to a world URL, return to previous world, or list visited worlds
* `/range` — Toggle the `35m` spatial voice & `/say` proximity ground ring
* `/setspawn [x y z]` — Set spawn point (saves locally and updates `world.toml` on the relay if admin)
* `/pubkey` / `/copykey` — Print your 64-char Ed25519 public key or copy it to the clipboard
* `/mint <buyer_pubkey> <lock|free>` — Sign a `VEKTORWEB_LICENSE_V1` certificate for your active `.glb`
* `/exportworld` — Export all active world props as paste-ready `[[props]]` TOML to `exports/` & clipboard
* `/chatlog` — Export recent chat history to `exports/`
* **Admin Commands** *(requires pubkey in `rules.admins` or `-admin`)*: `/setspawn [x y z]`, `/kick <nick|pubkey>`, `/ban <nick|pubkey>`, `/clearprops`

---

## World Manifest (`world.toml`) & `.glb` Guidelines

```toml
[world]
title = "VektorWeb Nexus Plaza"
motd = "Welcome to VektorWeb! Press [F1] for controls or type /help."
author_pubkey = ""
relay_url = "wss://vektor.whitenode45.ddns.net/"
spawn = [0.0, 2.0, 15.0]

[assets]
environment = "environment.glb"
environment_sha256 = "" # Optional SHA-256 SRI hash
script = "world.lua"
script_sha256 = ""      # Optional SHA-256 SRI hash

[rules]
placement_mode = "ephemeral" # "forbid" | "ephemeral" | "persistent"
max_avatar_mb = 15
max_item_mb = 5
admins = []

[[portals]]
name = "Outbound Node"
url = "vektorweb://example.com/world.toml"
pos = [0.0, 0.1, -16.0]
radius = 1.8
```

### `.glb` Export Tips (Blender / Blockbench)
* **Format:** Binary glTF 2.0 (`.glb`) with `+Y` up and `1 unit = 1 meter`.
* **Origins:** Place prop and avatar origins at the base/soles (`Y = 0`); the client automatically aligns avatar bounding boxes to floor height.
* **Step Height:** Keep walkable stairs and ledges at or below `0.55m` per step for smooth walk collision.
* **Live Reloading:** Send `SIGUSR1` (`kill -USR1 <pid>`) to `vektorweb-relay` to reload `world.toml`, `bans.txt`, and `blocked_items.txt` without dropping connected players.

---

## Protocol Specification

See [`PROTOCOL.md`](./PROTOCOL.md) for the full `vektorweb.v1` wire specification, authentication handshake, binary spatial voice framing, and client-side signed asset transfer flow.
