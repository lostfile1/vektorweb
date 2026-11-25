package main

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"vektorweb/internal/identity"
)

type WireMsg struct {
	Type       string    `json:"type"`
	Nonce      string    `json:"nonce,omitempty"`
	Timestamp  int64     `json:"timestamp,omitempty"`
	PubKey     string    `json:"pubkey,omitempty"`
	TargetPub  string    `json:"target_pub,omitempty"`
	Signature  string    `json:"signature,omitempty"`
	AvatarHash string    `json:"avatar_hash,omitempty"`
	Nick       string    `json:"nick,omitempty"`
	Pos        []float32 `json:"pos,omitempty"`
	Yaw        float32   `json:"yaw,omitempty"`
	Hash       string    `json:"hash,omitempty"`
	Seq        int       `json:"seq,omitempty"`
	Total      int       `json:"total,omitempty"`
	Data       string    `json:"data,omitempty"`
	Text       string    `json:"text,omitempty"`
	Channel    string    `json:"channel,omitempty"`
	PropID     string    `json:"prop_id,omitempty"`
	Scale      float32   `json:"scale,omitempty"`
}

type ClientConn struct {
	conn       *websocket.Conn
	writeMu    sync.Mutex
	pubKey     string
	nick       string
	avatarHash string
	pos        [3]float32
}

func (c *ClientConn) sendJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteJSON(v)
}

func (c *ClientConn) sendBinary(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return c.conn.WriteMessage(websocket.BinaryMessage, payload)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type RelayServer struct {
	domain        string
	worldDir      string
	serveHTTP     bool
	placementMode string
	mu            sync.RWMutex
	bannedKeys    map[string]bool
	blockedItems  map[string]bool
	clients       map[string]*ClientConn
	props         map[string]WireMsg
}

func main() {
	addr := flag.String("addr", "0.0.0.0:7777", "Listen address (host:port)")
	domain := flag.String("domain", "", "Public domain or IP bound to Ed25519 challenges (auto-detected from -addr if empty)")
	worldDir := flag.String("dir", "./world", "World folder to auto-create and serve")
	serveHTTP := flag.Bool("http", true, "Serve world files over HTTP on the same port (disable if using Apache/Nginx)")
	placement := flag.String("placement", "ephemeral", "Prop placement mode: forbid | ephemeral | persistent")
	flag.Parse()

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

	srv := &RelayServer{
		domain:        resolvedDomain,
		worldDir:      *worldDir,
		serveHTTP:     *serveHTTP,
		placementMode: *placement,
		bannedKeys:    loadFlatSet(filepath.Join(*worldDir, "bans.txt")),
		blockedItems:  loadFlatSet(filepath.Join(*worldDir, "blocked_items.txt")),
		clients:       make(map[string]*ClientConn),
		props:         make(map[string]WireMsg),
	}

	if *placement == "persistent" {
		srv.loadPersistentProps()
	}

	http.HandleFunc("/", srv.handleRoot)

	displayHost := resolvedDomain
	port := "7777"
	if idx := strings.LastIndex(*addr, ":"); idx != -1 {
		port = (*addr)[idx+1:]
	}

	log.Printf("vektorweb-relay listening on %s (domain=%s, placement=%s)", *addr, resolvedDomain, *placement)
	log.Printf("World folder : %s", *worldDir)
	if *serveHTTP {
		log.Printf("Join URL     : http://%s:%s/world.toml", displayHost, port)
	}
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func ensureWorldDir(dir, addr, domain string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

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
	hw, hh, hd := w/2, h, d/2
	verts := []float32{
		-hw, -hh, -hd, hw, -hh, -hd, hw, -hh, hd, -hw, -hh, hd,
		-hw, 0, -hd, hw, 0, -hd, hw, 0, hd, -hw, 0, hd,
	}
	indices := []uint16{
		0, 1, 2, 0, 2, 3, 4, 6, 5, 4, 7, 6, 0, 4, 5, 0, 5, 1,
		1, 5, 6, 1, 6, 2, 2, 6, 7, 2, 7, 3, 3, 7, 4, 3, 4, 0,
	}
	vBytes := make([]byte, len(verts)*4)
	for i, v := range verts {
		binary.LittleEndian.PutUint32(vBytes[i*4:], math.Float32bits(v))
	}
	iBytes := make([]byte, len(indices)*2)
	for i, idx := range indices {
		binary.LittleEndian.PutUint16(iBytes[i*2:], idx)
	}
	binBuf := append(vBytes, iBytes...)

	gltf := map[string]any{
		"asset":   map[string]string{"version": "2.0"},
		"scene":   0,
		"scenes":  []any{map[string]any{"nodes": []int{0}}},
		"nodes":   []any{map[string]int{"mesh": 0}},
		"meshes":  []any{map[string]any{"primitives": []any{map[string]any{"attributes": map[string]int{"POSITION": 0}, "indices": 1}}}},
		"buffers": []any{map[string]int{"byteLength": len(binBuf)}},
		"bufferViews": []any{
			map[string]int{"buffer": 0, "byteOffset": 0, "byteLength": len(vBytes)},
			map[string]int{"buffer": 0, "byteOffset": len(vBytes), "byteLength": len(iBytes)},
		},
		"accessors": []any{
			map[string]any{"bufferView": 0, "componentType": 5126, "count": 8, "type": "VEC3", "min": []float32{-hw, -hh, -hd}, "max": []float32{hw, 0, hd}},
			map[string]any{"bufferView": 1, "componentType": 5123, "count": 36, "type": "SCALAR"},
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

func (s *RelayServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		s.handleWS(w, r)
		return
	}
	if !s.serveHTTP {
		http.Error(w, "vektorweb-relay WebSocket endpoint", http.StatusUpgradeRequired)
		return
	}

	cleanPath := filepath.Clean("/" + r.URL.Path)
	base := filepath.Base(cleanPath)
	if base == "bans.txt" || base == "blocked_items.txt" || base == "world_props.json" || strings.HasPrefix(base, ".") {
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
			if pr.PropID != "" && !s.blockedItems[pr.Hash] {
				s.props[pr.PropID] = pr
			}
		}
		log.Printf("Restored %d persistent props", len(s.props))
	}
}

func (s *RelayServer) savePersistentPropsLocked() {
	if s.placementMode != "persistent" {
		return
	}
	list := make([]WireMsg, 0, len(s.props))
	for _, pr := range s.props {
		list = append(list, pr)
	}
	if b, err := json.MarshalIndent(list, "", "  "); err == nil {
		_ = os.WriteFile(s.persistentPropsPath(), b, 0644)
	}
}

func (s *RelayServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	conn.SetReadLimit(512 * 1024)

	nonceBytes := make([]byte, 32)
	_, _ = rand.Read(nonceBytes)
	nonce := hex.EncodeToString(nonceBytes)
	ts := time.Now().Unix()

	cc := &ClientConn{conn: conn}
	if err := cc.sendJSON(WireMsg{Type: "CHALLENGE", Nonce: nonce, Timestamp: ts}); err != nil {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var auth WireMsg
	if err := conn.ReadJSON(&auth); err != nil || auth.Type != "AUTH" {
		return
	}

	if !identity.VerifyChallenge(auth.PubKey, nonce, ts, s.domain, auth.Signature) {
		log.Printf("Rejected invalid Ed25519 signature from %s", r.RemoteAddr)
		return
	}

	s.mu.RLock()
	banned := s.bannedKeys[auth.PubKey]
	blockedAvatar := auth.AvatarHash != "" && s.blockedItems[auth.AvatarHash]
	s.mu.RUnlock()

	if banned {
		log.Printf("Blocked banned pubkey %s", auth.PubKey[:12])
		return
	}
	if blockedAvatar {
		log.Printf("Rejected blocked avatar %s from %s", auth.AvatarHash[:12], auth.PubKey[:12])
		return
	}

	cc.pubKey = auth.PubKey
	cc.avatarHash = auth.AvatarHash
	cc.nick = sanitizeNick(auth.Nick, auth.PubKey)

	s.mu.Lock()
	if old, exists := s.clients[auth.PubKey]; exists {
		_ = old.conn.Close()
	}
	s.clients[auth.PubKey] = cc
	total := len(s.clients)
	existingProps := make([]WireMsg, 0, len(s.props))
	for _, pr := range s.props {
		existingProps = append(existingProps, pr)
	}
	s.mu.Unlock()

	log.Printf("Player joined: %s (%s) [total: %d]", cc.nick, auth.PubKey[:12], total)

	for _, pr := range existingProps {
		_ = cc.sendJSON(pr)
	}

	defer func() {
		s.mu.Lock()
		if cur, ok := s.clients[auth.PubKey]; ok && cur == cc {
			delete(s.clients, auth.PubKey)
		}
		var removed []string
		if s.placementMode == "ephemeral" {
			for id, pr := range s.props {
				if pr.PubKey == auth.PubKey {
					delete(s.props, id)
					removed = append(removed, id)
				}
			}
		}
		s.mu.Unlock()

		s.broadcast(auth.PubKey, WireMsg{Type: "LEAVE", PubKey: auth.PubKey})
		for _, id := range removed {
			s.broadcast(auth.PubKey, WireMsg{Type: "PROP_REMOVE", PropID: id})
		}
		log.Printf("Player left: %s (%s)", cc.nick, auth.PubKey[:12])
	}()

	_ = conn.SetReadDeadline(time.Time{})
	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}

		if msgType == websocket.BinaryMessage {
			if len(raw) > 1 && raw[0] == 0x02 && len(raw) <= 32*1024 {
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
			continue
		}
		msg.PubKey = auth.PubKey

		switch msg.Type {
		case "STATE":
			s.mu.Lock()
			if len(msg.Pos) == 3 {
				cc.pos = [3]float32{msg.Pos[0], msg.Pos[1], msg.Pos[2]}
			}
			if msg.Nick != "" {
				cc.nick = sanitizeNick(msg.Nick, auth.PubKey)
			}
			msg.Nick = cc.nick
			s.mu.Unlock()

			if msg.AvatarHash != "" && s.isItemBlocked(msg.AvatarHash) {
				msg.AvatarHash = "none"
			}
			s.broadcast(auth.PubKey, msg)

		case "REQ_ASSET":
			if msg.Hash == "" || s.isItemBlocked(msg.Hash) {
				continue
			}
			s.sendTo(msg.TargetPub, msg)

		case "ASSET_CHUNK":
			if msg.Hash == "" || s.isItemBlocked(msg.Hash) || len(msg.Data) > 128*1024 {
				continue
			}
			if msg.TargetPub != "" {
				s.sendTo(msg.TargetPub, msg)
			} else {
				s.broadcast(auth.PubKey, msg)
			}

		case "PROP_SPAWN":
			if s.placementMode == "forbid" || msg.Hash == "" || msg.PropID == "" || len(msg.Pos) != 3 {
				continue
			}
			if s.isItemBlocked(msg.Hash) {
				continue
			}
			s.mu.Lock()
			s.props[msg.PropID] = msg
			s.savePersistentPropsLocked()
			s.mu.Unlock()
			s.broadcast(auth.PubKey, msg)

		case "PROP_REMOVE":
			s.mu.Lock()
			pr, ok := s.props[msg.PropID]
			if ok && pr.PubKey == auth.PubKey {
				delete(s.props, msg.PropID)
				s.savePersistentPropsLocked()
				s.mu.Unlock()
				s.broadcast(auth.PubKey, msg)
			} else {
				s.mu.Unlock()
			}

		case "CHAT":
			txt := strings.TrimSpace(msg.Text)
			if txt == "" || len(txt) > 512 {
				continue
			}
			msg.Text = txt
			msg.Nick = cc.nick
			msg.Timestamp = time.Now().Unix()

			if msg.Channel == "proximity" {
				s.broadcastProximity(cc, msg, 35.0)
			} else if msg.Channel == "whisper" && msg.TargetPub != "" {
				s.sendTo(msg.TargetPub, msg)
			} else {
				msg.Channel = "global"
				s.broadcast(auth.PubKey, msg)
			}
		}
	}
}

func sanitizeNick(raw, pub string) string {
	n := strings.TrimSpace(raw)
	if n == "" {
		if len(pub) >= 8 {
			return pub[:8]
		}
		return "anon"
	}
	if len(n) > 24 {
		n = n[:24]
	}
	return n
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
		_ = t.sendBinary(packet)
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
		_ = t.sendJSON(msg)
	}
}

func (s *RelayServer) isItemBlocked(hash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.blockedItems[hash]
}

func (s *RelayServer) broadcast(senderPub string, msg WireMsg) {
	s.mu.RLock()
	peers := make([]*ClientConn, 0, len(s.clients))
	for pub, c := range s.clients {
		if pub != senderPub {
			peers = append(peers, c)
		}
	}
	s.mu.RUnlock()

	for _, p := range peers {
		_ = p.sendJSON(msg)
	}
}

func (s *RelayServer) sendTo(targetPub string, msg WireMsg) {
	s.mu.RLock()
	target := s.clients[targetPub]
	s.mu.RUnlock()
	if target != nil {
		_ = target.sendJSON(msg)
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
		set[fields[0]] = true
	}
	return set
}
