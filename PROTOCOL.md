# VektorWeb Relay Wire Protocol (`vektorweb.v1`)

This specification defines the complete HTTP and WebSocket wire protocol for **VektorWeb** (`proto_version = 1`), enabling third-party clients (desktop 3D engines, terminal/TUI clients, browser WebGL viewers, or VR clients) to connect to any `vektorweb-relay` instance.

---

## 1. World Discovery, Health & Static Assets (HTTP)

A VektorWeb world is addressed by the HTTP(S) URL of its `world.toml` manifest (e.g., `http://127.0.0.1:7777/world.toml`).

### `GET /status.json` (Lightweight Health & Population Endpoint)
Returns unauthenticated JSON metadata for server browsers, health checks, and bots:
```json
{
  "proto_version": 1,
  "title": "VektorWeb Nexus Plaza",
  "domain": "127.0.0.1",
  "placement_mode": "ephemeral",
  "online": 2,
  "props": 4,
  "uptime_sec": 3600
}
```

### `world.toml` Schema

```toml
[world]
title = "VektorWeb Nexus Plaza"
author_pubkey = ""
relay_url = "ws://127.0.0.1:7777/"
spawn = [0.0, 2.0, 15.0]

[assets]
environment = "environment.glb"
environment_sha256 = ""
script = "world.lua"
script_sha256 = ""

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

- **Coordinate System:** Right-handed Cartesian coordinates in meters (`+Y` up, `-Z` forward).
- **Forbidden Paths:** Relays MUST NOT serve `bans.txt`, `blocked_items.txt`, `world_props.json`, or dot-prefixed hidden files over HTTP.

---

## 2. WebSocket Transport & Handshake

Clients connect to `relay_url` over WebSocket (advertising `Sec-WebSocket-Protocol: vektorweb.v1`).

### Step 1: Server `CHALLENGE`
```json
{
  "type": "CHALLENGE",
  "proto_version": 1,
  "nonce": "<64-char lowercase hex random nonce>",
  "timestamp": 1764080000
}
```

### Step 2: Client `AUTH` (within 10 seconds)
The client signs `"grid-auth:<nonce>:<timestamp>:<domain>"` using its Ed25519 private key:
```json
{
  "type": "AUTH",
  "proto_version": 1,
  "pubkey": "<64-char hex Ed25519 public key>",
  "signature": "<128-char hex Ed25519 signature>",
  "nick": "whitepaperkat",
  "avatar_hash": "<64-char hex SHA-256 of avatar .glb, or empty>",
  "capabilities": ["3d_glb", "voice_pcm", "props", "transfers"]
}
```

### Step 3: Server `WELCOME`, Spawn Sync & World Replay
```json
{
  "type": "WELCOME",
  "proto_version": 1,
  "pubkey": "<authenticated 64-char hex pubkey>",
  "nick": "<sanitized display name>",
  "channel": "ephemeral"
}
```
Immediately following `WELCOME`, the relay sends:
1. A `SPAWN_UPDATE` frame (`{"type":"SPAWN_UPDATE","pos":[x,y,z]}`) with the active world spawn coordinates.
2. All active `PROP_SPAWN` frames in the world.
3. The last known `STATE` frame for every currently connected peer so newcomers immediately see stationary players.

---

## 3. World Frames (`STATE`, `SPAWN_UPDATE`, `LEAVE`, `CHAT`, `PROP_SPAWN`, `PROP_REMOVE`, `REQ_ASSET`, `ASSET_CHUNK`, `ERROR`)

All JSON frames share a common envelope (`WireMsg`). The relay always overwrites `pubkey` on incoming frames with the authenticated session key so peers cannot spoof sender identity.

- **`STATE` (Client <-> Relay, Broadcast):** `{"type":"STATE","pubkey":"...","nick":"...","avatar_hash":"...","pos":[x,y,z],"yaw":0.0}`
- **`SPAWN_UPDATE` (Relay -> Clients, Broadcast):** `{"type":"SPAWN_UPDATE","pos":[x,y,z]}` (Sent on join and whenever an admin updates the world spawn via `/setspawn`.)
- **`LEAVE` (Relay -> Clients, Broadcast):** `{"type":"LEAVE","pubkey":"..."}`
- **`CHAT` (Client <-> Relay):** `{"type":"CHAT","pubkey":"...","nick":"...","channel":"global|proximity|whisper","target_pub":"...","text":"...","timestamp":0}`
- **`PROP_SPAWN` (Client <-> Relay):** `{"type":"PROP_SPAWN","pubkey":"...","prop_id":"...","hash":"...","pos":[x,y,z],"yaw":0.0,"scale":1.0}`
- **`PROP_REMOVE` (Client <-> Relay):** `{"type":"PROP_REMOVE","pubkey":"...","prop_id":"..."}` (Admins listed in `rules.admins` or `-admin` may remove any prop.)
- **`REQ_ASSET` & `ASSET_CHUNK` (Peer-to-Peer `.glb` Streaming via Relay):** Direct peer-to-peer asset transfer (`target_pub` addressed, `<= 128 KB` Base64 chunk per frame).
- **`ERROR` (Relay -> Client):** `{"type":"ERROR","code":"ERR_...","message":"..."}`

### Admin Chat Commands (When Authenticated `pubkey` Is in `rules.admins` or `-admin`)
- `/setspawn [x y z]`: Updates `[world] spawn` in `world.toml` (using the admin's current position if coordinates are omitted) and broadcasts `SPAWN_UPDATE`.
- `/kick <nick|pubkey_prefix>`: Disconnects the target peer immediately.
- `/ban <nick|pubkey>`: Appends the target's Ed25519 pubkey to `bans.txt` and disconnects them.
- `/clearprops`: Clears all active props from the world and broadcasts `PROP_REMOVE`.

---

## 4. Client-Side Signed Asset Ownership & Transfers (Over `CHAT` Whisper + `REQ_ASSET`/`ASSET_CHUNK`)

Key-signed prop/avatar licensing and P2P transfers happen purely between clients using `CHAT` (`channel = "whisper"`, prefixed `VWXFER:`) and `REQ_ASSET` / `ASSET_CHUNK`—zero relay state or database required:

1. **Vendor/Creator Minting (Client-Side):** A creator signs a canonical `VEKTORWEB_LICENSE_V1` certificate (`asset_hash`, `creator_pub`, `owner_pub`, `transferable`, `issued_at`, `nonce`, `signature`) with their local Ed25519 key.
2. **Client-Side Enforcement:** Clients verify `creator_pub`'s signature and `owner_pub == local_pubkey` before loading a licensed asset into inventory or transferring it. If `transferable == false`, the buyer's client refuses to re-issue or export the `.glb`.

---

## 5. Spatial Voice Transport (Binary Frames)

- **Client -> Relay:** `[0x02][16kHz mono s16le PCM payload...]` (`<= 32 KB`)
- **Relay -> Peers within 35.0m:** `[0x02][32-byte raw Ed25519 sender pubkey][16kHz mono s16le PCM payload...]`

---

## 6. Heartbeats, Signals & Limits

- **Ping/Pong:** Relay sends WebSocket `Ping` every `25s` and disconnects peers silent for `60s`.
- **Live Config Reload:** Sending `SIGUSR1` (`kill -USR1 <pid>`) reloads `world.toml`, `bans.txt`, and `blocked_items.txt` without disconnecting active sessions.
- **Max Frame Size:** `512 KB` text/binary read limit.
- **Rate Limits:** `STATE` capped at `~80 Hz`; `CHAT` capped at `4 msg/sec` (`<= 512` bytes); max `16` concurrent WebSocket connections per IP (honors `X-Real-IP` / `X-Forwarded-For` when `-trust-proxy` is enabled).
