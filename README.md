# VektorWeb

A lightweight, web-server-hosted 3D virtual environment built in Go and Raylib.

VektorWeb treats 3D worlds like static websites: any standard HTTP server (Nginx, Apache, Caddy, or the built-in single-port `vektorweb-relay`) can serve a world directory (`world.toml`, `environment.glb`, `world.lua`), while WebSockets handle live multiplayer presence, Ed25519 identity authentication, proximity voice chat, and P2P `.glb` prop/avatar streaming.

## Architecture

* **`vektorweb-relay`**: Single-binary HTTP static file host + WebSocket state/voice/asset relay (default port `7777`). Automatically initializes a starter plaza in `./world/` on first launch.
* **`vektorweb-client`**: Native desktop 3D Raylib client featuring grounded walk physics, mesh raycast collision, local `.glb` inventory validation (`glbguard`), spatial voice chat, and `vektorweb://` portal hopping.
* **Client-Owned Identity & Assets**: Ed25519 keypairs, avatars, and cached props live locally in `~/.vektorweb-client/`—no central account server required.

## Quick Start

### 1. Build from Source

Requires Go (`1.22+`) and Raylib C dependencies (`libgl1-mesa-dev`, `libx11-dev`, `libxrandr-dev`, `libxi-dev`, `libxcursor-dev`, `libxinerama-dev`, `libasound2-dev` on Debian/Ubuntu).

```sh
chmod +x ./build.sh
./build.sh
```

*(Pass `./build.sh release` to cross-compile `vektorweb-relay` across Linux, FreeBSD, OpenBSD, macOS, and Windows).*

### 2. Run Locally

Start the relay in Terminal 1:

```sh
./bin/vektorweb-relay
```

Launch the 3D client in Terminal 2:

```sh
./bin/vektorweb-client -nick "yourname"
```

### 3. Test Multiplayer Side-by-Side

Use `-profile` and `-nomic` to launch a second local client with an isolated Ed25519 identity and non-competing audio capture:

```sh
./bin/vektorweb-client -profile peer2 -nick "guest" -nomic
```

## Controls

| Key / Input | Action |
| :--- | :--- |
| `WASD` + Mouse | Walk & Look (`Left Shift` Sprint, `Space` Jump) |
| `F` | Toggle Walk + Ground Collision / Free-Fly Camera |
| `Esc` / `Left Click` | Unlock / Re-lock Mouse Cursor |
| `I` | Open Local Inventory (`1` Props, `2` Avatars, `Enter` Equip/Arm) |
| `G` / `P` | Toggle Ghost Placement Preview / Place Armed Prop |
| `T` | Open Text Chat |
| `V` (Hold) | Push-to-Talk Spatial Voice Chat |
| `Tab` | Open World Directory & Portal Hop (`vektorweb://`) |
| `F1` | Toggle Controls Help Overlay |

## Hosting a World on a VPS

1. Copy `./bin/vektorweb-relay` to your server (e.g., `/opt/vektorweb/`).
2. Run once to generate `./world/world.toml`, then set your public IP or domain in `world.toml`:

   ```toml
   name = "Starter VektorWeb Plaza"
   relay_url = "ws://YOUR_SERVER_IP:7777/"
   environment_glb = "environment.glb"
   script = "world.lua"
   ```
3. Or start directly with `-domain`:

   ```sh
   ./vektorweb-relay -addr 0.0.0.0:7777 -domain YOUR_SERVER_IP
   ```
4. Remote clients connect via:

   ```sh
   ./bin/vektorweb-client -world http://YOUR_SERVER_IP:7777/world.toml
   ```

*(For maximum performance behind Nginx with TLS, serve static files directly from disk via Nginx and run `./vektorweb-relay -http=false` on localhost.)*

## CLI Flags

* **`vektorweb-relay`**: `-addr` (default `0.0.0.0:7777`), `-world` (default `./world`), `-domain`, `-http` (default `true`).
* **`vektorweb-client`**: `-world`, `-nick`, `-avatar`, `-profile`, `-nomic`.

## Development Notes

Built as an open-source hobbyist desktop project with AI-assisted iteration, focusing on minimal dependencies, single-binary deployment, and standard open formats (`.glb`, `.toml`, `.lua`).