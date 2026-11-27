package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gorilla/websocket"
	"vektorweb/internal/identity"
)

const (
	ProtocolVersion    = 1
	MaxWireFrameBytes  = 512 * 1024
	MaxVoiceFrameBytes = 32 * 1024
	MaxAssetChunkChars = 128 * 1024
	MaxCachedPropBytes     = 10 * 1024 * 1024
	MaxTotalPropCacheBytes = 256 * 1024 * 1024
	MaxPropsPerClient      = 32
	MaxTotalProps      = 512
	PingInterval       = 25 * time.Second
	PongWait           = 60 * time.Second
	WriteWait          = 5 * time.Second
	OutboundQueueSize  = 128
)

type WireMsg struct {
	Type         string    `json:"type"`
	ProtoVersion int       `json:"proto_version,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	Nonce        string    `json:"nonce,omitempty"`
	Timestamp    int64     `json:"timestamp,omitempty"`
	PubKey       string    `json:"pubkey,omitempty"`
	TargetPub    string    `json:"target_pub,omitempty"`
	Signature    string    `json:"signature,omitempty"`
	AvatarHash   string    `json:"avatar_hash,omitempty"`
	Nick         string    `json:"nick,omitempty"`
	Pos          []float32 `json:"pos,omitempty"`
	Yaw          float32   `json:"yaw,omitempty"`
	Pitch        float32   `json:"pitch,omitempty"`
	Roll         float32   `json:"roll,omitempty"`
	Hash         string    `json:"hash,omitempty"`
	Seq          int       `json:"seq,omitempty"`
	Total        int       `json:"total,omitempty"`
	Data         string    `json:"data,omitempty"`
	Text         string    `json:"text,omitempty"`
	Channel      string    `json:"channel,omitempty"`
	PropID       string    `json:"prop_id,omitempty"`
	Scale        float32   `json:"scale,omitempty"`
	Code         string    `json:"code,omitempty"`
	Message      string    `json:"message,omitempty"`
}

type WorldBakedProp struct {
	ID    string    `toml:"id"`
	Hash  string    `toml:"hash"`
	Owner string    `toml:"owner"`
	Pos   []float32 `toml:"pos"`
	Yaw   float32   `toml:"yaw"`
	Pitch float32   `toml:"pitch"`
	Roll  float32   `toml:"roll"`
	Scale float32   `toml:"scale"`
}

type WorldConfigTOML struct {
	World struct {
		Title string    `toml:"title"`
		MOTD  string    `toml:"motd"`
		Spawn []float32 `toml:"spawn"`
	} `toml:"world"`
	Rules struct {
		PlacementMode string   `toml:"placement_mode"`
		Admins        []string `toml:"admins"`
	} `toml:"rules"`
	Props []WorldBakedProp `toml:"props"`
}

type StatusResponse struct {
	ProtoVersion  int    `json:"proto_version"`
	Title         string `json:"title"`
	Domain        string `json:"domain"`
	WorldURL      string `json:"world_url,omitempty"`
	PlacementMode string `json:"placement_mode"`
	Online        int    `json:"online"`
	Props         int    `json:"props"`
	UptimeSec     int64  `json:"uptime_sec"`
	LastSeenUnix  int64  `json:"last_seen_unix,omitempty"`
}

type DirectoryResponse struct {
	Worlds []StatusResponse `json:"worlds"`
}

type outboundFrame struct {
	msgType int
	data    []byte
	dropOK  bool
}

type ClientConn struct {
	conn         *websocket.Conn
	send         chan outboundFrame
	closeOnce    sync.Once
	pubKey       string
	nick         string
	avatarHash   string
	capabilities []string
	pos          [3]float32
	yaw          float32
	hasState     bool
	lastStateAt  time.Time
	lastChatAt   time.Time
}

func newClientConn(conn *websocket.Conn) *ClientConn {
	return &ClientConn{
		conn: conn,
		send: make(chan outboundFrame, OutboundQueueSize),
	}
}

func (c *ClientConn) close() {
	c.closeOnce.Do(func() {
		close(c.send)
		_ = c.conn.Close()
	})
}

func (c *ClientConn) enqueueJSON(v any, dropOK bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.enqueueRaw(websocket.TextMessage, b, dropOK)
}

func (c *ClientConn) enqueueRaw(msgType int, payload []byte, dropOK bool) {
	defer func() { _ = recover() }()
	frame := outboundFrame{msgType: msgType, data: payload, dropOK: dropOK}
	if dropOK {
		select {
		case c.send <- frame:
		default:
		}
		return
	}
	select {
	case c.send <- frame:
	case <-time.After(250 * time.Millisecond):
		go c.close()
	}
}

func (c *ClientConn) writePump() {
	ticker := time.NewTicker(PingInterval)
	defer func() {
		ticker.Stop()
		c.close()
	}()

	for {
		select {
		case frame, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(WriteWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
				return
			}
			if err := c.conn.WriteMessage(frame.msgType, frame.data); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(WriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"vektorweb.v1"},
	CheckOrigin:  func(r *http.Request) bool { return true },
}

type RelayServer struct {
	domain        string
	worldDir      string
	serveHTTP     bool
	placementMode string
	trustProxy    bool
	announceURL   string
	worldURL      string
	startedAt     time.Time

	mu              sync.RWMutex
	worldTitle      string
	worldMOTD       string
	worldSpawn      []float32
	adminKeys       map[string]bool
	cliAdmins       map[string]bool
	bannedKeys      map[string]bool
	blockedItems    map[string]bool
	bansModTime     time.Time
	blockedModTime  time.Time
	tomlModTime     time.Time
	clients         map[string]*ClientConn
	props           map[string]WireMsg
	propAssemblies  map[string]*RelayAssetAssembler
	ipConns         map[string]int
	announcedWorlds map[string]StatusResponse
}

type RelayAssetAssembler struct {
	FromPub   string
	Total     int
	Chunks    map[int][]byte
	CreatedAt time.Time
}

func main() {
	addr := flag.String("addr", "0.0.0.0:7777", "Listen address (host:port)")
	domain := flag.String("domain", "", "Public domain or IP bound to Ed25519 challenges (auto-detected from -addr if empty)")
	worldDir := flag.String("dir", "./world", "World folder to auto-create and serve")
	serveHTTP := flag.Bool("http", true, "Serve world files over HTTP on the same port (disable if using Apache/Nginx)")
	placement := flag.String("placement", "ephemeral", "Prop placement mode: forbid | ephemeral | admin_only | persistent")
	trustProxy := flag.Bool("trust-proxy", false, "Trust X-Real-IP / X-Forwarded-For headers from a local reverse proxy (Nginx/Caddy)")
	announceFlag := flag.String("announce", "", "Optional public directory HTTP(S) URL to POST status pings every 60s")
	adminFlag := flag.String("admin", "", "Comma-separated 64-char hex Ed25519 admin public keys (in addition to world.toml rules.admins)")
	flag.Parse()

	switch *placement {
	case "forbid", "ephemeral", "admin_only", "persistent":
	default:
		log.Fatalf("invalid -placement %q (expected forbid, ephemeral, admin_only, or persistent)", *placement)
	}

	resolvedDomain := strings.TrimSpace(*domain)
	if resolvedDomain == "" {
		host := *addr
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}
		if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
			resolvedDomain = "127.0.0.1"
		} else {
			resolvedDomain = host
		}
	}

	_ = mime.AddExtensionType(".glb", "model/gltf-binary")
	_ = mime.AddExtensionType(".toml", "application/toml")
	_ = mime.AddExtensionType(".lua", "text/x-lua")

	if err := ensureWorldDir(*worldDir, *addr, resolvedDomain); err != nil {
		log.Fatalf("failed initializing world folder %s: %v", *worldDir, err)
	}

	cliAdmins := make(map[string]bool)
	for _, k := range strings.Split(*adminFlag, ",") {
		k = strings.ToLower(strings.TrimSpace(k))
		if isValidHex64(k) {
			cliAdmins[k] = true
		}
	}

	port := "7777"
	if idx := strings.LastIndex(*addr, ":"); idx != -1 {
		port = (*addr)[idx+1:]
	}
	joinURL := fmt.Sprintf("http://%s:%s/world.toml", resolvedDomain, port)
	if port == "443" {
		joinURL = fmt.Sprintf("https://%s/world.toml", resolvedDomain)
	} else if port == "80" {
		joinURL = fmt.Sprintf("http://%s/world.toml", resolvedDomain)
	}

	announceTarget := strings.TrimSpace(*announceFlag)
	if announceTarget != "" && !strings.HasPrefix(announceTarget, "http://") && !strings.HasPrefix(announceTarget, "https://") {
		log.Fatalf("invalid -announce URL %q (must start with http:// or https://)", announceTarget)
	}

	srv := &RelayServer{
		domain:        resolvedDomain,
		worldDir:      *worldDir,
		serveHTTP:     *serveHTTP,
		placementMode: *placement,
		trustProxy:    *trustProxy,
		announceURL:   announceTarget,
		worldURL:      joinURL,
		startedAt:     time.Now(),
		worldTitle:    "VektorWeb World",
		worldMOTD:     "",
		worldSpawn:    []float32{0.0, 2.0, 10.0},
		adminKeys:     make(map[string]bool),
		cliAdmins:     cliAdmins,
		bannedKeys:    make(map[string]bool),
		blockedItems:  make(map[string]bool),
		clients:         make(map[string]*ClientConn),
		props:           make(map[string]WireMsg),
		propAssemblies:  make(map[string]*RelayAssetAssembler),
		ipConns:         make(map[string]int),
		announcedWorlds: make(map[string]StatusResponse),
	}

	srv.reloadConfigFiles(true)

	if *placement == "persistent" || *placement == "admin_only" {
		srv.loadPersistentProps()
	}

	go srv.watchLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleRoot)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	displayHost := resolvedDomain

	log.Printf("vektorweb-relay v%d listening on %s (domain=%s, placement=%s, trust_proxy=%v)",
		ProtocolVersion, *addr, resolvedDomain, *placement, *trustProxy)
	log.Printf("World folder : %s", *worldDir)
	if *serveHTTP {
		log.Printf("Join URL     : %s", joinURL)
		log.Printf("Status URL   : http://%s:%s/status.json", displayHost, port)
	}
	if announceTarget != "" {
		log.Printf("Announcing   : %s (every 60s)", announceTarget)
		go srv.announceLoop()
	}

	stop := make(chan os.Signal, 1)
	reloadSig := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	signal.Notify(reloadSig, syscall.SIGUSR1)

	go func() {
		for range reloadSig {
			log.Printf("Received SIGUSR1 — reloading bans.txt, blocked_items.txt, and world.toml")
			srv.reloadConfigFiles(true)
		}
	}()

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Printf("Shutting down vektorweb-relay...")

	// Hard watchdog so open WebSockets or keep-alive HTTP requests can never keep the process alive
	go func() {
		time.Sleep(2 * time.Second)
		log.Printf("Force-exiting vektorweb-relay.")
		os.Exit(0)
	}()

	srv.shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	_ = httpSrv.Close()
	log.Printf("Shutdown complete.")
	os.Exit(0)
}

func (s *RelayServer) watchLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.reloadConfigFiles(false)
		s.pruneStaleAssemblies()
	}
}

func fileModTime(path string) time.Time {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func (s *RelayServer) reloadConfigFiles(force bool) {
	bansPath := filepath.Join(s.worldDir, "bans.txt")
	blockedPath := filepath.Join(s.worldDir, "blocked_items.txt")
	tomlPath := filepath.Join(s.worldDir, "world.toml")

	bMod := fileModTime(bansPath)
	iMod := fileModTime(blockedPath)
	tMod := fileModTime(tomlPath)

	s.mu.Lock()
	var toKick []*ClientConn

	if force || !bMod.Equal(s.bansModTime) {
		s.bansModTime = bMod
		s.bannedKeys = loadFlatSet(bansPath)
		for pub, cc := range s.clients {
			if s.bannedKeys[strings.ToLower(pub)] || s.bannedKeys[pub] {
				toKick = append(toKick, cc)
			}
		}
	}

	if force || !iMod.Equal(s.blockedModTime) {
		s.blockedModTime = iMod
		s.blockedItems = loadFlatSet(blockedPath)
	}

	if force || !tMod.Equal(s.tomlModTime) {
		s.tomlModTime = tMod
		var wc WorldConfigTOML
		if raw, err := os.ReadFile(tomlPath); err == nil {
			if _, err := toml.Decode(string(raw), &wc); err == nil {
				if strings.TrimSpace(wc.World.Title) != "" {
					s.worldTitle = strings.TrimSpace(wc.World.Title)
				}
				s.worldMOTD = sanitizeControlChars(wc.World.MOTD, 240)
				if finiteVec3(wc.World.Spawn) {
					s.worldSpawn = append([]float32(nil), wc.World.Spawn...)
				}
				admins := make(map[string]bool, len(s.cliAdmins)+len(wc.Rules.Admins))
				for k := range s.cliAdmins {
					admins[k] = true
				}
				for _, k := range wc.Rules.Admins {
					k = strings.ToLower(strings.TrimSpace(k))
					if isValidHex64(k) {
						admins[k] = true
					}
				}
				s.adminKeys = admins
				pm := strings.ToLower(strings.TrimSpace(wc.Rules.PlacementMode))
				if pm == "forbid" || pm == "ephemeral" || pm == "admin_only" || pm == "persistent" {
					s.placementMode = pm
				}

				// Sync baked [[props]] defined in world.toml (prefixed "baked_")
				activeBaked := make(map[string]bool)
				for idx, bp := range wc.Props {
					hLower := strings.ToLower(strings.TrimSpace(bp.Hash))
					if !isValidHex64(hLower) || !finiteVec3(bp.Pos) {
						continue
					}
					if s.blockedItems[hLower] || s.blockedItems[bp.Hash] {
						continue
					}
					propID := sanitizeControlChars(bp.ID, 48)
					if propID == "" {
						propID = fmt.Sprintf("baked_%d_%s", idx, hLower[:8])
					} else if !strings.HasPrefix(propID, "baked_") {
						propID = "baked_" + propID
					}
					yaw := sanitizeFiniteFloat(bp.Yaw)
					pitch := sanitizeFiniteFloat(bp.Pitch)
					roll := sanitizeFiniteFloat(bp.Roll)
					scale := bp.Scale
					if math.IsNaN(float64(scale)) || math.IsInf(float64(scale), 0) || scale <= 0 {
						scale = 1.0
					} else if scale > 25.0 {
						scale = 25.0
					}
					owner := strings.ToLower(strings.TrimSpace(bp.Owner))
					if !isValidHex64(owner) {
						owner = "0000000000000000000000000000000000000000000000000000000000000000"
					}
					activeBaked[propID] = true
					s.props[propID] = WireMsg{
						Type:   "PROP_SPAWN",
						PropID: propID,
						PubKey: owner,
						Hash:   hLower,
						Pos:    []float32{bp.Pos[0], bp.Pos[1], bp.Pos[2]},
						Yaw:    yaw,
						Pitch:  pitch,
						Roll:   roll,
						Scale:  scale,
					}
				}
				for id := range s.props {
					if strings.HasPrefix(id, "baked_") && !activeBaked[id] {
						delete(s.props, id)
					}
				}
			}
		}
	}
	s.mu.Unlock()

	for _, cc := range toKick {
		s.sendError(cc, "ERR_BANNED", "public key was banned on this relay")
		go func(target *ClientConn) {
			time.Sleep(50 * time.Millisecond)
			target.close()
		}(cc)
	}
}

func (s *RelayServer) shutdown() {
	s.mu.Lock()
	s.savePersistentPropsLocked()
	peers := make([]*ClientConn, 0, len(s.clients))
	for _, c := range s.clients {
		peers = append(peers, c)
	}
	s.clients = make(map[string]*ClientConn)
	s.mu.Unlock()

	for _, p := range peers {
		p.close()
	}
}

func ensureWorldDir(dir, addr, domain string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Join(dir, "prop_cache"), 0755)

	port := "7777"
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		port = addr[idx+1:]
	}

	created := false

	envPath := filepath.Join(dir, "environment.glb")
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		if err := os.WriteFile(envPath, buildPlazaGLB(32.0, 0.25, 32.0), 0644); err != nil {
			return err
		}
		created = true
	}

	luaPath := filepath.Join(dir, "world.lua")
	if _, err := os.Stat(luaPath); os.IsNotExist(err) {
		starterLua := `-- Sandboxed world.lua (zero os/io/network access, 15ms/frame CPU cap)
local greeted = false

function on_tick(dt)
    local x, y, z = world.player_pos()
    world.set_hud(string.format("VEKTORWEB PLAZA // POS: %.1f, %.1f, %.1f", x, y, z))

    if not greeted then
        greeted = true
        world.notify("Welcome! Edit world.toml, environment.glb, or world.lua in your world folder.")
    end
end
`
		if err := os.WriteFile(luaPath, []byte(starterLua), 0644); err != nil {
			return err
		}
		created = true
	}

	tomlPath := filepath.Join(dir, "world.toml")
	if _, err := os.Stat(tomlPath); os.IsNotExist(err) {
		starterToml := fmt.Sprintf(`[world]
title = "Starter VektorWeb Plaza"
motd = "Welcome to VektorWeb! Press [F1] for controls or type /help."
author_pubkey = ""
relay_url = "ws://%s:%s/"
spawn = [0.0, 2.0, 10.0]

[assets]
environment = "environment.glb"
environment_sha256 = ""
script = "world.lua"
script_sha256 = ""

[rules]
placement_mode = "ephemeral"
max_avatar_mb = 15
max_item_mb = 5
admins = []
`, domain, port)
		if err := os.WriteFile(tomlPath, []byte(starterToml), 0644); err != nil {
			return err
		}
		created = true
	}

	bansPath := filepath.Join(dir, "bans.txt")
	if _, err := os.Stat(bansPath); os.IsNotExist(err) {
		_ = os.WriteFile(bansPath, []byte(`# Add banned Ed25519 public keys (one 64-char hex key per line)
`), 0644)
	}

	blockedPath := filepath.Join(dir, "blocked_items.txt")
	if _, err := os.Stat(blockedPath); os.IsNotExist(err) {
		_ = os.WriteFile(blockedPath, []byte(`# Add blocked .glb SHA-256 hashes (one 64-char hex hash per line)
`), 0644)
	}

	if created {
		log.Printf("Initialized starter world in %s (edit world.toml, environment.glb, or world.lua anytime)", dir)
	}
	return nil
}

func buildPlazaGLB(w, h, d float32) []byte {
	var pos []float32
	var norm []float32
	var col []float32
	var idx []uint16

	minV := [3]float32{1e9, 1e9, 1e9}
	maxV := [3]float32{-1e9, -1e9, -1e9}

	addQuad := func(p0, p1, p2, p3, n [3]float32, c [4]float32) {
		base := uint16(len(pos) / 3)
		pts := [4][3]float32{p0, p1, p2, p3}
		for _, p := range pts {
			pos = append(pos, p[0], p[1], p[2])
			norm = append(norm, n[0], n[1], n[2])
			col = append(col, c[0], c[1], c[2], c[3])
			for k := 0; k < 3; k++ {
				if p[k] < minV[k] {
					minV[k] = p[k]
				}
				if p[k] > maxV[k] {
					maxV[k] = p[k]
				}
			}
		}
		idx = append(idx, base, base+1, base+2, base, base+2, base+3)
	}

	addBox := func(cx, cy, cz, sx, sy, sz float32, cTop, cSide [4]float32) {
		hx, hy, hz := sx/2, sy/2, sz/2
		x0, x1 := cx-hx, cx+hx
		y0, y1 := cy-hy, cy+hy
		z0, z1 := cz-hz, cz+hz
		// Top (+Y) & Bottom (-Y)
		addQuad([3]float32{x0, y1, z1}, [3]float32{x1, y1, z1}, [3]float32{x1, y1, z0}, [3]float32{x0, y1, z0}, [3]float32{0, 1, 0}, cTop)
		addQuad([3]float32{x0, y0, z0}, [3]float32{x1, y0, z0}, [3]float32{x1, y0, z1}, [3]float32{x0, y0, z1}, [3]float32{0, -1, 0}, cSide)
		// +Z & -Z
		addQuad([3]float32{x0, y0, z1}, [3]float32{x1, y0, z1}, [3]float32{x1, y1, z1}, [3]float32{x0, y1, z1}, [3]float32{0, 0, 1}, cSide)
		addQuad([3]float32{x1, y0, z0}, [3]float32{x0, y0, z0}, [3]float32{x0, y1, z0}, [3]float32{x1, y1, z0}, [3]float32{0, 0, -1}, cSide)
		// +X & -X
		addQuad([3]float32{x1, y0, z1}, [3]float32{x1, y0, z0}, [3]float32{x1, y1, z0}, [3]float32{x1, y1, z1}, [3]float32{1, 0, 0}, cSide)
		addQuad([3]float32{x0, y0, z0}, [3]float32{x0, y0, z1}, [3]float32{x0, y1, z1}, [3]float32{x0, y1, z0}, [3]float32{-1, 0, 0}, cSide)
	}

	// Dark slate plaza deck + cyan perimeter trim
	deckTop := [4]float32{0.10, 0.13, 0.19, 1.0}
	deckSide := [4]float32{0.06, 0.08, 0.12, 1.0}
	cyanGlow := [4]float32{0.15, 0.85, 0.95, 1.0}
	goldTrim := [4]float32{0.95, 0.72, 0.20, 1.0}
	pillarBody := [4]float32{0.16, 0.20, 0.28, 1.0}

	addBox(0, -h/2, 0, w, h, d, deckTop, deckSide)

	// Glowing perimeter trim strips
	hw, hd := w/2, d/2
	addBox(0, 0.03, -hd+0.2, w, 0.06, 0.35, cyanGlow, cyanGlow)
	addBox(0, 0.03, hd-0.2, w, 0.06, 0.35, cyanGlow, cyanGlow)
	addBox(-hw+0.2, 0.03, 0, 0.35, 0.06, d, cyanGlow, cyanGlow)
	addBox(hw-0.2, 0.03, 0, 0.35, 0.06, d, cyanGlow, cyanGlow)

	// Walkable stepped central dais (step height 0.22m <= 0.55m max climb)
	addBox(0, 0.11, 0, 7.5, 0.22, 7.5, [4]float32{0.14, 0.18, 0.26, 1.0}, cyanGlow)
	addBox(0, 0.33, 0, 4.2, 0.22, 4.2, [4]float32{0.18, 0.23, 0.33, 1.0}, goldTrim)

	// Four corner obelisk pillars with cyan beacon caps
	offX, offZ := hw-2.2, hd-2.2
	corners := [4][2]float32{{-offX, -offZ}, {offX, -offZ}, {-offX, offZ}, {offX, offZ}}
	for _, c := range corners {
		addBox(c[0], 2.4, c[1], 1.1, 4.8, 1.1, pillarBody, pillarBody)
		addBox(c[0], 5.05, c[1], 1.25, 0.45, 1.25, cyanGlow, cyanGlow)
	}

	vCount := len(pos) / 3
	pBytes := make([]byte, len(pos)*4)
	for i, v := range pos {
		binary.LittleEndian.PutUint32(pBytes[i*4:], math.Float32bits(v))
	}
	nBytes := make([]byte, len(norm)*4)
	for i, v := range norm {
		binary.LittleEndian.PutUint32(nBytes[i*4:], math.Float32bits(v))
	}
	cBytes := make([]byte, len(col)*4)
	for i, v := range col {
		binary.LittleEndian.PutUint32(cBytes[i*4:], math.Float32bits(v))
	}
	iBytes := make([]byte, len(idx)*2)
	for i, id := range idx {
		binary.LittleEndian.PutUint16(iBytes[i*2:], id)
	}
	for len(iBytes)%4 != 0 {
		iBytes = append(iBytes, 0, 0)
	}

	var binBuf []byte
	pOff := 0
	binBuf = append(binBuf, pBytes...)
	nOff := len(binBuf)
	binBuf = append(binBuf, nBytes...)
	cOff := len(binBuf)
	binBuf = append(binBuf, cBytes...)
	iOff := len(binBuf)
	binBuf = append(binBuf, iBytes...)

	gltf := map[string]any{
		"asset":  map[string]string{"version": "2.0"},
		"scene":  0,
		"scenes": []any{map[string]any{"nodes": []int{0}}},
		"nodes":  []any{map[string]int{"mesh": 0}},
		"meshes": []any{map[string]any{"primitives": []any{map[string]any{
			"attributes": map[string]int{"POSITION": 0, "NORMAL": 1, "COLOR_0": 2},
			"indices":    3,
		}}}},
		"buffers": []any{map[string]int{"byteLength": len(binBuf)}},
		"bufferViews": []any{
			map[string]int{"buffer": 0, "byteOffset": pOff, "byteLength": len(pBytes), "target": 34962},
			map[string]int{"buffer": 0, "byteOffset": nOff, "byteLength": len(nBytes), "target": 34962},
			map[string]int{"buffer": 0, "byteOffset": cOff, "byteLength": len(cBytes), "target": 34962},
			map[string]int{"buffer": 0, "byteOffset": iOff, "byteLength": len(idx) * 2, "target": 34963},
		},
		"accessors": []any{
			map[string]any{"bufferView": 0, "componentType": 5126, "count": vCount, "type": "VEC3", "min": []float32{minV[0], minV[1], minV[2]}, "max": []float32{maxV[0], maxV[1], maxV[2]}},
			map[string]any{"bufferView": 1, "componentType": 5126, "count": vCount, "type": "VEC3"},
			map[string]any{"bufferView": 2, "componentType": 5126, "count": vCount, "type": "VEC4"},
			map[string]any{"bufferView": 3, "componentType": 5123, "count": len(idx), "type": "SCALAR"},
		},
	}
	jBytes, _ := json.Marshal(gltf)
	for len(jBytes)%4 != 0 {
		jBytes = append(jBytes, ' ')
	}
	totalLen := uint32(12 + 8 + len(jBytes) + 8 + len(binBuf))
	out := make([]byte, totalLen)
	binary.LittleEndian.PutUint32(out[0:4], 0x46546C67)
	binary.LittleEndian.PutUint32(out[4:8], 2)
	binary.LittleEndian.PutUint32(out[8:12], totalLen)
	binary.LittleEndian.PutUint32(out[12:16], uint32(len(jBytes)))
	binary.LittleEndian.PutUint32(out[16:20], 0x4E4F534A)
	copy(out[20:20+len(jBytes)], jBytes)
	bOff := 20 + len(jBytes)
	binary.LittleEndian.PutUint32(out[bOff:bOff+4], uint32(len(binBuf)))
	binary.LittleEndian.PutUint32(out[bOff+4:bOff+8], 0x004E4942)
	copy(out[bOff+8:], binBuf)
	return out
}

func (s *RelayServer) announceLoop() {
	httpClient := &http.Client{Timeout: 8 * time.Second}
	s.postStatusPing(httpClient)
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.postStatusPing(httpClient)
	}
}

func (s *RelayServer) postStatusPing(client *http.Client) {
	if s.announceURL == "" {
		return
	}
	s.mu.RLock()
	payload := StatusResponse{
		ProtoVersion:  ProtocolVersion,
		Title:         s.worldTitle,
		Domain:        s.domain,
		WorldURL:      s.worldURL,
		PlacementMode: s.placementMode,
		Online:        len(s.clients),
		Props:         len(s.props),
		UptimeSec:     int64(time.Since(s.startedAt).Seconds()),
	}
	s.mu.RUnlock()

	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, s.announceURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("vektorweb-relay/%d", ProtocolVersion))

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Directory announce failed: %v", err)
		return
	}
	_ = resp.Body.Close()
}

func (s *RelayServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	resp := StatusResponse{
		ProtoVersion:  ProtocolVersion,
		Title:         s.worldTitle,
		Domain:        s.domain,
		WorldURL:      s.worldURL,
		PlacementMode: s.placementMode,
		Online:        len(s.clients),
		Props:         len(s.props),
		UptimeSec:     int64(time.Since(s.startedAt).Seconds()),
	}
	s.mu.RUnlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *RelayServer) handleDirectory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method == http.MethodPost {
		var ping StatusResponse
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&ping); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		wURL := strings.TrimSpace(ping.WorldURL)
		if !strings.HasPrefix(wURL, "http://") && !strings.HasPrefix(wURL, "https://") {
			http.Error(w, "world_url must be http(s)", http.StatusBadRequest)
			return
		}
		ping.Title = sanitizeControlChars(ping.Title, 64)
		if ping.Title == "" {
			ping.Title = "Untitled World"
		}
		ping.Domain = sanitizeControlChars(ping.Domain, 64)
		ping.PlacementMode = sanitizeControlChars(ping.PlacementMode, 24)
		if ping.Online < 0 {
			ping.Online = 0
		}
		if ping.Props < 0 {
			ping.Props = 0
		}
		ping.LastSeenUnix = time.Now().Unix()

		s.mu.Lock()
		if len(s.announcedWorlds) < 256 || s.announcedWorlds[wURL].WorldURL != "" {
			s.announcedWorlds[wURL] = ping
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	now := time.Now().Unix()
	s.mu.Lock()
	selfEntry := StatusResponse{
		ProtoVersion:  ProtocolVersion,
		Title:         s.worldTitle,
		Domain:        s.domain,
		WorldURL:      s.worldURL,
		PlacementMode: s.placementMode,
		Online:        len(s.clients),
		Props:         len(s.props),
		UptimeSec:     int64(time.Since(s.startedAt).Seconds()),
		LastSeenUnix:  now,
	}
	list := []StatusResponse{selfEntry}
	for k, entry := range s.announcedWorlds {
		if now-entry.LastSeenUnix > 180 {
			delete(s.announcedWorlds, k)
			continue
		}
		if strings.EqualFold(strings.TrimSpace(entry.WorldURL), strings.TrimSpace(s.worldURL)) {
			continue
		}
		list = append(list, entry)
	}
	s.mu.Unlock()

	_ = json.NewEncoder(w).Encode(DirectoryResponse{Worlds: list})
}

func (s *RelayServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		s.handleWS(w, r)
		return
	}
	if r.URL.Path == "/status.json" {
		s.handleStatus(w, r)
		return
	}
	if r.URL.Path == "/directory.json" {
		s.handleDirectory(w, r)
		return
	}
	if !s.serveHTTP {
		http.Error(w, "vektorweb-relay WebSocket endpoint", http.StatusUpgradeRequired)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cleanPath := filepath.Clean("/" + r.URL.Path)
	base := filepath.Base(cleanPath)
	if strings.HasPrefix(cleanPath, "/prop_cache") || base == "bans.txt" || base == "blocked_items.txt" || base == "world_props.json" || strings.HasPrefix(base, ".") {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	if cleanPath == "/" {
		cleanPath = "/world.toml"
	}
	target := filepath.Join(s.worldDir, filepath.FromSlash(cleanPath))
	http.ServeFile(w, r, target)
}

func (s *RelayServer) propCachePath(hash string) string {
	return filepath.Join(s.worldDir, "prop_cache", strings.ToLower(hash)+".glb")
}

func (s *RelayServer) resolveCachedPropFile(hash string) (string, bool) {
	if !isValidHex64(hash) {
		return "", false
	}
	hLower := strings.ToLower(hash)
	for _, p := range []string{
		s.propCachePath(hLower),
		filepath.Join(s.worldDir, hLower+".glb"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() >= 20 && st.Size() <= MaxCachedPropBytes {
			return p, true
		}
	}
	return "", false
}

func (s *RelayServer) hasCachedProp(hash string) bool {
	_, ok := s.resolveCachedPropFile(hash)
	return ok
}

func (s *RelayServer) propCacheDiskUsage() int64 {
	entries, err := os.ReadDir(filepath.Join(s.worldDir, "prop_cache"))
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".glb") {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	return total
}

func (s *RelayServer) pruneStaleAssemblies() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, asm := range s.propAssemblies {
		if time.Since(asm.CreatedAt) > 30*time.Second {
			delete(s.propAssemblies, h)
		}
	}
}

func (s *RelayServer) gcUnusedCachedPropsLocked() {
	cacheDir := filepath.Join(s.worldDir, "prop_cache")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	active := make(map[string]bool, len(s.props))
	for _, pr := range s.props {
		active[strings.ToLower(pr.Hash)] = true
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".glb") {
			continue
		}
		h := strings.TrimSuffix(e.Name(), ".glb")
		if !active[h] {
			_ = os.Remove(filepath.Join(cacheDir, e.Name()))
			_ = os.Remove(filepath.Join(cacheDir, e.Name()+".owner"))
		}
	}
}

func (s *RelayServer) requestPropCacheFromClient(cc *ClientConn, pubKey, hash string) {
	if s.placementMode != "persistent" && s.placementMode != "admin_only" {
		return
	}
	hLower := strings.ToLower(hash)
	if !isValidHex64(hLower) || s.hasCachedProp(hLower) {
		return
	}
	s.mu.Lock()
	if _, pending := s.propAssemblies[hLower]; pending {
		s.mu.Unlock()
		return
	}
	s.propAssemblies[hLower] = &RelayAssetAssembler{
		FromPub:   pubKey,
		Chunks:    make(map[int][]byte),
		CreatedAt: time.Now(),
	}
	s.mu.Unlock()

	cc.enqueueJSON(WireMsg{
		Type:      "REQ_ASSET",
		TargetPub: pubKey,
		Hash:      hLower,
	}, false)
}

func (s *RelayServer) handleRelayCacheChunk(senderPub string, msg WireMsg) bool {
	hLower := strings.ToLower(msg.Hash)
	if !isValidHex64(hLower) || msg.Total <= 0 || msg.Total > 512 || msg.Seq < 0 || msg.Seq >= msg.Total {
		return false
	}
	s.mu.Lock()
	asm, ok := s.propAssemblies[hLower]
	if !ok || !strings.EqualFold(asm.FromPub, senderPub) {
		s.mu.Unlock()
		return false
	}
	if asm.Total == 0 {
		asm.Total = msg.Total
	} else if asm.Total != msg.Total {
		delete(s.propAssemblies, hLower)
		s.mu.Unlock()
		return true
	}
	raw, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		s.mu.Unlock()
		return true
	}
	asm.Chunks[msg.Seq] = raw
	if len(asm.Chunks) < asm.Total {
		s.mu.Unlock()
		return true
	}
	var full []byte
	for i := 0; i < asm.Total; i++ {
		chunk, exists := asm.Chunks[i]
		if !exists {
			delete(s.propAssemblies, hLower)
			s.mu.Unlock()
			return true
		}
		full = append(full, chunk...)
		if len(full) > MaxCachedPropBytes {
			delete(s.propAssemblies, hLower)
			s.mu.Unlock()
			log.Printf("Rejected oversized prop cache %s (> %d bytes)", hLower[:12], MaxCachedPropBytes)
			return true
		}
	}
	delete(s.propAssemblies, hLower)
	s.mu.Unlock()

	// Verify SHA-256 integrity and binary glTF 2.0 header magic ("glTF")
	if len(full) < 20 || binary.LittleEndian.Uint32(full[0:4]) != 0x46546C67 {
		log.Printf("Rejected invalid GLB magic for prop cache %s", hLower[:12])
		return true
	}
	if !strings.EqualFold(identity.FileSHA256(full), hLower) {
		log.Printf("Rejected SHA-256 mismatch for prop cache %s", hLower[:12])
		return true
	}
	if s.propCacheDiskUsage()+int64(len(full)) > MaxTotalPropCacheBytes {
		log.Printf("Skipped caching prop %s: prop_cache 256MB limit reached", hLower[:12])
		return true
	}
	_ = os.MkdirAll(filepath.Join(s.worldDir, "prop_cache"), 0755)
	tmpPath := s.propCachePath(hLower) + ".tmp"
	if err := os.WriteFile(tmpPath, full, 0644); err == nil {
		if os.Rename(tmpPath, s.propCachePath(hLower)) == nil {
			_ = os.WriteFile(s.propCachePath(hLower)+".owner", []byte(strings.ToLower(senderPub)+"\n"), 0644)
			log.Printf("Cached prop %s.glb (%d bytes) in prop_cache/", hLower[:12], len(full))
		}
	}
	return true
}

func (s *RelayServer) streamCachedPropToPeer(target *ClientConn, hash string) bool {
	hLower := strings.ToLower(hash)
	p, ok := s.resolveCachedPropFile(hLower)
	if !ok {
		return false
	}
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 || len(data) > MaxCachedPropBytes {
		return false
	}
	go func(cc *ClientConn, blob []byte) {
		const chunkSize = 48 * 1024
		total := (len(blob) + chunkSize - 1) / chunkSize
		for i := 0; i < total; i++ {
			start := i * chunkSize
			end := start + chunkSize
			if end > len(blob) {
				end = len(blob)
			}
			cc.enqueueJSON(WireMsg{
				Type:      "ASSET_CHUNK",
				TargetPub: cc.pubKey,
				Hash:      hLower,
				Seq:       i,
				Total:     total,
				Data:      base64.StdEncoding.EncodeToString(blob[start:end]),
			}, false)
			time.Sleep(10 * time.Millisecond)
		}
	}(target, data)
	return true
}

func (s *RelayServer) persistentPropsPath() string {
	return filepath.Join(s.worldDir, "world_props.json")
}

func (s *RelayServer) loadPersistentProps() {
	b, err := os.ReadFile(s.persistentPropsPath())
	if err != nil {
		return
	}
	var list []WireMsg
	if err := json.Unmarshal(b, &list); err == nil {
		for _, pr := range list {
			if pr.PropID != "" && !s.blockedItems[strings.ToLower(pr.Hash)] && !s.blockedItems[pr.Hash] {
				s.props[pr.PropID] = pr
			}
		}
		log.Printf("Restored %d persistent props", len(s.props))
	}
}

func (s *RelayServer) savePersistentPropsLocked() {
	if s.placementMode != "persistent" && s.placementMode != "admin_only" {
		return
	}
	list := make([]WireMsg, 0, len(s.props))
	for _, pr := range s.props {
		list = append(list, pr)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmpPath := s.persistentPropsPath() + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0644); err == nil {
		_ = os.Rename(tmpPath, s.persistentPropsPath())
	}
}

func (s *RelayServer) updateWorldSpawnLocked(pos []float32) error {
	if !finiteVec3(pos) {
		return errors.New("invalid spawn coordinates")
	}
	s.worldSpawn = []float32{pos[0], pos[1], pos[2]}
	tomlPath := filepath.Join(s.worldDir, "world.toml")
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		return err
	}
	spawnLine := fmt.Sprintf("spawn = [%.2f, %.2f, %.2f]", pos[0], pos[1], pos[2])
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	replaced := false
	inWorld := false
	worldEndIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inWorld && worldEndIdx == -1 {
				worldEndIdx = i
			}
			inWorld = (trimmed == "[world]")
			continue
		}
		if inWorld && strings.HasPrefix(trimmed, "spawn") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "spawn"))
			if strings.HasPrefix(rest, "=") {
				lines[i] = spawnLine
				replaced = true
				break
			}
		}
	}
	if !replaced {
		if worldEndIdx > 0 {
			newLines := make([]string, 0, len(lines)+1)
			newLines = append(newLines, lines[:worldEndIdx]...)
			newLines = append(newLines, spawnLine)
			newLines = append(newLines, lines[worldEndIdx:]...)
			lines = newLines
		} else if inWorld {
			lines = append(lines, spawnLine)
		} else {
			lines = append(lines, "", "[world]", spawnLine)
		}
	}
	tmpPath := tomlPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, tomlPath); err != nil {
		return err
	}
	s.tomlModTime = fileModTime(tomlPath)
	return nil
}

func (s *RelayServer) appendBanKeyLocked(pubKey string) {
	pubKey = strings.ToLower(strings.TrimSpace(pubKey))
	if !isValidHex64(pubKey) || s.bannedKeys[pubKey] {
		return
	}
	s.bannedKeys[pubKey] = true
	bansPath := filepath.Join(s.worldDir, "bans.txt")
	f, err := os.OpenFile(bansPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		_, _ = fmt.Fprintf(f, "%s # banned via admin command at %s\n", pubKey, time.Now().UTC().Format(time.RFC3339))
		_ = f.Close()
		s.bansModTime = fileModTime(bansPath)
	}
}

func (s *RelayServer) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.trustProxy {
		if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
			if ip := net.ParseIP(xri); ip != nil {
				return ip.String()
			}
		}
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
	}
	return host
}

func isValidHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func sanitizeFiniteFloat(v float32) float32 {
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return 0
	}
	return v
}

func finiteVec3(p []float32) bool {
	if len(p) != 3 {
		return false
	}
	for _, v := range p {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return false
		}
	}
	return true
}

func (s *RelayServer) sendError(cc *ClientConn, code, message string) {
	cc.enqueueJSON(WireMsg{
		Type:    "ERROR",
		Code:    code,
		Message: message,
	}, false)
}

func (s *RelayServer) handleWS(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	s.mu.Lock()
	if s.ipConns[ip] >= 16 {
		s.mu.Unlock()
		http.Error(w, "too many connections from IP", http.StatusTooManyRequests)
		return
	}
	s.ipConns[ip]++
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.ipConns[ip]--
		if s.ipConns[ip] <= 0 {
			delete(s.ipConns, ip)
		}
		s.mu.Unlock()
	}()

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	cc := newClientConn(conn)
	defer cc.close()

	conn.SetReadLimit(MaxWireFrameBytes)

	nonceBytes := make([]byte, 32)
	_, _ = rand.Read(nonceBytes)
	nonce := hex.EncodeToString(nonceBytes)
	ts := time.Now().Unix()

	_ = conn.SetWriteDeadline(time.Now().Add(WriteWait))
	if err := conn.WriteJSON(WireMsg{
		Type:         "CHALLENGE",
		ProtoVersion: ProtocolVersion,
		Nonce:        nonce,
		Timestamp:    ts,
	}); err != nil {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var auth WireMsg
	if err := conn.ReadJSON(&auth); err != nil || auth.Type != "AUTH" {
		return
	}

	if auth.ProtoVersion != 0 && auth.ProtoVersion != ProtocolVersion {
		_ = conn.SetWriteDeadline(time.Now().Add(WriteWait))
		_ = conn.WriteJSON(WireMsg{
			Type:         "ERROR",
			ProtoVersion: ProtocolVersion,
			Code:         "ERR_PROTO_MISMATCH",
			Message:      fmt.Sprintf("unsupported proto_version %d (relay supports v%d)", auth.ProtoVersion, ProtocolVersion),
		})
		return
	}

	if !identity.VerifyChallenge(auth.PubKey, nonce, ts, s.domain, auth.Signature) {
		log.Printf("Rejected invalid Ed25519 signature from %s", ip)
		_ = conn.SetWriteDeadline(time.Now().Add(WriteWait))
		_ = conn.WriteJSON(WireMsg{
			Type:    "ERROR",
			Code:    "ERR_AUTH_FAILED",
			Message: "invalid Ed25519 challenge signature",
		})
		return
	}

	pubLower := strings.ToLower(auth.PubKey)
	avLower := strings.ToLower(auth.AvatarHash)

	s.mu.RLock()
	banned := s.bannedKeys[pubLower] || s.bannedKeys[auth.PubKey]
	blockedAvatar := avLower != "" && avLower != "none" && (s.blockedItems[avLower] || s.blockedItems[auth.AvatarHash])
	isAdmin := s.adminKeys[pubLower]
	s.mu.RUnlock()

	if banned {
		log.Printf("Blocked banned pubkey %s", auth.PubKey[:12])
		_ = conn.SetWriteDeadline(time.Now().Add(WriteWait))
		_ = conn.WriteJSON(WireMsg{
			Type:    "ERROR",
			Code:    "ERR_BANNED",
			Message: "public key is banned on this relay",
		})
		return
	}
	if blockedAvatar {
		log.Printf("Rejected blocked avatar %s from %s", auth.AvatarHash[:12], auth.PubKey[:12])
		_ = conn.SetWriteDeadline(time.Now().Add(WriteWait))
		_ = conn.WriteJSON(WireMsg{
			Type:    "ERROR",
			Code:    "ERR_BLOCKED_AVATAR",
			Message: "avatar hash is blocked on this relay",
		})
		return
	}

	if auth.AvatarHash != "" && auth.AvatarHash != "none" && !isValidHex64(auth.AvatarHash) {
		auth.AvatarHash = "none"
	}
	cc.pubKey = auth.PubKey
	cc.avatarHash = auth.AvatarHash
	cc.nick = sanitizeNick(auth.Nick, auth.PubKey)
	cc.capabilities = sanitizeCaps(auth.Capabilities)

	go cc.writePump()

	_ = conn.SetReadDeadline(time.Now().Add(PongWait))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(PongWait))
		return nil
	})

	s.mu.Lock()
	if old, exists := s.clients[auth.PubKey]; exists {
		old.close()
	}
	s.clients[auth.PubKey] = cc
	total := len(s.clients)
	existingProps := make([]WireMsg, 0, len(s.props))
	for _, pr := range s.props {
		existingProps = append(existingProps, pr)
	}
	existingPeers := make([]WireMsg, 0, len(s.clients))
	for pub, peer := range s.clients {
		if pub == auth.PubKey || !peer.hasState {
			continue
		}
		existingPeers = append(existingPeers, WireMsg{
			Type:       "STATE",
			PubKey:     peer.pubKey,
			Nick:       peer.nick,
			AvatarHash: peer.avatarHash,
			Pos:        []float32{peer.pos[0], peer.pos[1], peer.pos[2]},
			Yaw:        peer.yaw,
		})
	}
	s.mu.Unlock()

	cc.enqueueJSON(WireMsg{
		Type:         "WELCOME",
		ProtoVersion: ProtocolVersion,
		PubKey:       auth.PubKey,
		Nick:         cc.nick,
		Channel:      s.placementMode,
	}, false)

	s.mu.RLock()
	curSpawn := append([]float32(nil), s.worldSpawn...)
	curMOTD := s.worldMOTD
	s.mu.RUnlock()
	if finiteVec3(curSpawn) {
		cc.enqueueJSON(WireMsg{
			Type: "SPAWN_UPDATE",
			Pos:  curSpawn,
		}, false)
	}
	if curMOTD != "" {
		cc.enqueueJSON(WireMsg{
			Type:      "CHAT",
			Nick:      "MOTD",
			Channel:   "whisper",
			Text:      curMOTD,
			Timestamp: time.Now().Unix(),
		}, false)
	}

	if isAdmin {
		cc.enqueueJSON(WireMsg{
			Type:      "CHAT",
			Nick:      "RELAY",
			Channel:   "whisper",
			Text:      "Admin authenticated (/setspawn [x y z], /kick, /ban, /clearprops)",
			Timestamp: time.Now().Unix(),
		}, false)
	}

	log.Printf("Player joined: %s (%s) [total: %d]", cc.nick, auth.PubKey[:12], total)

	// Replay active world props and currently connected peers' last known poses
	for _, pr := range existingProps {
		cc.enqueueJSON(pr, false)
	}
	for _, st := range existingPeers {
		cc.enqueueJSON(st, false)
	}

	defer func() {
		s.mu.Lock()
		wasCurrent := false
		if cur, ok := s.clients[auth.PubKey]; ok && cur == cc {
			delete(s.clients, auth.PubKey)
			wasCurrent = true
		}
		var removed []string
		if wasCurrent && s.placementMode == "ephemeral" {
			for id, pr := range s.props {
				if pr.PubKey == auth.PubKey {
					delete(s.props, id)
					removed = append(removed, id)
				}
			}
		}
		s.mu.Unlock()

		if wasCurrent {
			s.broadcast(auth.PubKey, WireMsg{Type: "LEAVE", PubKey: auth.PubKey}, false)
			for _, id := range removed {
				s.broadcast(auth.PubKey, WireMsg{Type: "PROP_REMOVE", PropID: id}, false)
			}
		}
		log.Printf("Player left: %s (%s)", cc.nick, auth.PubKey[:12])
	}()

	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(PongWait))

		if msgType == websocket.BinaryMessage {
			if len(raw) > 1 && raw[0] == 0x02 && len(raw) <= MaxVoiceFrameBytes {
				pubBytes, err := hex.DecodeString(auth.PubKey)
				if err == nil && len(pubBytes) == 32 {
					packet := make([]byte, 1+32+len(raw)-1)
					packet[0] = 0x02
					copy(packet[1:33], pubBytes)
					copy(packet[33:], raw[1:])
					s.relayVoice(cc, packet)
				}
			}
			continue
		}

		var msg WireMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			s.sendError(cc, "ERR_BAD_JSON", "malformed JSON frame")
			continue
		}
		msg.PubKey = auth.PubKey

		switch msg.Type {
		case "STATE":
			now := time.Now()
			if !cc.lastStateAt.IsZero() && now.Sub(cc.lastStateAt) < 12*time.Millisecond {
				continue
			}
			cc.lastStateAt = now

			if len(msg.Pos) > 0 && !finiteVec3(msg.Pos) {
				s.sendError(cc, "ERR_INVALID_POS", "position must be 3 finite floats")
				continue
			}
			msg.Yaw = sanitizeFiniteFloat(msg.Yaw)
			msg.Pitch = sanitizeFiniteFloat(msg.Pitch)
			msg.Roll = sanitizeFiniteFloat(msg.Roll)

			s.mu.Lock()
			if len(msg.Pos) == 3 {
				cc.pos = [3]float32{msg.Pos[0], msg.Pos[1], msg.Pos[2]}
				cc.hasState = true
			}
			cc.yaw = msg.Yaw
			if msg.Nick != "" {
				cc.nick = sanitizeNick(msg.Nick, auth.PubKey)
			}
			if msg.AvatarHash != "" {
				cc.avatarHash = msg.AvatarHash
			}
			msg.Nick = cc.nick
			s.mu.Unlock()

			if msg.AvatarHash != "" && msg.AvatarHash != "none" && s.isItemBlocked(msg.AvatarHash) {
				msg.AvatarHash = "none"
			}
			s.broadcast(auth.PubKey, msg, true)

		case "REQ_ASSET":
			if !isValidHex64(msg.Hash) {
				s.sendError(cc, "ERR_INVALID_HASH", "asset hash must be 64-char hex SHA-256")
				continue
			}
			if s.isItemBlocked(msg.Hash) {
				s.sendError(cc, "ERR_BLOCKED_ITEM", "requested asset hash is blocked")
				continue
			}
			if msg.TargetPub != "" {
				s.mu.RLock()
				targetOnline := s.clients[msg.TargetPub] != nil
				s.mu.RUnlock()
				if targetOnline {
					s.sendTo(msg.TargetPub, msg, false)
				} else if !s.streamCachedPropToPeer(cc, msg.Hash) {
					s.broadcast(auth.PubKey, WireMsg{Type: "REQ_ASSET", PubKey: auth.PubKey, Hash: msg.Hash}, false)
				}
			} else {
				if !s.streamCachedPropToPeer(cc, msg.Hash) {
					s.broadcast(auth.PubKey, msg, false)
				}
			}

		case "ASSET_CHUNK":
			if !isValidHex64(msg.Hash) || len(msg.Data) > MaxAssetChunkChars {
				s.sendError(cc, "ERR_INVALID_CHUNK", "invalid asset chunk hash or size")
				continue
			}
			if s.isItemBlocked(msg.Hash) {
				continue
			}
			if (msg.TargetPub == "" || strings.EqualFold(msg.TargetPub, auth.PubKey)) && s.handleRelayCacheChunk(auth.PubKey, msg) {
				continue
			}
			if msg.TargetPub != "" {
				s.sendTo(msg.TargetPub, msg, false)
			} else {
				s.broadcast(auth.PubKey, msg, false)
			}

		case "PROP_SPAWN":
			if s.placementMode == "forbid" {
				s.sendError(cc, "ERR_PLACEMENT_FORBIDDEN", "prop placement is disabled on this world")
				continue
			}
			if s.placementMode == "admin_only" {
				s.mu.RLock()
				isAdm := s.adminKeys[pubLower]
				s.mu.RUnlock()
				if !isAdm {
					s.sendError(cc, "ERR_PLACEMENT_ADMIN_ONLY", "prop placement is restricted to world admins")
					continue
				}
			}
			if !isValidHex64(msg.Hash) || msg.PropID == "" || len(msg.PropID) > 64 || !finiteVec3(msg.Pos) {
				s.sendError(cc, "ERR_INVALID_PROP", "prop requires valid prop_id, 64-char sha256 hash, and 3D pos")
				continue
			}
			if math.IsNaN(float64(msg.Yaw)) || math.IsInf(float64(msg.Yaw), 0) {
				msg.Yaw = 0
			}
			if math.IsNaN(float64(msg.Scale)) || math.IsInf(float64(msg.Scale), 0) || msg.Scale <= 0 {
				msg.Scale = 1.0
			}
			if msg.Scale > 25.0 {
				msg.Scale = 25.0
			}

			s.mu.Lock()
			hLower := strings.ToLower(msg.Hash)
			if s.blockedItems[hLower] || s.blockedItems[msg.Hash] {
				s.mu.Unlock()
				s.sendError(cc, "ERR_BLOCKED_ITEM", "prop asset hash is blocked on this relay")
				continue
			}
			if ownerBytes, err := os.ReadFile(s.propCachePath(hLower) + ".owner"); err == nil {
				cachedOwner := strings.TrimSpace(string(ownerBytes))
				if cachedOwner != "" && !strings.EqualFold(cachedOwner, pubLower) && !s.adminKeys[pubLower] {
					s.mu.Unlock()
					s.sendError(cc, "ERR_DRM_PROTECTED", "asset is display-protected by world owner")
					continue
				}
			}
			if existing, exists := s.props[msg.PropID]; exists && existing.PubKey != auth.PubKey {
				s.mu.Unlock()
				s.sendError(cc, "ERR_PROP_OWNERSHIP", "prop_id belongs to another peer")
				continue
			}
			owned := 0
			for _, pr := range s.props {
				if pr.PubKey == auth.PubKey && pr.PropID != msg.PropID {
					owned++
				}
			}
			if owned >= MaxPropsPerClient || len(s.props) >= MaxTotalProps {
				s.mu.Unlock()
				s.sendError(cc, "ERR_PROP_LIMIT", "prop limit reached")
				continue
			}
			s.props[msg.PropID] = msg
			s.savePersistentPropsLocked()
			s.mu.Unlock()

			s.requestPropCacheFromClient(cc, auth.PubKey, msg.Hash)
			s.broadcast(auth.PubKey, msg, false)

		case "PROP_REMOVE":
			if msg.PropID == "" {
				continue
			}
			s.mu.Lock()
			pr, ok := s.props[msg.PropID]
			allowDelete := ok && (pr.PubKey == auth.PubKey || s.adminKeys[pubLower])
			if allowDelete {
				delete(s.props, msg.PropID)
				s.savePersistentPropsLocked()
				s.gcUnusedCachedPropsLocked()
				s.mu.Unlock()
				s.broadcast("", msg, false)
			} else {
				s.mu.Unlock()
				s.sendError(cc, "ERR_PROP_OWNERSHIP", "prop not found or owned by another peer")
			}

		case "CHAT":
			now := time.Now()
			if !cc.lastChatAt.IsZero() && now.Sub(cc.lastChatAt) < 250*time.Millisecond {
				s.sendError(cc, "ERR_RATE_LIMIT", "chat rate limit exceeded")
				continue
			}
			cc.lastChatAt = now

			txt := strings.TrimSpace(msg.Text)
			if txt == "" || len(txt) > 512 {
				continue
			}

			// Any authenticated user can clear their own placed props via /clearmyprops
			if txt == "/clearmyprops" {
				s.handleClearMyProps(cc)
				continue
			}

			// Handle admin slash commands (/setspawn, /kick, /ban, /clearprops) if sender is in admins list
			if txt == "/setspawn" || strings.HasPrefix(txt, "/setspawn ") ||
				strings.HasPrefix(txt, "/kick ") || strings.HasPrefix(txt, "/ban ") || txt == "/clearprops" {
				s.handleAdminCommand(cc, txt)
				continue
			}

			msg.Text = txt
			msg.Nick = cc.nick
			msg.Timestamp = now.Unix()

			if msg.Channel == "proximity" {
				s.broadcastProximity(cc, msg, 35.0)
			} else if msg.Channel == "whisper" && msg.TargetPub != "" {
				s.sendTo(msg.TargetPub, msg, false)
			} else {
				msg.Channel = "global"
				s.broadcast(auth.PubKey, msg, false)
			}

		default:
			s.sendError(cc, "ERR_UNKNOWN_TYPE", fmt.Sprintf("unknown message type %q", msg.Type))
		}
	}
}

func (s *RelayServer) handleClearMyProps(cc *ClientConn) {
	s.mu.Lock()
	var ids []string
	for id, pr := range s.props {
		if strings.EqualFold(pr.PubKey, cc.pubKey) {
			ids = append(ids, id)
			delete(s.props, id)
		}
	}
	if len(ids) > 0 {
		s.savePersistentPropsLocked()
		s.gcUnusedCachedPropsLocked()
	}
	s.mu.Unlock()

	for _, id := range ids {
		s.broadcast("", WireMsg{Type: "PROP_REMOVE", PropID: id}, false)
	}
	cc.enqueueJSON(WireMsg{
		Type:      "CHAT",
		Nick:      "RELAY",
		Channel:   "whisper",
		Text:      fmt.Sprintf("Cleared %d of your props from the world.", len(ids)),
		Timestamp: time.Now().Unix(),
	}, false)
}

func (s *RelayServer) handleAdminCommand(cc *ClientConn, cmd string) {
	pubLower := strings.ToLower(cc.pubKey)
	s.mu.RLock()
	isAdmin := s.adminKeys[pubLower]
	s.mu.RUnlock()

	if !isAdmin {
		s.sendError(cc, "ERR_FORBIDDEN", "admin privileges required")
		return
	}

	if cmd == "/setspawn" || strings.HasPrefix(cmd, "/setspawn ") {
		var newPos []float32
		if cmd == "/setspawn" {
			s.mu.RLock()
			if !cc.hasState {
				s.mu.RUnlock()
				s.sendError(cc, "ERR_NO_POS", "move slightly first or pass /setspawn <x> <y> <z>")
				return
			}
			newPos = []float32{cc.pos[0], cc.pos[1], cc.pos[2]}
			s.mu.RUnlock()
		} else {
			var x, y, z float32
			if _, err := fmt.Sscanf(strings.TrimPrefix(cmd, "/setspawn "), "%f %f %f", &x, &y, &z); err != nil {
				s.sendError(cc, "ERR_INVALID_POS", "usage: /setspawn or /setspawn <x> <y> <z>")
				return
			}
			newPos = []float32{x, y, z}
		}
		if !finiteVec3(newPos) || math.Abs(float64(newPos[0])) > 10000 || math.Abs(float64(newPos[1])) > 10000 || math.Abs(float64(newPos[2])) > 10000 {
			s.sendError(cc, "ERR_INVALID_POS", "spawn coordinates out of valid range")
			return
		}

		s.mu.Lock()
		err := s.updateWorldSpawnLocked(newPos)
		s.mu.Unlock()
		if err != nil {
			s.sendError(cc, "ERR_SPAWN_SAVE", "failed saving spawn to world.toml: "+err.Error())
			return
		}

		s.broadcast("", WireMsg{Type: "SPAWN_UPDATE", Pos: newPos}, false)
		cc.enqueueJSON(WireMsg{
			Type:      "CHAT",
			PubKey:    cc.pubKey,
			Nick:      "RELAY",
			Channel:   "whisper",
			Text:      fmt.Sprintf("Updated world spawn to [%.2f, %.2f, %.2f] in world.toml.", newPos[0], newPos[1], newPos[2]),
			Timestamp: time.Now().Unix(),
		}, false)
		return
	}

	if cmd == "/clearprops" {
		s.mu.Lock()
		ids := make([]string, 0, len(s.props))
		for id := range s.props {
			ids = append(ids, id)
		}
		s.props = make(map[string]WireMsg)
		s.savePersistentPropsLocked()
		s.gcUnusedCachedPropsLocked()
		s.mu.Unlock()

		for _, id := range ids {
			s.broadcast("", WireMsg{Type: "PROP_REMOVE", PropID: id}, false)
		}
		cc.enqueueJSON(WireMsg{
			Type:      "CHAT",
			PubKey:    cc.pubKey,
			Nick:      "RELAY",
			Channel:   "whisper",
			Text:      fmt.Sprintf("Cleared %d props from world.", len(ids)),
			Timestamp: time.Now().Unix(),
		}, false)
		return
	}

	isBan := strings.HasPrefix(cmd, "/ban ")
	query := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(cmd, "/ban "), "/kick "))
	if query == "" {
		return
	}
	qLower := strings.ToLower(query)

	s.mu.Lock()
	var target *ClientConn
	for pub, peer := range s.clients {
		if pub == cc.pubKey {
			continue
		}
		shortPub := strings.ToLower(pub)
		if len(shortPub) > 6 {
			shortPub = shortPub[:6]
		}
		peerTag := strings.ToLower(peer.nick) + "#" + shortPub
		if strings.HasPrefix(strings.ToLower(pub), qLower) || strings.EqualFold(peer.nick, query) || peerTag == qLower {
			target = peer
			break
		}
	}
	if isBan && target != nil {
		s.appendBanKeyLocked(target.pubKey)
	} else if isBan && isValidHex64(qLower) {
		s.appendBanKeyLocked(qLower)
	}
	s.mu.Unlock()

	if target != nil {
		action := "kicked"
		code := "ERR_KICKED"
		if isBan {
			action = "banned"
			code = "ERR_BANNED"
		}
		s.sendError(target, code, "you were "+action+" by a world admin")
		go func(t *ClientConn) {
			time.Sleep(50 * time.Millisecond)
			t.close()
		}(target)
		cc.enqueueJSON(WireMsg{
			Type:      "CHAT",
			PubKey:    cc.pubKey,
			Nick:      "RELAY",
			Channel:   "whisper",
			Text:      fmt.Sprintf("Admin %s %s (%s...).", action, target.nick, target.pubKey[:12]),
			Timestamp: time.Now().Unix(),
		}, false)
	} else if isBan && isValidHex64(qLower) {
		cc.enqueueJSON(WireMsg{
			Type:      "CHAT",
			PubKey:    cc.pubKey,
			Nick:      "RELAY",
			Channel:   "whisper",
			Text:      fmt.Sprintf("Added offline pubkey %s... to bans.txt.", qLower[:12]),
			Timestamp: time.Now().Unix(),
		}, false)
	} else {
		s.sendError(cc, "ERR_NOT_FOUND", "no matching online peer for "+query)
	}
}

func sanitizeControlChars(raw string, maxLen int) string {
	var b strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(raw) {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
		count++
		if maxLen > 0 && count >= maxLen {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func sanitizeNick(raw, pub string) string {
	var b strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(raw) {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
		count++
		if count >= 24 {
			break
		}
	}
	n := strings.TrimSpace(b.String())
	if n == "" {
		if len(pub) >= 8 {
			return pub[:8]
		}
		return "anon"
	}
	return n
}

func sanitizeCaps(caps []string) []string {
	if len(caps) == 0 {
		return nil
	}
	if len(caps) > 16 {
		caps = caps[:16]
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		c = strings.TrimSpace(c)
		if c != "" && len(c) <= 32 {
			out = append(out, c)
		}
	}
	return out
}

func (s *RelayServer) relayVoice(sender *ClientConn, packet []byte) {
	s.mu.RLock()
	sPos := sender.pos
	var targets []*ClientConn
	for pub, c := range s.clients {
		if pub == sender.pubKey {
			continue
		}
		dx := float64(c.pos[0] - sPos[0])
		dy := float64(c.pos[1] - sPos[1])
		dz := float64(c.pos[2] - sPos[2])
		if math.Sqrt(dx*dx+dy*dy+dz*dz) <= 35.0 {
			targets = append(targets, c)
		}
	}
	s.mu.RUnlock()

	for _, t := range targets {
		t.enqueueRaw(websocket.BinaryMessage, packet, true)
	}
}

func (s *RelayServer) broadcastProximity(sender *ClientConn, msg WireMsg, radius float64) {
	s.mu.RLock()
	sPos := sender.pos
	var targets []*ClientConn
	for pub, c := range s.clients {
		if pub == sender.pubKey {
			continue
		}
		dx := float64(c.pos[0] - sPos[0])
		dy := float64(c.pos[1] - sPos[1])
		dz := float64(c.pos[2] - sPos[2])
		if math.Sqrt(dx*dx+dy*dy+dz*dz) <= radius {
			targets = append(targets, c)
		}
	}
	s.mu.RUnlock()

	for _, t := range targets {
		t.enqueueJSON(msg, false)
	}
}

func (s *RelayServer) isItemBlocked(hash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.blockedItems[strings.ToLower(hash)] || s.blockedItems[hash]
}

func (s *RelayServer) broadcast(senderPub string, msg WireMsg, dropOK bool) {
	s.mu.RLock()
	peers := make([]*ClientConn, 0, len(s.clients))
	for pub, c := range s.clients {
		if pub != senderPub {
			peers = append(peers, c)
		}
	}
	s.mu.RUnlock()

	for _, p := range peers {
		p.enqueueJSON(msg, dropOK)
	}
}

func (s *RelayServer) sendTo(targetPub string, msg WireMsg, dropOK bool) {
	s.mu.RLock()
	target := s.clients[targetPub]
	s.mu.RUnlock()
	if target != nil {
		target.enqueueJSON(msg, dropOK)
	}
}

func loadFlatSet(path string) map[string]bool {
	set := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		return set
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		set[strings.ToLower(fields[0])] = true
	}
	return set
}
