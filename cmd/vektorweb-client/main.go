package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/BurntSushi/toml"
	rl "github.com/gen2brain/raylib-go/raylib"
	"github.com/gorilla/websocket"

	"vektorweb/internal/glbguard"
	"vektorweb/internal/identity"
	"vektorweb/internal/luavm"
)

const (
	MaxWorldManifestBytes = 256 * 1024
	MaxWorldScriptBytes   = 256 * 1024
	MaxDefaultEnvBytes    = 50 * 1024 * 1024
)

type WorldPortal struct {
	Name   string    `toml:"name"`
	URL    string    `toml:"url"`
	Pos    []float32 `toml:"pos"`
	Radius float32   `toml:"radius"`
}

type WorldManifest struct {
	World struct {
		Title        string    `toml:"title"`
		AuthorPubkey string    `toml:"author_pubkey"`
		RelayURL     string    `toml:"relay_url"`
		Spawn        []float32 `toml:"spawn"`
	} `toml:"world"`
	Assets struct {
		Environment       string `toml:"environment"`
		EnvironmentSHA256 string `toml:"environment_sha256"`
		Script            string `toml:"script"`
		ScriptSHA256      string `toml:"script_sha256"`
	} `toml:"assets"`
	Rules struct {
		PlacementMode string `toml:"placement_mode"`
		MaxAvatarMB   int    `toml:"max_avatar_mb"`
		MaxItemMB     int    `toml:"max_item_mb"`
	} `toml:"rules"`
	Portals []WorldPortal `toml:"portals"`
}

type WorldDirectory struct {
	Worlds []struct {
		Name string `toml:"name"`
		URL  string `toml:"url"`
	} `toml:"worlds"`
}

type ClientSettings struct {
	Nick        string  `toml:"nick"`
	FOV         float32 `toml:"fov"`
	MouseSens   float32 `toml:"mouse_sens"`
	VoiceVolume float32 `toml:"voice_volume"`
	ShowRadar   bool    `toml:"show_radar"`
	ShowTags    bool    `toml:"show_tags"`
	ShowCross   bool    `toml:"show_crosshair"`
	EnableFog   bool    `toml:"enable_fog"`
}

func defaultClientSettings() ClientSettings {
	return ClientSettings{
		FOV:         72.0,
		MouseSens:   0.0025,
		VoiceVolume: 1.0,
		ShowRadar:   true,
		ShowTags:    true,
		ShowCross:   true,
		EnableFog:   true,
	}
}

func loadClientSettings(path string) ClientSettings {
	cfg := defaultClientSettings()
	if b, err := os.ReadFile(path); err == nil {
		_, _ = toml.Decode(string(b), &cfg)
	}
	if cfg.FOV < 50 || cfg.FOV > 110 {
		cfg.FOV = 72.0
	}
	if cfg.MouseSens < 0.0005 || cfg.MouseSens > 0.015 {
		cfg.MouseSens = 0.0025
	}
	if cfg.VoiceVolume < 0 || cfg.VoiceVolume > 2.0 {
		cfg.VoiceVolume = 1.0
	}
	return cfg
}

func saveClientSettings(path string, cfg ClientSettings) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "nick = %q\n", cfg.Nick)
	fmt.Fprintf(&sb, "fov = %.1f\n", cfg.FOV)
	fmt.Fprintf(&sb, "mouse_sens = %.4f\n", cfg.MouseSens)
	fmt.Fprintf(&sb, "voice_volume = %.2f\n", cfg.VoiceVolume)
	fmt.Fprintf(&sb, "show_radar = %v\n", cfg.ShowRadar)
	fmt.Fprintf(&sb, "show_tags = %v\n", cfg.ShowTags)
	fmt.Fprintf(&sb, "show_crosshair = %v\n", cfg.ShowCross)
	fmt.Fprintf(&sb, "enable_fog = %v\n", cfg.EnableFog)
	_ = os.WriteFile(path, []byte(sb.String()), 0644)
}

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

type TransferWhisperPayload struct {
	Action     string                 `json:"action"` // "OFFER" | "ACCEPT" | "DECLINE"
	TransferID string                 `json:"transfer_id"`
	AssetKind  string                 `json:"asset_kind"` // "prop" | "avatar"
	FileName   string                 `json:"file_name"`
	Hash       string                 `json:"hash"`
	License    *identity.AssetLicense `json:"license,omitempty"`
}

const transferWhisperPrefix = "VWXFER:"

type OutgoingTransfer struct {
	TransferID string
	TargetPub  string
	AssetKind  string
	FilePath   string
	FileName   string
	Hash       string
	License    *identity.AssetLicense
}

type IncomingTransfer struct {
	TransferID string
	FromPub    string
	FromNick   string
	AssetKind  string
	FileName   string
	Hash       string
	License    *identity.AssetLicense
}

type PendingIncomingFile struct {
	FileName  string
	AssetKind string
	License   *identity.AssetLicense
	FromPub   string
}

type RemotePeer struct {
	PubKey     string
	Nick       string
	Pos        rl.Vector3
	Yaw        float32
	TargetPos  rl.Vector3
	TargetYaw  float32
	HasPos     bool
	AvatarHash string
	LastSeen   time.Time
	Speaking   time.Time
}

type WorldProp struct {
	PropID string
	Owner  string
	Hash   string
	Pos    rl.Vector3
	Yaw    float32
	Scale  float32
}

type InventoryItem struct {
	Name      string
	Path      string
	Hash      string
	Kind      string
	License   *identity.AssetLicense
	StatusTag string
	CanSend   bool
	IsCreator bool
}

type PendingDrop struct {
	FileName string
	Hash     string
	Bytes    []byte
}

type AssetAssembler struct {
	Total     int
	Chunks    map[int][]byte
	CreatedAt time.Time
}

type AssemblyProgress struct {
	Hash     string
	Received int
	Total    int
}

type ChatEntry struct {
	Text      string
	Color     rl.Color
	Timestamp time.Time
}

type ModelAnimSet struct {
	Anims      []rl.ModelAnimation
	Frame      int32
	LastUpdate float64
}

func modelFeetOffsetY(m rl.Model) float32 {
	if m.MeshCount <= 0 {
		return 0
	}
	bb := rl.GetModelBoundingBox(m)
	if math.IsNaN(float64(bb.Min.Y)) || math.IsInf(float64(bb.Min.Y), 0) {
		return 0
	}
	return -bb.Min.Y
}

type RayTarget struct {
	Kind      string // "prop" | "peer"
	ID        string // propID or peer PubKey
	Label     string
	SubLabel  string
	CanDelete bool
	Box       rl.BoundingBox
}

type NetState struct {
	mu               sync.RWMutex
	conn             *websocket.Conn
	writeMu          sync.Mutex
	connected        bool
	relayHost        string
	liveSpawn        []float32
	peers            map[string]*RemotePeer
	props            map[string]*WorldProp
	requested        map[string]bool
	assemblies       map[string]*AssetAssembler
	readyModels      []string
	invDirty         bool
	chatLog          []ChatEntry
	nick             string
	muted            bool
	blockedPeers     map[string]bool
	localPub         string
	localPos         rl.Vector3
	localAvHash      string
	localAvBytes     []byte
	localAssets      map[string][]byte
	cacheDir         string
	itemsDir         string
	avatarsDir       string
	maxAssetB        int
	voiceQueue       [][]float32
	relayCancel      context.CancelFunc
	outgoingXfers    map[string]*OutgoingTransfer
	incomingXfers    []*IncomingTransfer
	pendingRecvFiles map[string]*PendingIncomingFile
}

func (n *NetState) sendJSON(v any) error {
	n.mu.RLock()
	c := n.conn
	n.mu.RUnlock()
	if c == nil {
		return nil
	}
	n.writeMu.Lock()
	defer n.writeMu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	return c.WriteJSON(v)
}

func (n *NetState) sendBinary(payload []byte) error {
	n.mu.RLock()
	c := n.conn
	n.mu.RUnlock()
	if c == nil {
		return nil
	}
	n.writeMu.Lock()
	defer n.writeMu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(1 * time.Second))
	return c.WriteMessage(websocket.BinaryMessage, payload)
}

func (n *NetState) clearChat() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.chatLog = nil
}

func (n *NetState) addChatColor(line string, col rl.Color) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.chatLog = append(n.chatLog, ChatEntry{
		Text:      sanitizeDisplayText(line, 320),
		Color:     col,
		Timestamp: time.Now(),
	})
	if len(n.chatLog) > 24 {
		n.chatLog = n.chatLog[len(n.chatLog)-24:]
	}
}

func (n *NetState) addChat(line string) {
	col := rl.LightGray
	switch {
	case strings.HasPrefix(line, "[RELAY ERR"), strings.HasPrefix(line, "[GLBGUARD"),
		strings.HasPrefix(line, "[DROP ERR"), strings.HasPrefix(line, "[PORTAL ERR"),
		strings.HasPrefix(line, "[LICENSE"), strings.HasPrefix(line, "[MINT ERR"),
		strings.HasPrefix(line, "[TRANSFER ERR"):
		col = rl.NewColor(255, 110, 110, 255)
	case strings.HasPrefix(line, "[TRANSFER]"):
		col = rl.Gold
	case strings.HasPrefix(line, "[WORLD]"), strings.HasPrefix(line, "[PORTAL]"):
		col = rl.SkyBlue
	case strings.HasPrefix(line, "[SYS]"), strings.HasPrefix(line, "[AVATAR]"),
		strings.HasPrefix(line, "[INVENTORY]"), strings.HasPrefix(line, "[PROP]"),
		strings.HasPrefix(line, "[MINTED]"), strings.HasPrefix(line, "[SCREENSHOT]"):
		col = rl.NewColor(130, 230, 160, 255)
	case strings.HasPrefix(line, "[WHISPER"):
		col = rl.NewColor(220, 150, 255, 255)
	}
	n.addChatColor(line, col)
}

func (n *NetState) resetSessionForPortal() {
	n.mu.Lock()
	if n.relayCancel != nil {
		n.relayCancel()
		n.relayCancel = nil
	}
	if n.conn != nil {
		_ = n.conn.Close()
		n.conn = nil
	}
	n.connected = false
	n.liveSpawn = nil
	n.peers = make(map[string]*RemotePeer)
	n.props = make(map[string]*WorldProp)
	n.requested = make(map[string]bool)
	n.assemblies = make(map[string]*AssetAssembler)
	n.outgoingXfers = make(map[string]*OutgoingTransfer)
	n.incomingXfers = nil
	n.pendingRecvFiles = make(map[string]*PendingIncomingFile)
	n.voiceQueue = nil
	n.mu.Unlock()
}

type LoadedWorld struct {
	URL         string
	Manifest    *WorldManifest
	EnvCacheGLB string
	Sandbox     *luavm.WorldVM
}

func sanitizeDisplayText(s string, maxLen int) string {
	var b strings.Builder
	b.Grow(len(s))
	count := 0
	for _, r := range s {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
		count++
		if maxLen > 0 && count >= maxLen {
			break
		}
	}
	return b.String()
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

func formatPeerTag(nick, pubKey string) string {
	cleanNick := sanitizeDisplayText(strings.TrimSpace(nick), 24)
	pubLower := strings.ToLower(strings.TrimSpace(pubKey))
	shortPub := pubLower
	if len(shortPub) > 6 {
		shortPub = shortPub[:6]
	}
	if cleanNick == "" {
		if len(pubLower) >= 8 {
			return pubLower[:8]
		}
		return shortPub
	}
	if shortPub == "" || (len(pubLower) >= 8 && strings.EqualFold(cleanNick, pubLower[:8])) {
		return cleanNick
	}
	return fmt.Sprintf("%s#%s", cleanNick, shortPub)
}

func lerpAngleDeg(a, b, t float32) float32 {
	diff := float32(math.Mod(float64(b-a+540.0), 360.0) - 180.0)
	return a + diff*t
}

func normalizeWorldURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "vektorweb://") {
		rest := strings.TrimPrefix(trimmed, "vektorweb://")
		hostPart := strings.Split(rest, "/")[0]
		if strings.HasPrefix(rest, "127.0.0.1") || strings.HasPrefix(rest, "localhost") ||
			strings.HasPrefix(rest, "[::1]") || strings.Contains(hostPart, ":") {
			return "http://" + rest
		}
		return "https://" + rest
	}
	return trimmed
}

func resolveRelativeAssetURL(baseWorldURL, assetRef string) (string, error) {
	ref := strings.TrimSpace(assetRef)
	if ref == "" {
		return "", fmt.Errorf("empty asset path")
	}
	if strings.Contains(ref, "://") || strings.HasPrefix(ref, "//") {
		return "", fmt.Errorf("external asset URLs are forbidden in world.toml (%q)", ref)
	}
	cleaned := filepath.ToSlash(filepath.Clean("/" + ref))
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("invalid asset path traversal in %q", ref)
	}
	return strings.TrimRight(baseWorldURL, "/") + cleaned, nil
}

func buildDefaultGLB(w, h, d float32) []byte {
	hw, hh, hd := w/2, h, d/2
	verts := []float32{
		-hw, 0, -hd, hw, 0, -hd, hw, 0, hd, -hw, 0, hd,
		-hw * 0.85, hh, -hd * 0.85, hw * 0.85, hh, -hd * 0.85, hw * 0.85, hh, hd * 0.85, -hw * 0.85, hh, hd * 0.85,
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
			map[string]any{"bufferView": 0, "componentType": 5126, "count": 8, "type": "VEC3", "min": []float32{-hw, 0, -hd}, "max": []float32{hw, hh, hd}},
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

// safeSpawnPosition raycasts downward from above the requested spawn point against
// worldModel so players never spawn embedded inside floors, ramps, or geometry.
func safeSpawnPosition(worldModel rl.Model, requested rl.Vector3) rl.Vector3 {
	const eyeHeight = float32(1.65)
	probeOrigin := rl.NewVector3(requested.X, requested.Y+6.0, requested.Z)
	downRay := rl.NewRay(probeOrigin, rl.NewVector3(0, -1, 0))

	bestGround := float32(-9999.0)
	if requested.X >= -32.0 && requested.X <= 32.0 && requested.Z >= -32.0 && requested.Z <= 32.0 {
		bestGround = 0.0
	}
	for i := int32(0); i < worldModel.MeshCount; i++ {
		hit := rl.GetRayCollisionMesh(downRay, worldModel.GetMeshes()[i], worldModel.Transform)
		if hit.Hit && hit.Distance <= 14.0 && hit.Point.Y > bestGround {
			bestGround = hit.Point.Y
		}
	}
	if bestGround > -9000.0 && requested.Y < bestGround+eyeHeight {
		return rl.NewVector3(requested.X, bestGround+eyeHeight+0.05, requested.Z)
	}
	return requested
}

func updateLocalWorldTOMLSpawn(worldURL string, pos rl.Vector3) (string, bool) {
	u, err := url.Parse(normalizeWorldURL(worldURL))
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return "", false
	}
	candidates := []string{
		filepath.Join(".", "world", "world.toml"),
		filepath.Join(".", "world.toml"),
	}
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		spawnLine := fmt.Sprintf("spawn = [%.2f, %.2f, %.2f]", pos.X, pos.Y, pos.Z)
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
				replaced = true
			} else if inWorld {
				lines = append(lines, spawnLine)
				replaced = true
			}
		}
		if replaced {
			tmp := p + ".tmp"
			if os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0644) == nil && os.Rename(tmp, p) == nil {
				return p, true
			}
		}
	}
	return "", false
}

func loadWorldFromURL(rawURL, cacheDir string) (*LoadedWorld, error) {
	rawURL = normalizeWorldURL(rawURL)
	if rawURL == "local://sandbox" {
		floorBytes := buildDefaultGLB(24.0, 0.2, 24.0)
		h := identity.FileSHA256(floorBytes)
		cached := filepath.Join(cacheDir, h+".glb")
		_ = os.WriteFile(cached, floorBytes, 0644)
		m := &WorldManifest{}
		m.World.Title = "Local Offline Sandbox"
		m.World.Spawn = []float32{0, 2.0, 10.0}
		m.Rules.PlacementMode = "ephemeral"
		m.Rules.MaxAvatarMB = 15
		m.Rules.MaxItemMB = 5
		return &LoadedWorld{URL: rawURL, Manifest: m, EnvCacheGLB: cached}, nil
	}

	manifest, baseWorldURL, err := fetchManifest(rawURL)
	if err != nil {
		return nil, err
	}
	if manifest.World.Title == "" {
		manifest.World.Title = "Untitled VektorWeb World"
	}
	manifest.World.Title = sanitizeDisplayText(manifest.World.Title, 64)

	envURL, err := resolveRelativeAssetURL(baseWorldURL, manifest.Assets.Environment)
	if err != nil {
		return nil, err
	}

	var envBytes []byte
	if manifest.Assets.EnvironmentSHA256 != "" && isValidHex64(manifest.Assets.EnvironmentSHA256) {
		cachedCandidate := filepath.Join(cacheDir, strings.ToLower(manifest.Assets.EnvironmentSHA256)+".glb")
		if b, err := os.ReadFile(cachedCandidate); err == nil {
			if strings.EqualFold(identity.FileSHA256(b), manifest.Assets.EnvironmentSHA256) {
				envBytes = b
			}
		}
	}

	if envBytes == nil {
		envBytes, err = fetchBytes(envURL, MaxDefaultEnvBytes)
		if err != nil {
			return nil, fmt.Errorf("failed fetching environment.glb: %w", err)
		}
	}

	actualHash := identity.FileSHA256(envBytes)
	if manifest.Assets.EnvironmentSHA256 != "" && !strings.EqualFold(actualHash, manifest.Assets.EnvironmentSHA256) {
		return nil, fmt.Errorf("SRI mismatch on environment.glb: got %s, want %s", actualHash, manifest.Assets.EnvironmentSHA256)
	}
	if err := glbguard.ValidateGLB(envBytes, MaxDefaultEnvBytes); err != nil {
		return nil, fmt.Errorf("glbguard rejected world geometry: %w", err)
	}
	cachedEnvPath := filepath.Join(cacheDir, actualHash+".glb")
	_ = os.WriteFile(cachedEnvPath, envBytes, 0644)

	var sandbox *luavm.WorldVM
	if manifest.Assets.Script != "" {
		luaURL, err := resolveRelativeAssetURL(baseWorldURL, manifest.Assets.Script)
		if err != nil {
			log.Printf("Warning: rejected world.lua path: %v", err)
		} else {
			luaBytes, err := fetchBytes(luaURL, MaxWorldScriptBytes)
			if err != nil {
				log.Printf("Warning: could not fetch world.lua: %v", err)
			} else {
				if manifest.Assets.ScriptSHA256 != "" && !strings.EqualFold(identity.FileSHA256(luaBytes), manifest.Assets.ScriptSHA256) {
					return nil, fmt.Errorf("SRI mismatch on world.lua")
				}
				sandbox, err = luavm.NewSandbox(string(luaBytes))
				if err != nil {
					log.Printf("Warning: world.lua rejected by sandbox: %v", err)
				}
			}
		}
	}

	return &LoadedWorld{
		URL:         rawURL,
		Manifest:    manifest,
		EnvCacheGLB: cachedEnvPath,
		Sandbox:     sandbox,
	}, nil
}

func startMicCapture(ctx context.Context, ns *NetState, pttActive *bool, mu *sync.Mutex) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "freebsd", "openbsd":
		if _, err := exec.LookPath("aucat"); err == nil {
			cmd = exec.CommandContext(ctx, "aucat", "-r", "16000", "-c", "1", "-e", "s16le", "-o", "-")
		}
	case "linux":
		if _, err := exec.LookPath("arecord"); err == nil {
			cmd = exec.CommandContext(ctx, "arecord", "-q", "-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "raw")
		} else if _, err := exec.LookPath("pw-record"); err == nil {
			cmd = exec.CommandContext(ctx, "pw-record", "--rate", "16000", "--channels", "1", "--format", "s16", "-")
		}
	}

	if cmd != nil {
		stdout, err := cmd.StdoutPipe()
		if err == nil && cmd.Start() == nil {
			go func() {
				defer func() { _ = cmd.Wait() }()
				frame := make([]byte, 2048)
				for {
					if _, err := io.ReadFull(stdout, frame); err != nil {
						return
					}
					mu.Lock()
					tx := *pttActive
					mu.Unlock()
					if tx {
						pkt := make([]byte, 1+len(frame))
						pkt[0] = 0x02
						copy(pkt[1:], frame)
						_ = ns.sendBinary(pkt)
					}
				}
			}()
			return
		}
	}

	go func() {
		phase := 0.0
		ticker := time.NewTicker(64 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mu.Lock()
				tx := *pttActive
				mu.Unlock()
				if tx {
					const samples = 1024
					pkt := make([]byte, 1+samples*2)
					pkt[0] = 0x02
					for i := 0; i < samples; i++ {
						s := int16(math.Sin(phase) * 2600)
						phase += 2.0 * math.Pi * 220.0 / 16000.0
						binary.LittleEndian.PutUint16(pkt[1+i*2:], uint16(s))
					}
					_ = ns.sendBinary(pkt)
				}
			}
		}
	}()
}

func licensePathForGLB(glbPath string) string {
	ext := filepath.Ext(glbPath)
	if ext != "" {
		return strings.TrimSuffix(glbPath, ext) + ".license.json"
	}
	return glbPath + ".license.json"
}

func scanInventoryFolder(folder, kind, cacheDir, localPubKey string, maxBytes int, localAssets map[string][]byte) []InventoryItem {
	var out []InventoryItem
	entries, err := os.ReadDir(folder)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".glb") {
			continue
		}
		fullPath := filepath.Join(folder, e.Name())
		b, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		if err := glbguard.ValidateGLB(b, maxBytes); err != nil {
			continue
		}
		h := identity.FileSHA256(b)

		var lic *identity.AssetLicense
		licSidecar := licensePathForGLB(fullPath)
		if l, err := identity.LoadAssetLicense(licSidecar); err == nil {
			lic = l
		} else if l, err := identity.LoadAssetLicense(filepath.Join(cacheDir, h+".license.json")); err == nil {
			lic = l
		}

		statusTag := ""
		canSend := true
		isCreator := true

		if lic != nil {
			if err := lic.Verify(h, localPubKey); err != nil {
				log.Printf("Skipping %s: invalid or unauthorized license (%v)", e.Name(), err)
				continue
			}
			isCreator = strings.EqualFold(lic.CreatorPub, localPubKey)
			if isCreator {
				statusTag = " [CREATOR]"
				canSend = true
			} else if lic.Transferable {
				statusTag = " [RESELLABLE]"
				canSend = true
			} else {
				statusTag = " [VENDOR-LOCKED]"
				canSend = false
			}
		}

		localAssets[h] = b
		_ = os.WriteFile(filepath.Join(cacheDir, h+".glb"), b, 0644)
		out = append(out, InventoryItem{
			Name:      e.Name(),
			Path:      fullPath,
			Hash:      h,
			Kind:      kind,
			License:   lic,
			StatusTag: statusTag,
			CanSend:   canSend,
			IsCreator: isCreator,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func resolveWalkMovement(cam *rl.Camera3D, headAnchor *rl.Vector3, spawnPos rl.Vector3, worldModel rl.Model, dt float32, mouseSens float32, velY *float32, grounded *bool, thirdPerson bool, thirdPersonDist float32) {
	const (
		moveSpeed  = 6.5
		sprintMult = 1.75
		eyeHeight  = 1.65
		gravity    = 22.0
		jumpSpeed  = 7.8
		wallRadius = 0.45
		stepHeight = 0.55
	)

	delta := rl.GetMouseDelta()
	forward := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))

	yaw := -delta.X * mouseSens
	pitch := -delta.Y * mouseSens

	cosY, sinY := float32(math.Cos(float64(yaw))), float32(math.Sin(float64(yaw)))
	fYaw := rl.NewVector3(
		forward.X*cosY+forward.Z*sinY,
		forward.Y,
		-forward.X*sinY+forward.Z*cosY,
	)

	newY := fYaw.Y + pitch
	if newY > 0.95 {
		newY = 0.95
	} else if newY < -0.95 {
		newY = -0.95
	}
	lookDir := rl.Vector3Normalize(rl.NewVector3(fYaw.X, newY, fYaw.Z))

	flatFwd := rl.Vector3Normalize(rl.NewVector3(lookDir.X, 0, lookDir.Z))
	flatRight := rl.Vector3Normalize(rl.Vector3CrossProduct(flatFwd, cam.Up))

	wish := rl.Vector3Zero()
	if rl.IsKeyDown(rl.KeyW) {
		wish = rl.Vector3Add(wish, flatFwd)
	}
	if rl.IsKeyDown(rl.KeyS) {
		wish = rl.Vector3Subtract(wish, flatFwd)
	}
	if rl.IsKeyDown(rl.KeyD) {
		wish = rl.Vector3Add(wish, flatRight)
	}
	if rl.IsKeyDown(rl.KeyA) {
		wish = rl.Vector3Subtract(wish, flatRight)
	}

	speed := float32(moveSpeed)
	if rl.IsKeyDown(rl.KeyLeftShift) {
		speed *= sprintMult
	}

	headPos := *headAnchor

	if rl.Vector3Length(wish) > 0.001 {
		wish = rl.Vector3Scale(rl.Vector3Normalize(wish), speed*dt)
		moveDir := rl.Vector3Normalize(wish)

		waistPos := rl.NewVector3(headPos.X, headPos.Y-eyeHeight+stepHeight, headPos.Z)
		wallRay := rl.NewRay(waistPos, moveDir)
		blocked := false
		for i := int32(0); i < worldModel.MeshCount; i++ {
			hit := rl.GetRayCollisionMesh(wallRay, worldModel.GetMeshes()[i], worldModel.Transform)
			if hit.Hit && hit.Distance < wallRadius+rl.Vector3Length(wish) {
				blocked = true
				break
			}
		}
		if !blocked {
			headPos.X += wish.X
			headPos.Z += wish.Z
		}
	}

	if *grounded && rl.IsKeyPressed(rl.KeySpace) {
		*velY = jumpSpeed
		*grounded = false
	}

	*velY -= gravity * dt
	headPos.Y += (*velY) * dt

	footY := headPos.Y - eyeHeight
	rayOrigin := rl.NewVector3(headPos.X, footY+stepHeight, headPos.Z)
	downRay := rl.NewRay(rayOrigin, rl.NewVector3(0, -1, 0))

	groundY := float32(-9999.0)
	if headPos.X >= -32.0 && headPos.X <= 32.0 && headPos.Z >= -32.0 && headPos.Z <= 32.0 {
		groundY = 0.0
	}

	for i := int32(0); i < worldModel.MeshCount; i++ {
		hit := rl.GetRayCollisionMesh(downRay, worldModel.GetMeshes()[i], worldModel.Transform)
		if hit.Hit && hit.Point.Y > groundY && hit.Distance <= stepHeight+0.45 {
			groundY = hit.Point.Y
		}
	}

	if groundY > -9000.0 && footY <= groundY && *velY <= 0 {
		headPos.Y = groundY + eyeHeight
		*velY = 0
		*grounded = true
	} else {
		*grounded = false
	}

	if headPos.Y < -25.0 {
		headPos = spawnPos
		*velY = 0
	}

	*headAnchor = headPos
	if thirdPerson {
		camBack := rl.Vector3Scale(lookDir, -1.0)
		camDist := thirdPersonDist
		camRay := rl.NewRay(headPos, camBack)
		for i := int32(0); i < worldModel.MeshCount; i++ {
			hit := rl.GetRayCollisionMesh(camRay, worldModel.GetMeshes()[i], worldModel.Transform)
			if hit.Hit && hit.Distance < camDist && hit.Distance > 0.35 {
				camDist = hit.Distance - 0.25
			}
		}
		cam.Position = rl.Vector3Add(headPos, rl.Vector3Scale(camBack, camDist))
		cam.Target = rl.Vector3Add(headPos, lookDir)
	} else {
		cam.Position = headPos
		cam.Target = rl.Vector3Add(headPos, lookDir)
	}
}

func sendTransferWhisper(ns *NetState, targetPub string, payload TransferWhisperPayload) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = ns.sendJSON(WireMsg{
		Type:      "CHAT",
		Channel:   "whisper",
		TargetPub: targetPub,
		Text:      transferWhisperPrefix + string(b),
	})
}

// buildFogShader creates a lightweight GLSL 330 distance-fog & hemisphere rim-light shader
// so 3D worlds have crisp depth perception without requiring external shader files.
func buildFogShader() (rl.Shader, int32, int32) {
	vs := `#version 330
in vec3 vertexPosition;
in vec2 vertexTexCoord;
in vec3 vertexNormal;
in vec4 vertexColor;

uniform mat4 mvp;
uniform mat4 matModel;

out vec2 fragTexCoord;
out vec4 fragColor;
out vec3 fragWorldPos;
out vec3 fragNormal;

void main() {
    fragTexCoord = vertexTexCoord;
    fragColor = vertexColor;
    vec4 wp = matModel * vec4(vertexPosition, 1.0);
    fragWorldPos = wp.xyz;
    fragNormal = normalize(mat3(matModel) * vertexNormal);
    gl_Position = mvp * vec4(vertexPosition, 1.0);
}
`
	fs := `#version 330
in vec2 fragTexCoord;
in vec4 fragColor;
in vec3 fragWorldPos;
in vec3 fragNormal;

uniform sampler2D texture0;
uniform vec4 colDiffuse;
uniform vec3 viewPos;
uniform vec4 fogColor;

out vec4 finalColor;

void main() {
    vec4 texel = texture(texture0, fragTexCoord);
    vec4 base = texel * colDiffuse * fragColor;
    if (base.a < 0.05) discard;

    vec3 n = normalize(fragNormal);
    if (length(fragNormal) < 0.01) n = vec3(0.0, 1.0, 0.0);

    // Subtle hemisphere sky/ground fill + directional key
    float hemi = dot(n, vec3(0.0, 1.0, 0.0)) * 0.5 + 0.5;
    float key = max(dot(n, normalize(vec3(0.45, 0.85, 0.35))), 0.0);
    vec3 lit = base.rgb * (0.58 + 0.24 * hemi + 0.28 * key);

    float dist = length(fragWorldPos - viewPos);
    float fogFactor = clamp((dist - 18.0) / 85.0, 0.0, 0.88);
    vec3 outRgb = mix(lit, fogColor.rgb, fogFactor);
    finalColor = vec4(outRgb, base.a);
}
`
	sh := rl.LoadShaderFromMemory(vs, fs)
	viewPosLoc := rl.GetShaderLocation(sh, "viewPos")
	fogColorLoc := rl.GetShaderLocation(sh, "fogColor")
	if fogColorLoc >= 0 {
		rl.SetShaderValue(sh, fogColorLoc, []float32{11.0 / 255.0, 14.0 / 255.0, 21.0 / 255.0, 1.0}, rl.ShaderUniformVec4)
	}
	return sh, viewPosLoc, fogColorLoc
}

func applyShaderToModel(m rl.Model, sh rl.Shader, enable bool) {
	if m.MeshCount <= 0 || m.MaterialCount <= 0 || m.Materials == nil {
		return
	}
	mats := unsafe.Slice(m.Materials, m.MaterialCount)
	for i := range mats {
		if enable && sh.ID > 0 {
			mats[i].Shader = sh
		} else {
			mats[i].Shader = rl.LoadShader("", "")
		}
	}
}

// glbHasAnimations inspects the glTF 2.0 JSON chunk of a .glb file to check
// whether any animation channels exist before calling rl.LoadModelAnimations.
// In raylib-go v0.63.0, LoadModelAnimations panics when the C function returns
// NULL / count == 0 for static .glb files that contain no animations.
func glbHasAnimations(glbPath string) bool {
	b, err := os.ReadFile(glbPath)
	if err != nil || len(b) < 20 {
		return false
	}
	// Verify magic "glTF" (0x46546C67)
	if binary.LittleEndian.Uint32(b[0:4]) != 0x46546C67 {
		return false
	}
	jsonLen := int(binary.LittleEndian.Uint32(b[12:16]))
	chunkType := binary.LittleEndian.Uint32(b[16:20])
	if chunkType != 0x4E4F534A || jsonLen <= 0 || 20+jsonLen > len(b) {
		return false
	}
	var doc struct {
		Animations []json.RawMessage `json:"animations"`
	}
	if err := json.Unmarshal(b[20:20+jsonLen], &doc); err != nil {
		return false
	}
	return len(doc.Animations) > 0
}

func loadModelWithAnims(glbPath string, sh rl.Shader, useFog bool) (rl.Model, *ModelAnimSet) {
	m := rl.LoadModel(glbPath)
	if useFog && sh.ID > 0 {
		applyShaderToModel(m, sh, true)
	}
	if glbHasAnimations(glbPath) {
		anims := rl.LoadModelAnimations(glbPath)
		if len(anims) > 0 {
			return m, &ModelAnimSet{Anims: anims, Frame: 0, LastUpdate: rl.GetTime()}
		}
	}
	return m, nil
}

func unloadModelAndAnims(m rl.Model, animSet *ModelAnimSet) {
	if animSet != nil && len(animSet.Anims) > 0 {
		rl.UnloadModelAnimations(animSet.Anims)
	}
	rl.UnloadModel(m)
}

func main() {
	worldURL := flag.String("world", "http://127.0.0.1:7777/world.toml", "URL or vektorweb:// link to world.toml")
	avatarFile := flag.String("avatar", "", "Optional path to local .glb avatar")
	propFile := flag.String("prop", "", "Optional path to local .glb prop")
	nickFlag := flag.String("nick", "", "Display nickname (defaults to pubkey prefix)")
	profileFlag := flag.String("profile", "", "Optional profile suffix (e.g. peer2 -> ~/.vektorweb-client-peer2)")
	noMicFlag := flag.Bool("nomic", false, "Disable microphone capture (useful for second local test client)")
	flag.Parse()

	targetWorld := *worldURL
	if flag.NArg() > 0 && flag.Arg(0) != "" {
		targetWorld = flag.Arg(0)
	}
	targetWorld = normalizeWorldURL(targetWorld)

	home, _ := os.UserHomeDir()
	dirName := ".vektorweb-client"
	if *profileFlag != "" {
		dirName = ".vektorweb-client-" + *profileFlag
	}
	baseDir := filepath.Join(home, dirName)
	cacheDir := filepath.Join(baseDir, "cache")
	avatarsDir := filepath.Join(baseDir, "avatars")
	itemsDir := filepath.Join(baseDir, "items")
	exportsDir := filepath.Join(baseDir, "exports")
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(avatarsDir, 0755)
	_ = os.MkdirAll(itemsDir, 0755)
	_ = os.MkdirAll(exportsDir, 0755)

	settingsPath := filepath.Join(baseDir, "settings.toml")
	cfg := loadClientSettings(settingsPath)

	kp, err := identity.LoadOrCreate(baseDir)
	if err != nil {
		log.Fatalf("identity error: %v", err)
	}
	nick := sanitizeDisplayText(strings.TrimSpace(*nickFlag), 24)
	if nick != "" {
		if cfg.Nick != nick {
			cfg.Nick = nick
			saveClientSettings(settingsPath, cfg)
		}
	} else if savedNick := sanitizeDisplayText(strings.TrimSpace(cfg.Nick), 24); savedNick != "" {
		nick = savedNick
	} else {
		nick = kp.PubHex()[:8]
	}

	defaultAvPath := filepath.Join(avatarsDir, "avatar.glb")
	if _, err := os.Stat(defaultAvPath); os.IsNotExist(err) {
		_ = os.WriteFile(defaultAvPath, buildDefaultGLB(0.6, 1.7, 0.4), 0644)
	}
	defaultPropPath := filepath.Join(itemsDir, "crate.glb")
	if _, err := os.Stat(defaultPropPath); os.IsNotExist(err) {
		_ = os.WriteFile(defaultPropPath, buildDefaultGLB(0.9, 0.9, 0.9), 0644)
	}

	dirPath := filepath.Join(baseDir, "worlds.toml")
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		defaultDir := fmt.Sprintf("[[worlds]]\nname = \"Local Relay World\"\nurl = %q\n\n[[worlds]]\nname = \"Offline Local Sandbox\"\nurl = \"local://sandbox\"\n", targetWorld)
		_ = os.WriteFile(dirPath, []byte(defaultDir), 0644)
	}
	directory := loadWorldDirectory(dirPath, targetWorld)

	activeWorld, err := loadWorldFromURL(targetWorld, cacheDir)
	if err != nil {
		log.Printf("Could not reach %s (%v) -> booting into local://sandbox", targetWorld, err)
		activeWorld, _ = loadWorldFromURL("local://sandbox", cacheDir)
	}

	maxAvMB := activeWorld.Manifest.Rules.MaxAvatarMB
	if maxAvMB <= 0 {
		maxAvMB = 15
	}
	maxItemMB := activeWorld.Manifest.Rules.MaxItemMB
	if maxItemMB <= 0 {
		maxItemMB = 5
	}

	localAssets := make(map[string][]byte)

	avatarInv := scanInventoryFolder(avatarsDir, "avatar", cacheDir, kp.PubHex(), maxAvMB*1024*1024, localAssets)
	propInv := scanInventoryFolder(itemsDir, "prop", cacheDir, kp.PubHex(), maxItemMB*1024*1024, localAssets)

	if *propFile != "" {
		if b, err := os.ReadFile(*propFile); err == nil {
			if err := glbguard.ValidateGLB(b, maxItemMB*1024*1024); err == nil {
				h := identity.FileSHA256(b)
				localAssets[h] = b
				_ = os.WriteFile(filepath.Join(cacheDir, h+".glb"), b, 0644)
				propInv = append([]InventoryItem{{
					Name:      filepath.Base(*propFile),
					Path:      *propFile,
					Hash:      h,
					Kind:      "prop",
					CanSend:   true,
					IsCreator: true,
				}}, propInv...)
			}
		}
	}

	avPath := *avatarFile
	if avPath == "" {
		avPath = defaultAvPath
	}
	localAvHash := "none"
	var localAvBytes []byte
	if b, err := os.ReadFile(avPath); err == nil {
		if err := glbguard.ValidateGLB(b, maxAvMB*1024*1024); err == nil {
			h := identity.FileSHA256(b)
			licOk := true
			if lic, err := identity.LoadAssetLicense(licensePathForGLB(avPath)); err == nil {
				if err := lic.Verify(h, kp.PubHex()); err != nil {
					log.Printf("Avatar license rejected: %v", err)
					licOk = false
				}
			}
			if licOk {
				localAvHash = h
				localAvBytes = b
				localAssets[localAvHash] = b
				_ = os.WriteFile(filepath.Join(cacheDir, localAvHash+".glb"), b, 0644)
			}
		}
	}

	propIdx := 0

	netState := &NetState{
		peers:            make(map[string]*RemotePeer),
		props:            make(map[string]*WorldProp),
		requested:        make(map[string]bool),
		assemblies:       make(map[string]*AssetAssembler),
		nick:             nick,
		blockedPeers:     make(map[string]bool),
		localPub:         kp.PubHex(),
		localAvHash:      localAvHash,
		localAvBytes:     localAvBytes,
		localAssets:      localAssets,
		cacheDir:         cacheDir,
		itemsDir:         itemsDir,
		avatarsDir:       avatarsDir,
		maxAssetB:        maxAvMB * 1024 * 1024,
		outgoingXfers:    make(map[string]*OutgoingTransfer),
		pendingRecvFiles: make(map[string]*PendingIncomingFile),
	}

	startRelayForWorld := func(w *LoadedWorld) {
		netState.resetSessionForPortal()
		if w.Manifest.World.RelayURL != "" {
			ctx, cancel := context.WithCancel(context.Background())
			netState.mu.Lock()
			netState.relayCancel = cancel
			netState.mu.Unlock()
			go runRelayLoop(ctx, w.Manifest.World.RelayURL, kp, netState)
		}
	}
	startRelayForWorld(activeWorld)

	rl.SetConfigFlags(rl.FlagWindowResizable | rl.FlagMsaa4xHint)
	rl.InitWindow(1280, 720, "VektorWeb - "+activeWorld.Manifest.World.Title)
	defer rl.CloseWindow()
	rl.SetTargetFPS(60)

	rl.InitAudioDevice()
	defer rl.CloseAudioDevice()
	rl.SetAudioStreamBufferSizeDefault(1024)
	voiceStream := rl.LoadAudioStream(16000, 16, 1)
	rl.PlayAudioStream(voiceStream)
	defer rl.UnloadAudioStream(voiceStream)

	var pttMu sync.Mutex
	pttActive := false
	micCtx, micCancel := context.WithCancel(context.Background())
	defer micCancel()
	if !*noMicFlag {
		startMicCapture(micCtx, netState, &pttActive, &pttMu)
	}

	fogShader, viewPosLoc, _ := buildFogShader()
	defaultShader := rl.LoadShader("", "")
	defer func() {
		if fogShader.ID > 0 {
			rl.UnloadShader(fogShader)
		}
	}()

	setModelFog := func(m rl.Model, enable bool) {
		if m.MeshCount <= 0 || m.MaterialCount <= 0 || m.Materials == nil {
			return
		}
		mats := unsafe.Slice(m.Materials, m.MaterialCount)
		for i := range mats {
			if enable && fogShader.ID > 0 {
				mats[i].Shader = fogShader
			} else {
				mats[i].Shader = defaultShader
			}
		}
	}

	worldModel, worldAnims := loadModelWithAnims(activeWorld.EnvCacheGLB, fogShader, cfg.EnableFog)
	defer func() {
		unloadModelAndAnims(worldModel, worldAnims)
		if activeWorld.Sandbox != nil {
			activeWorld.Sandbox.Close()
		}
	}()

	loadedModels := make(map[string]rl.Model)
	loadedAnims := make(map[string]*ModelAnimSet)
	for _, pe := range propInv {
		m, a := loadModelWithAnims(filepath.Join(cacheDir, pe.Hash+".glb"), fogShader, cfg.EnableFog)
		loadedModels[pe.Hash] = m
		if a != nil {
			loadedAnims[pe.Hash] = a
		}
	}
	for _, ae := range avatarInv {
		if _, ok := loadedModels[ae.Hash]; !ok {
			m, a := loadModelWithAnims(filepath.Join(cacheDir, ae.Hash+".glb"), fogShader, cfg.EnableFog)
			loadedModels[ae.Hash] = m
			if a != nil {
				loadedAnims[ae.Hash] = a
			}
		}
	}
	defer func() {
		for h, m := range loadedModels {
			unloadModelAndAnims(m, loadedAnims[h])
		}
	}()

	rawSpawn := rl.NewVector3(0, 1.65, 10)
	if len(activeWorld.Manifest.World.Spawn) == 3 {
		rawSpawn = rl.NewVector3(activeWorld.Manifest.World.Spawn[0], activeWorld.Manifest.World.Spawn[1], activeWorld.Manifest.World.Spawn[2])
	}
	spawn := safeSpawnPosition(worldModel, rawSpawn)

	playerHeadPos := spawn
	cam := rl.Camera3D{
		Position:   spawn,
		Target:     rl.NewVector3(spawn.X, spawn.Y, spawn.Z-1.0),
		Up:         rl.NewVector3(0, 1, 0),
		Fovy:       cfg.FOV,
		Projection: rl.CameraPerspective,
	}

	velY := float32(0.0)
	lastPortalHop := time.Time{}
	hopToWorld := func(targetURL string) {
		targetURL = normalizeWorldURL(targetURL)
		netState.addChat("[PORTAL]: Loading " + targetURL + " ...")
		nextW, err := loadWorldFromURL(targetURL, cacheDir)
		if err != nil {
			netState.addChat("[PORTAL ERR]: " + err.Error())
			return
		}
		unloadModelAndAnims(worldModel, worldAnims)
		if activeWorld.Sandbox != nil {
			activeWorld.Sandbox.Close()
		}
		activeWorld = nextW
		worldModel, worldAnims = loadModelWithAnims(activeWorld.EnvCacheGLB, fogShader, cfg.EnableFog)
		rl.SetWindowTitle("VektorWeb - " + activeWorld.Manifest.World.Title)

		newSpawn := rl.NewVector3(0, 1.65, 10)
		if len(activeWorld.Manifest.World.Spawn) == 3 {
			newSpawn = rl.NewVector3(activeWorld.Manifest.World.Spawn[0], activeWorld.Manifest.World.Spawn[1], activeWorld.Manifest.World.Spawn[2])
		}
		spawn = safeSpawnPosition(worldModel, newSpawn)
		playerHeadPos = spawn
		cam.Position = spawn
		cam.Target = rl.NewVector3(spawn.X, spawn.Y, spawn.Z-1.0)
		velY = 0
		lastPortalHop = time.Now()
		startRelayForWorld(activeWorld)
		netState.addChat("[PORTAL]: Arrived at " + activeWorld.Manifest.World.Title)
	}

	chatActive := false
	dirOpen := false
	helpOpen := false
	invOpen := false
	settingsOpen := false
	peersOpen := false
	invTab := "prop"
	invCursor := 0
	settingsCursor := 0
	peerCursor := 0

	transferModalOpen := false
	transferAllowResale := false
	transferPeerCursor := 0
	preselectedPeerPub := ""

	walkMode := true
	thirdPerson := false
	thirdPersonDist := float32(3.6)
	grounded := false
	ghostActive := false
	snapToGrid := false
	ghostYawOffset := float32(0)
	ghostYOffset := float32(0)
	ghostScale := float32(1.0)
	var pendingDrop *PendingDrop

	chatInput := ""
	var chatHistory []string
	chatHistIdx := -1
	chatDraft := ""
	luaHUD := ""
	lastSync := time.Now()
	lastJanitor := time.Now()
	var ownedPropIDs []string

	rl.SetExitKey(0)
	cursorLocked := true
	rl.DisableCursor()

	for !rl.WindowShouldClose() {
		sw := int32(rl.GetScreenWidth())
		sh := int32(rl.GetScreenHeight())

		netState.mu.Lock()
		hasIncomingOffer := len(netState.incomingXfers) > 0
		needInvRescan := netState.invDirty
		netState.invDirty = false
		var incomingSpawn []float32
		if len(netState.liveSpawn) == 3 {
			incomingSpawn = append([]float32(nil), netState.liveSpawn...)
			netState.liveSpawn = nil
		}
		netState.mu.Unlock()

		if len(incomingSpawn) == 3 {
			spawn = safeSpawnPosition(worldModel, rl.NewVector3(incomingSpawn[0], incomingSpawn[1], incomingSpawn[2]))
		}

		if needInvRescan {
			avatarInv = scanInventoryFolder(avatarsDir, "avatar", cacheDir, kp.PubHex(), maxAvMB*1024*1024, localAssets)
			propInv = scanInventoryFolder(itemsDir, "prop", cacheDir, kp.PubHex(), maxItemMB*1024*1024, localAssets)
		}

		anyOverlay := chatActive || invOpen || dirOpen || settingsOpen || peersOpen || helpOpen || hasIncomingOffer || pendingDrop != nil

		if !rl.IsWindowFocused() {
			rl.SetTargetFPS(15)
			if cursorLocked {
				rl.EnableCursor()
				cursorLocked = false
			}
		} else {
			rl.SetTargetFPS(60)
			if !anyOverlay {
				if rl.IsKeyPressed(rl.KeyEscape) {
					if ghostActive {
						ghostActive = false
						netState.addChat("[BUILD]: Exited build mode")
					} else if cursorLocked {
						rl.EnableCursor()
						cursorLocked = false
					}
				} else if rl.IsMouseButtonPressed(rl.MouseButtonLeft) && !cursorLocked {
					rl.DisableCursor()
					cursorLocked = true
				}
			} else if cursorLocked {
				rl.EnableCursor()
				cursorLocked = false
			}
		}

		dt := float64(rl.GetFrameTime())
		if dt > 0.1 {
			dt = 0.1
		}

		// Step embedded glTF animations (~30 fps keyframe advance)
		nowSec := rl.GetTime()
		stepAnim := func(m rl.Model, animSet *ModelAnimSet) {
			if animSet == nil || len(animSet.Anims) == 0 {
				return
			}
			anim := animSet.Anims[0]
			if anim.KeyframeCount <= 0 {
				return
			}
			if nowSec-animSet.LastUpdate >= 1.0/30.0 {
				animSet.LastUpdate = nowSec
				animSet.Frame = (animSet.Frame + 1) % anim.KeyframeCount
				rl.UpdateModelAnimation(m, anim, float32(animSet.Frame))
			}
		}
		stepAnim(worldModel, worldAnims)
		for h, animSet := range loadedAnims {
			if m, ok := loadedModels[h]; ok {
				stepAnim(m, animSet)
			}
		}

		// Periodic stale assembly & peer cleanup
		if time.Since(lastJanitor) >= 10*time.Second {
			lastJanitor = time.Now()
			netState.mu.Lock()
			for h, asm := range netState.assemblies {
				if time.Since(asm.CreatedAt) > 30*time.Second {
					delete(netState.assemblies, h)
					delete(netState.requested, h)
				}
			}
			for pub, p := range netState.peers {
				if time.Since(p.LastSeen) > 45*time.Second {
					delete(netState.peers, pub)
				}
			}
			netState.mu.Unlock()
		}

		// File drop handler (.glb)
		if rl.IsFileDropped() {
			dropped := rl.LoadDroppedFiles()
			for _, fpath := range dropped {
				if !strings.HasSuffix(strings.ToLower(fpath), ".glb") {
					netState.addChat("[DROP ERR]: Only .glb files are supported")
					continue
				}
				b, err := os.ReadFile(fpath)
				if err != nil {
					netState.addChat("[DROP ERR]: Could not read " + filepath.Base(fpath))
					continue
				}
				if err := glbguard.ValidateGLB(b, maxAvMB*1024*1024); err != nil {
					netState.addChat("[GLBGUARD]: " + err.Error())
					continue
				}
				h := identity.FileSHA256(b)
				if lic, err := identity.LoadAssetLicense(licensePathForGLB(fpath)); err == nil {
					if err := lic.Verify(h, kp.PubHex()); err != nil {
						netState.addChat("[LICENSE ERR]: " + err.Error())
						continue
					}
					_ = identity.SaveAssetLicense(filepath.Join(cacheDir, h+".license.json"), lic)
				}
				pendingDrop = &PendingDrop{
					FileName: sanitizeFileName(filepath.Base(fpath)),
					Hash:     h,
					Bytes:    b,
				}
				break
			}
			rl.UnloadDroppedFiles()
		}

		if pendingDrop != nil {
			if rl.IsKeyPressed(rl.KeyOne) {
				destP := filepath.Join(avatarsDir, pendingDrop.FileName)
				_ = os.WriteFile(destP, pendingDrop.Bytes, 0644)
				_ = os.WriteFile(defaultAvPath, pendingDrop.Bytes, 0644)
				cachedP := filepath.Join(cacheDir, pendingDrop.Hash+".glb")
				_ = os.WriteFile(cachedP, pendingDrop.Bytes, 0644)
				if lic, err := identity.LoadAssetLicense(filepath.Join(cacheDir, pendingDrop.Hash+".license.json")); err == nil {
					_ = identity.SaveAssetLicense(licensePathForGLB(destP), lic)
					_ = identity.SaveAssetLicense(licensePathForGLB(defaultAvPath), lic)
				}

				if _, exists := loadedModels[pendingDrop.Hash]; !exists {
					m, a := loadModelWithAnims(cachedP, fogShader, cfg.EnableFog)
					loadedModels[pendingDrop.Hash] = m
					if a != nil {
						loadedAnims[pendingDrop.Hash] = a
					}
				}

				netState.mu.Lock()
				netState.localAvHash = pendingDrop.Hash
				netState.localAvBytes = pendingDrop.Bytes
				netState.localAssets[pendingDrop.Hash] = pendingDrop.Bytes
				netState.mu.Unlock()
				localAvHash = pendingDrop.Hash

				avatarInv = scanInventoryFolder(avatarsDir, "avatar", cacheDir, kp.PubHex(), maxAvMB*1024*1024, localAssets)
				netState.addChat("[AVATAR]: Equipped " + pendingDrop.FileName + "!")
				pendingDrop = nil
			} else if rl.IsKeyPressed(rl.KeyTwo) {
				destP := filepath.Join(itemsDir, pendingDrop.FileName)
				_ = os.WriteFile(destP, pendingDrop.Bytes, 0644)
				cachedP := filepath.Join(cacheDir, pendingDrop.Hash+".glb")
				_ = os.WriteFile(cachedP, pendingDrop.Bytes, 0644)
				if lic, err := identity.LoadAssetLicense(filepath.Join(cacheDir, pendingDrop.Hash+".license.json")); err == nil {
					_ = identity.SaveAssetLicense(licensePathForGLB(destP), lic)
				}

				netState.mu.Lock()
				netState.localAssets[pendingDrop.Hash] = pendingDrop.Bytes
				netState.mu.Unlock()

				if _, exists := loadedModels[pendingDrop.Hash]; !exists {
					m, a := loadModelWithAnims(cachedP, fogShader, cfg.EnableFog)
					loadedModels[pendingDrop.Hash] = m
					if a != nil {
						loadedAnims[pendingDrop.Hash] = a
					}
				}
				propInv = scanInventoryFolder(itemsDir, "prop", cacheDir, kp.PubHex(), maxItemMB*1024*1024, localAssets)
				for i, it := range propInv {
					if it.Hash == pendingDrop.Hash {
						propIdx = i
						break
					}
				}
				ghostActive = true

				netState.addChat("[INVENTORY]: Added " + pendingDrop.FileName + " & armed Ghost [P]!")
				pendingDrop = nil
			} else if rl.IsKeyPressed(rl.KeyEscape) {
				pendingDrop = nil
			}
		}

		netState.mu.Lock()
		toLoad := append([]string(nil), netState.readyModels...)
		netState.readyModels = nil
		netState.localPos = playerHeadPos
		interpAlpha := float32(math.Min(1.0, dt*14.0))
		for _, p := range netState.peers {
			if p.HasPos {
				p.Pos.X += (p.TargetPos.X - p.Pos.X) * interpAlpha
				p.Pos.Y += (p.TargetPos.Y - p.Pos.Y) * interpAlpha
				p.Pos.Z += (p.TargetPos.Z - p.Pos.Z) * interpAlpha
				p.Yaw = lerpAngleDeg(p.Yaw, p.TargetYaw, interpAlpha)
			}
		}
		netState.mu.Unlock()

		for _, h := range toLoad {
			if _, exists := loadedModels[h]; !exists {
				p := filepath.Join(cacheDir, h+".glb")
				m, a := loadModelWithAnims(p, fogShader, cfg.EnableFog)
				loadedModels[h] = m
				if a != nil {
					loadedAnims[h] = a
				}
			}
		}

		if rl.IsAudioStreamProcessed(voiceStream) {
			netState.mu.Lock()
			var samples []float32
			if len(netState.voiceQueue) > 0 {
				samples = netState.voiceQueue[0]
				netState.voiceQueue = netState.voiceQueue[1:]
			}
			netState.mu.Unlock()

			if len(samples) > 0 {
				for i := range samples {
					s := samples[i] * cfg.VoiceVolume
					if s > 1.0 {
						s = 1.0
					} else if s < -1.0 {
						s = -1.0
					}
					samples[i] = s
				}
				rl.UpdateAudioStream(voiceStream, samples)
			}
		}

		if activeWorld.Sandbox != nil {
			actions, hud := activeWorld.Sandbox.Tick(dt, playerHeadPos.X, playerHeadPos.Y, playerHeadPos.Z)
			luaHUD = sanitizeDisplayText(hud, 96)
			for _, act := range actions {
				switch act.Type {
				case "NOTIFY":
					netState.addChat("[WORLD]: " + act.Text)
				case "TELEPORT":
					offset := rl.Vector3Subtract(cam.Target, cam.Position)
					playerHeadPos = rl.NewVector3(act.Pos[0], act.Pos[1], act.Pos[2])
					cam.Position = playerHeadPos
					cam.Target = rl.Vector3Add(cam.Position, offset)
				case "PORTAL":
					if act.WorldURL != "" && normalizeWorldURL(act.WorldURL) != activeWorld.URL {
						hopToWorld(act.WorldURL)
					}
				}
			}
		} else {
			luaHUD = ""
		}

		// Handle incoming client-to-client transfer offer prompt ([Y] Accept / [N] Decline)
		if hasIncomingOffer && pendingDrop == nil {
			netState.mu.Lock()
			offer := netState.incomingXfers[0]
			netState.mu.Unlock()

			if rl.IsKeyPressed(rl.KeyY) {
				netState.mu.Lock()
				netState.incomingXfers = netState.incomingXfers[1:]
				netState.pendingRecvFiles[offer.Hash] = &PendingIncomingFile{
					FileName:  offer.FileName,
					AssetKind: offer.AssetKind,
					License:   offer.License,
					FromPub:   offer.FromPub,
				}
				netState.mu.Unlock()

				sendTransferWhisper(netState, offer.FromPub, TransferWhisperPayload{
					Action:     "ACCEPT",
					TransferID: offer.TransferID,
					AssetKind:  offer.AssetKind,
					FileName:   offer.FileName,
					Hash:       offer.Hash,
					License:    offer.License,
				})
				_ = netState.sendJSON(WireMsg{
					Type:      "REQ_ASSET",
					TargetPub: offer.FromPub,
					Hash:      offer.Hash,
				})
				netState.addChat(fmt.Sprintf("[TRANSFER]: Accepted %s from %s, downloading...", offer.FileName, offer.FromNick))
			} else if rl.IsKeyPressed(rl.KeyN) || rl.IsKeyPressed(rl.KeyEscape) {
				netState.mu.Lock()
				netState.incomingXfers = netState.incomingXfers[1:]
				netState.mu.Unlock()

				sendTransferWhisper(netState, offer.FromPub, TransferWhisperPayload{
					Action:     "DECLINE",
					TransferID: offer.TransferID,
					Hash:       offer.Hash,
				})
				netState.addChat("[TRANSFER]: Declined transfer offer.")
			}
		}

		if pendingDrop == nil && !hasIncomingOffer && !chatActive {
			if rl.IsKeyPressed(rl.KeyF1) {
				helpOpen = !helpOpen
				if helpOpen {
					invOpen, dirOpen, settingsOpen, peersOpen, transferModalOpen = false, false, false, false, false
				}
			}
			if rl.IsKeyPressed(rl.KeyF2) {
				settingsOpen = !settingsOpen
				if settingsOpen {
					invOpen, dirOpen, helpOpen, peersOpen, transferModalOpen = false, false, false, false, false
					settingsCursor = 0
				}
			}
			if rl.IsKeyPressed(rl.KeyF3) {
				peersOpen = !peersOpen
				if peersOpen {
					invOpen, dirOpen, helpOpen, settingsOpen, transferModalOpen = false, false, false, false, false
					peerCursor = 0
				}
			}
			if rl.IsKeyPressed(rl.KeyF12) {
				shotName := fmt.Sprintf("vektorweb_%s.png", time.Now().Format("20060102_150405"))
				shotPath := filepath.Join(exportsDir, shotName)
				rl.TakeScreenshot(shotPath)
				netState.addChat("[SCREENSHOT]: Saved " + shotName)
			}
			if rl.IsKeyPressed(rl.KeyI) {
				invOpen = !invOpen
				transferModalOpen = false
				preselectedPeerPub = ""
				dirOpen, helpOpen, settingsOpen, peersOpen = false, false, false, false
				if invOpen {
					avatarInv = scanInventoryFolder(avatarsDir, "avatar", cacheDir, kp.PubHex(), maxAvMB*1024*1024, localAssets)
					propInv = scanInventoryFolder(itemsDir, "prop", cacheDir, kp.PubHex(), maxItemMB*1024*1024, localAssets)
					invCursor = 0
				}
			}
			if rl.IsKeyPressed(rl.KeyTab) {
				dirOpen = !dirOpen
				invOpen, transferModalOpen, helpOpen, settingsOpen, peersOpen = false, false, false, false, false
			}
		}

		forward := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))
		ghostPos := rl.Vector3Add(playerHeadPos, rl.Vector3Scale(forward, 3.2))
		ghostPos.Y = ghostYOffset
		ghostYaw := float32(math.Atan2(float64(forward.X), float64(forward.Z))*(180.0/math.Pi)) + ghostYawOffset

		if snapToGrid {
			ghostPos.X = float32(math.Round(float64(ghostPos.X)))
			ghostPos.Y = float32(math.Round(float64(ghostPos.Y)*2.0) / 2.0)
			ghostPos.Z = float32(math.Round(float64(ghostPos.Z)))
			ghostYaw = float32(math.Round(float64(ghostYaw)/45.0) * 45.0)
		}

		// Raycast look-at selection (within 8m) for placed props or nearby peers
		var lookedAt *RayTarget
		if !anyOverlay && !ghostActive {
			lookRay := rl.NewRay(cam.Position, forward)
			bestDist := float32(8.0)

			netState.mu.RLock()
			for _, prop := range netState.props {
				rad := float32(0.55)
				if prop.Scale > 0 {
					rad *= prop.Scale
				}
				box := rl.NewBoundingBox(
					rl.NewVector3(prop.Pos.X-rad, prop.Pos.Y, prop.Pos.Z-rad),
					rl.NewVector3(prop.Pos.X+rad, prop.Pos.Y+rad*2.0, prop.Pos.Z+rad),
				)
				hit := rl.GetRayCollisionBox(lookRay, box)
				if hit.Hit && hit.Distance < bestDist {
					bestDist = hit.Distance
					canDel := strings.EqualFold(prop.Owner, kp.PubHex())
					ownerLabel := prop.Owner
					if len(ownerLabel) > 8 {
						ownerLabel = ownerLabel[:8]
					}
					if p, ok := netState.peers[prop.Owner]; ok && p.Nick != "" {
						ownerLabel = p.Nick
					} else if canDel {
						ownerLabel = "You"
					}
					sub := "Owner: " + ownerLabel
					if canDel {
						sub += "  |  [Del] Remove Prop"
					}
					lookedAt = &RayTarget{
						Kind:      "prop",
						ID:        prop.PropID,
						Label:     "Prop " + prop.PropID[:minInt(6, len(prop.PropID))],
						SubLabel:  sub,
						CanDelete: canDel,
						Box:       box,
					}
				}
			}

			for _, peer := range netState.peers {
				if time.Since(peer.LastSeen) > 10*time.Second || netState.blockedPeers[peer.PubKey] {
					continue
				}
				box := rl.NewBoundingBox(
					rl.NewVector3(peer.Pos.X-0.45, peer.Pos.Y-1.65, peer.Pos.Z-0.45),
					rl.NewVector3(peer.Pos.X+0.45, peer.Pos.Y+0.35, peer.Pos.Z+0.45),
				)
				hit := rl.GetRayCollisionBox(lookRay, box)
				if hit.Hit && hit.Distance < bestDist {
					bestDist = hit.Distance
					lookedAt = &RayTarget{
						Kind:     "peer",
						ID:       peer.PubKey,
						Label:    formatPeerTag(peer.Nick, peer.PubKey),
						SubLabel: "[E] Offer Trade/Item",
						Box:      box,
					}
				}
			}
			netState.mu.RUnlock()
		}

		var activeProp InventoryItem
		if len(propInv) > 0 {
			activeProp = propInv[propIdx%len(propInv)]
		}

		if pendingDrop != nil || hasIncomingOffer {
		} else if helpOpen {
			if rl.IsKeyPressed(rl.KeyEscape) {
				helpOpen = false
			}
		} else if settingsOpen {
			// 0: FOV, 1: Mouse Sens, 2: Voice Volume, 3: Mini-Radar, 4: Nameplates, 5: Crosshair, 6: Distance Fog
			if rl.IsKeyPressed(rl.KeyDown) || rl.IsKeyPressed(rl.KeyS) {
				settingsCursor = (settingsCursor + 1) % 7
			}
			if rl.IsKeyPressed(rl.KeyUp) || rl.IsKeyPressed(rl.KeyW) {
				settingsCursor = (settingsCursor + 6) % 7
			}
			left := rl.IsKeyPressed(rl.KeyLeft) || rl.IsKeyPressed(rl.KeyA)
			right := rl.IsKeyPressed(rl.KeyRight) || rl.IsKeyPressed(rl.KeyD) || rl.IsKeyPressed(rl.KeyEnter)
			if left || right {
				dir := float32(1)
				if left {
					dir = -1
				}
				switch settingsCursor {
				case 0:
					cfg.FOV = clampFloat32(cfg.FOV+dir*4.0, 50, 110)
					cam.Fovy = cfg.FOV
				case 1:
					cfg.MouseSens = clampFloat32(cfg.MouseSens+dir*0.0005, 0.0005, 0.015)
				case 2:
					cfg.VoiceVolume = clampFloat32(cfg.VoiceVolume+dir*0.1, 0.0, 2.0)
				case 3:
					cfg.ShowRadar = !cfg.ShowRadar
				case 4:
					cfg.ShowTags = !cfg.ShowTags
				case 5:
					cfg.ShowCross = !cfg.ShowCross
				case 6:
					cfg.EnableFog = !cfg.EnableFog
					setModelFog(worldModel, cfg.EnableFog)
					for _, m := range loadedModels {
						setModelFog(m, cfg.EnableFog)
					}
				}
				saveClientSettings(settingsPath, cfg)
			}
			if rl.IsKeyPressed(rl.KeyEscape) {
				settingsOpen = false
			}
		} else if peersOpen {
			netState.mu.RLock()
			var onlinePeers []*RemotePeer
			for _, p := range netState.peers {
				if time.Since(p.LastSeen) <= 15*time.Second {
					onlinePeers = append(onlinePeers, p)
				}
			}
			netState.mu.RUnlock()

			if rl.IsKeyPressed(rl.KeyDown) || rl.IsKeyPressed(rl.KeyS) {
				if len(onlinePeers) > 0 {
					peerCursor = (peerCursor + 1) % len(onlinePeers)
				}
			}
			if rl.IsKeyPressed(rl.KeyUp) || rl.IsKeyPressed(rl.KeyW) {
				if len(onlinePeers) > 0 {
					peerCursor = (peerCursor - 1 + len(onlinePeers)) % len(onlinePeers)
				}
			}
			if rl.IsKeyPressed(rl.KeyB) && len(onlinePeers) > 0 {
				target := onlinePeers[peerCursor%len(onlinePeers)]
				netState.mu.Lock()
				blocked := !netState.blockedPeers[target.PubKey]
				if blocked {
					netState.blockedPeers[target.PubKey] = true
				} else {
					delete(netState.blockedPeers, target.PubKey)
				}
				netState.mu.Unlock()
				if blocked {
					netState.addChat("[BLOCK]: Muted/hid " + target.Nick)
				} else {
					netState.addChat("[BLOCK]: Unblocked " + target.Nick)
				}
			}
			if rl.IsKeyPressed(rl.KeyEnter) && len(onlinePeers) > 0 {
				target := onlinePeers[peerCursor%len(onlinePeers)]
				offset := rl.Vector3Subtract(cam.Target, cam.Position)
				playerHeadPos = rl.NewVector3(target.Pos.X+1.5, target.Pos.Y, target.Pos.Z+1.5)
				cam.Position = playerHeadPos
				cam.Target = rl.Vector3Add(cam.Position, offset)
				peersOpen = false
				netState.addChat("[TELEPORT]: Warped to " + target.Nick)
			}
			if rl.IsKeyPressed(rl.KeyEscape) {
				peersOpen = false
			}
		} else if invOpen {
			activeList := propInv
			if invTab == "avatar" {
				activeList = avatarInv
			}

			if transferModalOpen {
				netState.mu.RLock()
				var onlinePeers []*RemotePeer
				for _, p := range netState.peers {
					if time.Since(p.LastSeen) <= 15*time.Second {
						onlinePeers = append(onlinePeers, p)
					}
				}
				netState.mu.RUnlock()

				if preselectedPeerPub != "" {
					for i, p := range onlinePeers {
						if p.PubKey == preselectedPeerPub {
							transferPeerCursor = i
							break
						}
					}
					preselectedPeerPub = ""
				}

				if rl.IsKeyPressed(rl.KeyDown) || rl.IsKeyPressed(rl.KeyS) {
					if len(onlinePeers) > 0 {
						transferPeerCursor = (transferPeerCursor + 1) % len(onlinePeers)
					}
				}
				if rl.IsKeyPressed(rl.KeyUp) || rl.IsKeyPressed(rl.KeyW) {
					if len(onlinePeers) > 0 {
						transferPeerCursor = (transferPeerCursor - 1 + len(onlinePeers)) % len(onlinePeers)
					}
				}
				if rl.IsKeyPressed(rl.KeyL) {
					transferAllowResale = !transferAllowResale
				}
				if rl.IsKeyPressed(rl.KeyEnter) && len(onlinePeers) > 0 && len(activeList) > 0 {
					chosen := activeList[invCursor%len(activeList)]
					recipient := onlinePeers[transferPeerCursor%len(onlinePeers)]

					if !chosen.CanSend {
						netState.addChat("[LICENSE]: Item is vendor-locked (non-transferable)!")
					} else {
						var lic *identity.AssetLicense
						var err error
						if chosen.IsCreator {
							lic, err = kp.MintAssetLicense(chosen.Hash, recipient.PubKey, transferAllowResale)
						} else if chosen.License != nil && chosen.License.Transferable {
							lic, err = kp.MintAssetLicense(chosen.Hash, recipient.PubKey, true)
						}
						if err != nil || lic == nil {
							netState.addChat("[TRANSFER ERR]: Failed signing AssetLicense")
						} else {
							tidBytes := make([]byte, 8)
							_, _ = rand.Read(tidBytes)
							tid := "xfer_" + hex.EncodeToString(tidBytes)

							netState.mu.Lock()
							netState.outgoingXfers[tid] = &OutgoingTransfer{
								TransferID: tid,
								TargetPub:  recipient.PubKey,
								AssetKind:  chosen.Kind,
								FilePath:   chosen.Path,
								FileName:   chosen.Name,
								Hash:       chosen.Hash,
								License:    lic,
							}
							netState.mu.Unlock()

							sendTransferWhisper(netState, recipient.PubKey, TransferWhisperPayload{
								Action:     "OFFER",
								TransferID: tid,
								AssetKind:  chosen.Kind,
								FileName:   chosen.Name,
								Hash:       chosen.Hash,
								License:    lic,
							})
							modeStr := "VENDOR-LOCKED"
							if transferAllowResale {
								modeStr = "RESELLABLE"
							}
							netState.addChat(fmt.Sprintf("[TRANSFER]: Sent %s license offer for %s to %s", modeStr, chosen.Name, formatPeerTag(recipient.Nick, recipient.PubKey)))
							transferModalOpen = false
						}
					}
				} else if rl.IsKeyPressed(rl.KeyEscape) {
					transferModalOpen = false
				}
			} else {
				if rl.IsKeyPressed(rl.KeyOne) {
					invTab = "prop"
					invCursor = 0
				} else if rl.IsKeyPressed(rl.KeyTwo) {
					invTab = "avatar"
					invCursor = 0
				}

				if rl.IsKeyPressed(rl.KeyDown) || rl.IsKeyPressed(rl.KeyS) {
					if len(activeList) > 0 {
						invCursor = (invCursor + 1) % len(activeList)
					}
				}
				if rl.IsKeyPressed(rl.KeyUp) || rl.IsKeyPressed(rl.KeyW) {
					if len(activeList) > 0 {
						invCursor = (invCursor - 1 + len(activeList)) % len(activeList)
					}
				}
				if rl.IsKeyPressed(rl.KeyR) && len(activeList) > 0 {
					chosen := activeList[invCursor%len(activeList)]
					if !chosen.CanSend {
						netState.addChat("[LICENSE]: Cannot transfer — asset is vendor-locked by creator!")
					} else {
						transferModalOpen = true
						transferPeerCursor = 0
						transferAllowResale = false
					}
				}
				if rl.IsKeyPressed(rl.KeyEnter) && len(activeList) > 0 {
					chosen := activeList[invCursor%len(activeList)]
					if invTab == "avatar" {
						if _, ok := loadedModels[chosen.Hash]; !ok {
							m, a := loadModelWithAnims(filepath.Join(cacheDir, chosen.Hash+".glb"), fogShader, cfg.EnableFog)
							loadedModels[chosen.Hash] = m
							if a != nil {
								loadedAnims[chosen.Hash] = a
							}
						}
						netState.mu.Lock()
						netState.localAvHash = chosen.Hash
						netState.localAvBytes = localAssets[chosen.Hash]
						netState.mu.Unlock()
						localAvHash = chosen.Hash
						netState.addChat("[AVATAR]: Equipped " + chosen.Name)
					} else {
						propIdx = invCursor % len(propInv)
						if _, ok := loadedModels[chosen.Hash]; !ok {
							m, a := loadModelWithAnims(filepath.Join(cacheDir, chosen.Hash+".glb"), fogShader, cfg.EnableFog)
							loadedModels[chosen.Hash] = m
							if a != nil {
								loadedAnims[chosen.Hash] = a
							}
						}
						ghostActive = true
						netState.addChat("[INVENTORY]: Selected " + chosen.Name + " (Press [P] to place)")
					}
					invOpen = false
				} else if rl.IsKeyPressed(rl.KeyEscape) {
					invOpen = false
				}
			}
		} else if dirOpen {
			for i := 0; i < len(directory.Worlds) && i < 9; i++ {
				if rl.IsKeyPressed(int32(rl.KeyOne + i)) {
					dirOpen = false
					hopToWorld(directory.Worlds[i].URL)
					break
				}
			}
			if rl.IsKeyPressed(rl.KeyEscape) {
				dirOpen = false
			}
		} else if !chatActive {
			if rl.IsKeyPressed(rl.KeyT) {
				chatActive = true
				chatHistIdx = len(chatHistory)
				chatDraft = ""
			} else if rl.IsKeyPressed(rl.KeySlash) {
				chatActive = true
				chatInput = "/"
				chatHistIdx = len(chatHistory)
				chatDraft = "/"
			} else {
				if rl.IsKeyPressed(rl.KeyF) {
					walkMode = !walkMode
					velY = 0
					if walkMode {
						playerHeadPos = cam.Position
						netState.addChat("[MOVE]: Walk + Ground Collision ON")
					} else {
						if thirdPerson {
							fwdNow := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))
							cam.Position = playerHeadPos
							cam.Target = rl.Vector3Add(playerHeadPos, fwdNow)
							thirdPerson = false
						}
						netState.addChat("[MOVE]: Free-Fly Camera ON")
					}
				}
				if rl.IsKeyPressed(rl.KeyC) && walkMode {
					fwdNow := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))
					thirdPerson = !thirdPerson
					if thirdPerson {
						camBack := rl.Vector3Scale(fwdNow, -1.0)
						camDist := thirdPersonDist
						camRay := rl.NewRay(playerHeadPos, camBack)
						for i := int32(0); i < worldModel.MeshCount; i++ {
							hit := rl.GetRayCollisionMesh(camRay, worldModel.GetMeshes()[i], worldModel.Transform)
							if hit.Hit && hit.Distance < camDist && hit.Distance > 0.35 {
								camDist = hit.Distance - 0.25
							}
						}
						cam.Position = rl.Vector3Add(playerHeadPos, rl.Vector3Scale(camBack, camDist))
						cam.Target = rl.Vector3Add(playerHeadPos, fwdNow)
						netState.addChat("[CAMERA]: 3rd-Person View ON (Scroll wheel zooms)")
					} else {
						cam.Position = playerHeadPos
						cam.Target = rl.Vector3Add(playerHeadPos, fwdNow)
						netState.addChat("[CAMERA]: 1st-Person View ON")
					}
				}
				if thirdPerson {
					wheel := rl.GetMouseWheelMove()
					if wheel != 0 {
						thirdPersonDist = clampFloat32(thirdPersonDist-wheel*0.45, 1.5, 9.0)
					}
				}
				if rl.IsKeyPressed(rl.KeyR) && !ghostActive {
					playerHeadPos = spawn
					cam.Position = spawn
					cam.Target = rl.NewVector3(spawn.X, spawn.Y, spawn.Z-1.0)
					velY = 0
					netState.addChat("[SPAWN]: Returned to world spawn point")
				}

				// Look-at interaction: [Delete] removes your targeted prop; [E] opens trade for targeted peer
				if lookedAt != nil {
					if lookedAt.Kind == "prop" && lookedAt.CanDelete && rl.IsKeyPressed(rl.KeyDelete) {
						netState.mu.Lock()
						delete(netState.props, lookedAt.ID)
						netState.mu.Unlock()
						_ = netState.sendJSON(WireMsg{Type: "PROP_REMOVE", PropID: lookedAt.ID})
						netState.addChat("[PROP]: Removed " + lookedAt.Label)
					} else if lookedAt.Kind == "peer" && !ghostActive && rl.IsKeyPressed(rl.KeyE) {
						avatarInv = scanInventoryFolder(avatarsDir, "avatar", cacheDir, kp.PubHex(), maxAvMB*1024*1024, localAssets)
						propInv = scanInventoryFolder(itemsDir, "prop", cacheDir, kp.PubHex(), maxItemMB*1024*1024, localAssets)
						invOpen = true
						invCursor = 0
						transferModalOpen = true
						preselectedPeerPub = lookedAt.ID
					}
				}

				if cursorLocked {
					if walkMode {
						resolveWalkMovement(&cam, &playerHeadPos, spawn, worldModel, float32(dt), cfg.MouseSens, &velY, &grounded, thirdPerson, thirdPersonDist)
					} else {
						rl.UpdateCamera(&cam, rl.CameraFree)
						playerHeadPos = cam.Position
					}
				} else if thirdPerson && walkMode {
					// Keep 3rd-person camera orbit updated even while cursor is unlocked (e.g. [Esc] or [I] Inventory)
					fwdNow := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))
					camBack := rl.Vector3Scale(fwdNow, -1.0)
					camDist := thirdPersonDist
					camRay := rl.NewRay(playerHeadPos, camBack)
					for i := int32(0); i < worldModel.MeshCount; i++ {
						hit := rl.GetRayCollisionMesh(camRay, worldModel.GetMeshes()[i], worldModel.Transform)
						if hit.Hit && hit.Distance < camDist && hit.Distance > 0.35 {
							camDist = hit.Distance - 0.25
						}
					}
					cam.Position = rl.Vector3Add(playerHeadPos, rl.Vector3Scale(camBack, camDist))
					cam.Target = rl.Vector3Add(playerHeadPos, fwdNow)
				}

				if rl.IsKeyPressed(rl.KeyG) && activeProp.Hash != "" {
					ghostActive = !ghostActive
					if ghostActive {
						netState.addChat("[BUILD]: Entered build mode ([G] or [Esc] to exit)")
					} else {
						netState.addChat("[BUILD]: Exited build mode")
					}
				}
				if rl.IsKeyPressed(rl.KeyB) {
					snapToGrid = !snapToGrid
				}
				if rl.IsKeyPressed(rl.KeyN) && len(propInv) > 1 {
					propIdx = (propIdx + 1) % len(propInv)
				}
				if rl.IsKeyPressed(rl.KeyM) {
					netState.mu.Lock()
					netState.muted = !netState.muted
					m := netState.muted
					netState.mu.Unlock()
					if m {
						netState.addChat("[AUDIO]: Incoming voice muted")
					} else {
						netState.addChat("[AUDIO]: Incoming voice unmuted")
					}
				}

				if ghostActive {
					if rl.IsKeyPressed(rl.KeyR) {
						ghostYawOffset = 0
						ghostYOffset = 0
						ghostScale = 1.0
						netState.addChat("[BUILD]: Reset prop rotation, height, and scale")
					}
					if rl.IsKeyPressed(rl.KeyF4) {
						spawn = safeSpawnPosition(worldModel, playerHeadPos)
						_ = netState.sendJSON(WireMsg{
							Type:    "CHAT",
							Channel: "global",
							Text:    fmt.Sprintf("/setspawn %.2f %.2f %.2f", spawn.X, spawn.Y, spawn.Z),
						})
						if savedPath, ok := updateLocalWorldTOMLSpawn(activeWorld.URL, spawn); ok {
							netState.addChat(fmt.Sprintf("[SPAWN]: Updated %s -> [%.2f, %.2f, %.2f]", savedPath, spawn.X, spawn.Y, spawn.Z))
						} else {
							netState.addChat(fmt.Sprintf("[SPAWN]: Set local/requested spawn to [%.2f, %.2f, %.2f]", spawn.X, spawn.Y, spawn.Z))
						}
					}
					if rl.IsKeyDown(rl.KeyQ) {
						ghostYawOffset -= 90.0 * float32(dt)
					}
					if rl.IsKeyDown(rl.KeyE) {
						ghostYawOffset += 90.0 * float32(dt)
					}
					if rl.IsKeyDown(rl.KeyPageUp) {
						ghostYOffset += 1.5 * float32(dt)
					}
					if rl.IsKeyDown(rl.KeyPageDown) {
						ghostYOffset -= 1.5 * float32(dt)
					}
					if rl.IsKeyPressed(rl.KeyEqual) {
						ghostScale = clampFloat32(ghostScale+0.25, 0.25, 10.0)
					}
					if rl.IsKeyPressed(rl.KeyMinus) {
						ghostScale = clampFloat32(ghostScale-0.25, 0.25, 10.0)
					}
				}

				if rl.IsKeyPressed(rl.KeyP) && activeProp.Hash != "" && activeWorld.Manifest.Rules.PlacementMode != "forbid" {
					idBytes := make([]byte, 8)
					_, _ = rand.Read(idBytes)
					propID := hex.EncodeToString(idBytes)

					wp := &WorldProp{
						PropID: propID,
						Owner:  kp.PubHex(),
						Hash:   activeProp.Hash,
						Pos:    ghostPos,
						Yaw:    ghostYaw,
						Scale:  ghostScale,
					}
					netState.mu.Lock()
					netState.props[propID] = wp
					netState.mu.Unlock()
					ownedPropIDs = append(ownedPropIDs, propID)

					_ = netState.sendJSON(WireMsg{
						Type:   "PROP_SPAWN",
						PropID: propID,
						Hash:   activeProp.Hash,
						Pos:    []float32{ghostPos.X, ghostPos.Y, ghostPos.Z},
						Yaw:    ghostYaw,
						Scale:  ghostScale,
					})
					netState.addChat(fmt.Sprintf("[PROP]: Placed %s (%s...)", activeProp.Name, propID[:6]))
				}

				if rl.IsKeyPressed(rl.KeyX) && len(ownedPropIDs) > 0 {
					lastID := ownedPropIDs[len(ownedPropIDs)-1]
					ownedPropIDs = ownedPropIDs[:len(ownedPropIDs)-1]
					netState.mu.Lock()
					delete(netState.props, lastID)
					netState.mu.Unlock()
					_ = netState.sendJSON(WireMsg{Type: "PROP_REMOVE", PropID: lastID})
				}

				pttMu.Lock()
				pttActive = rl.IsKeyDown(rl.KeyV)
				pttMu.Unlock()
			}
		} else {
			pttMu.Lock()
			pttActive = false
			pttMu.Unlock()

			// Ctrl+V clipboard paste into chat input
			if (rl.IsKeyDown(rl.KeyLeftControl) || rl.IsKeyDown(rl.KeyRightControl)) && rl.IsKeyPressed(rl.KeyV) {
				clip := sanitizeDisplayText(rl.GetClipboardText(), 240)
				if clip != "" {
					rem := 240 - len([]rune(chatInput))
					if rem > 0 {
						clipRunes := []rune(clip)
						if len(clipRunes) > rem {
							clipRunes = clipRunes[:rem]
						}
						chatInput += string(clipRunes)
					}
				}
			}
			// Up/Down arrow history recall
			if rl.IsKeyPressed(rl.KeyUp) && len(chatHistory) > 0 {
				if chatHistIdx == len(chatHistory) {
					chatDraft = chatInput
				}
				if chatHistIdx > 0 {
					chatHistIdx--
					chatInput = chatHistory[chatHistIdx]
				}
			} else if rl.IsKeyPressed(rl.KeyDown) && len(chatHistory) > 0 {
				if chatHistIdx < len(chatHistory)-1 {
					chatHistIdx++
					chatInput = chatHistory[chatHistIdx]
				} else if chatHistIdx == len(chatHistory)-1 {
					chatHistIdx = len(chatHistory)
					chatInput = chatDraft
				}
			}
			for key := rl.GetCharPressed(); key > 0; key = rl.GetCharPressed() {
				if key >= 32 && key <= 126 && len([]rune(chatInput)) < 240 {
					chatInput += string(rune(key))
				}
			}
			if (rl.IsKeyPressed(rl.KeyBackspace) || rl.IsKeyPressedRepeat(rl.KeyBackspace)) && len(chatInput) > 0 {
				runes := []rune(chatInput)
				chatInput = string(runes[:len(runes)-1])
			}
			if rl.IsKeyPressed(rl.KeyEnter) {
				if strings.TrimSpace(chatInput) != "" {
					if len(chatHistory) == 0 || chatHistory[len(chatHistory)-1] != chatInput {
						chatHistory = append(chatHistory, chatInput)
						if len(chatHistory) > 25 {
							chatHistory = chatHistory[len(chatHistory)-25:]
						}
					}
					handleChatCommand(chatInput, kp, netState, hopToWorld, &avatarInv, &propInv, &localAvHash, exportsDir, &spawn, playerHeadPos, activeWorld.URL, activeWorld.Manifest.World.Title, worldModel, &cfg, settingsPath, &directory, dirPath)
					chatInput = ""
				}
				chatHistIdx = len(chatHistory)
				chatDraft = ""
				chatActive = false
			} else if rl.IsKeyPressed(rl.KeyEscape) {
				chatInput = ""
				chatHistIdx = len(chatHistory)
				chatDraft = ""
				chatActive = false
			}
		}

		// Compute local body position & yaw (used for 3rd-person avatar, portals & STATE sync)
		lookVec := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))
		bodyHeadPos := playerHeadPos
		bodyYaw := float32(math.Atan2(float64(lookVec.X), float64(lookVec.Z)) * (180.0 / math.Pi))

		// Update shader camera position uniform
		if cfg.EnableFog && fogShader.ID > 0 && viewPosLoc >= 0 {
			rl.SetShaderValue(fogShader, viewPosLoc, []float32{cam.Position.X, cam.Position.Y, cam.Position.Z}, rl.ShaderUniformVec3)
		}

		// Check physical [[portals]] trigger volumes defined in world.toml
		if time.Since(lastPortalHop) > 3*time.Second {
			for _, portal := range activeWorld.Manifest.Portals {
				if len(portal.Pos) != 3 || portal.URL == "" {
					continue
				}
				rad := portal.Radius
				if rad <= 0 {
					rad = 1.6
				}
				pCenter := rl.NewVector3(portal.Pos[0], portal.Pos[1], portal.Pos[2])
				if rl.Vector3Distance(bodyHeadPos, pCenter) <= rad {
					hopToWorld(portal.URL)
					break
				}
			}
		}

		if time.Since(lastSync) >= 33*time.Millisecond {
			lastSync = time.Now()
			netState.mu.RLock()
			curNick := netState.nick
			netState.mu.RUnlock()

			_ = netState.sendJSON(WireMsg{
				Type:       "STATE",
				Nick:       curNick,
				Pos:        []float32{bodyHeadPos.X, bodyHeadPos.Y, bodyHeadPos.Z},
				Yaw:        bodyYaw,
				AvatarHash: localAvHash,
			})
		}

		type screenLabel struct {
			screenPos rl.Vector2
			label     string
			speaking  bool
			isPortal  bool
		}
		var screenLabels []screenLabel

		rl.BeginDrawing()
		rl.ClearBackground(rl.NewColor(11, 14, 21, 255))

		rl.BeginMode3D(cam)
		rl.DrawGrid(32, 1.0)
		rl.DrawModel(worldModel, rl.Vector3Zero(), 1.0, rl.White)

		// Render physical 3D world-space portal gates from [[portals]]
		pulse := float32(math.Sin(rl.GetTime()*3.0))*0.12 + 1.0
		for _, portal := range activeWorld.Manifest.Portals {
			if len(portal.Pos) != 3 || portal.URL == "" {
				continue
			}
			rad := portal.Radius
			if rad <= 0 {
				rad = 1.6
			}
			pPos := rl.NewVector3(portal.Pos[0], portal.Pos[1], portal.Pos[2])
			rl.DrawCircle3D(pPos, rad*pulse, rl.NewVector3(0, 1, 0), 90, rl.SkyBlue)
			rl.DrawCylinderWires(pPos, rad, rad, 2.6, 16, rl.Fade(rl.SkyBlue, 0.55))

			toPortal := rl.Vector3Subtract(pPos, cam.Position)
			dist := rl.Vector3Length(toPortal)
			if dist > 0.5 && dist < 50.0 && rl.Vector3DotProduct(lookVec, toPortal) > 0 {
				labelName := portal.Name
				if labelName == "" {
					labelName = portal.URL
				}
				sp := rl.GetWorldToScreen(rl.NewVector3(pPos.X, pPos.Y+2.9, pPos.Z), cam)
				screenLabels = append(screenLabels, screenLabel{
					screenPos: sp,
					label:     "PORTAL: " + sanitizeDisplayText(labelName, 32),
					isPortal:  true,
				})
			}
		}

		if thirdPerson && walkMode {
			feetPos := rl.NewVector3(bodyHeadPos.X, bodyHeadPos.Y-1.65, bodyHeadPos.Z)
			rl.DrawCircle3D(rl.NewVector3(feetPos.X, feetPos.Y+0.02, feetPos.Z), 0.42, rl.NewVector3(1, 0, 0), 90, rl.Fade(rl.SkyBlue, 0.7))
			if mod, ok := loadedModels[localAvHash]; ok {
				drawPos := rl.NewVector3(feetPos.X, feetPos.Y+modelFeetOffsetY(mod), feetPos.Z)
				rl.DrawModelEx(mod, drawPos, rl.NewVector3(0, 1, 0), bodyYaw, rl.NewVector3(1, 1, 1), rl.White)
			} else {
				rl.DrawCapsule(
					rl.NewVector3(feetPos.X, feetPos.Y+0.35, feetPos.Z),
					rl.NewVector3(feetPos.X, feetPos.Y+1.45, feetPos.Z),
					0.35, 8, 8, rl.Lime,
				)
			}
		}

		if ghostActive {
			spawnFloor := rl.NewVector3(spawn.X, spawn.Y-1.62, spawn.Z)
			rl.DrawCircle3D(spawnFloor, 0.85, rl.NewVector3(1, 0, 0), 90, rl.Gold)
			rl.DrawCylinderWires(spawnFloor, 0.85, 0.85, 2.0, 12, rl.Fade(rl.Gold, 0.65))
			if activeProp.Hash != "" {
				if mod, ok := loadedModels[activeProp.Hash]; ok {
					rl.DrawModelEx(mod, ghostPos, rl.NewVector3(0, 1, 0), ghostYaw, rl.NewVector3(ghostScale, ghostScale, ghostScale), rl.Fade(rl.Lime, 0.55))
				}
				rl.DrawCircle3D(ghostPos, 0.5, rl.NewVector3(1, 0, 0), 90, rl.Lime)
			}
		}

		if lookedAt != nil {
			hlCol := rl.SkyBlue
			if lookedAt.CanDelete {
				hlCol = rl.Gold
			}
			rl.DrawBoundingBox(lookedAt.Box, hlCol)
		}

		netState.mu.RLock()
		for _, prop := range netState.props {
			if mod, ok := loadedModels[prop.Hash]; ok {
				s := prop.Scale
				if s <= 0 {
					s = 1.0
				}
				rl.DrawModelEx(mod, prop.Pos, rl.NewVector3(0, 1, 0), prop.Yaw, rl.NewVector3(s, s, s), rl.White)
			} else {
				rl.DrawCubeWires(prop.Pos, 0.8, 0.8, 0.8, rl.Gold)
			}
		}

		for _, peer := range netState.peers {
			if time.Since(peer.LastSeen) > 10*time.Second || netState.blockedPeers[peer.PubKey] {
				continue
			}
			peerFeetPos := rl.NewVector3(peer.Pos.X, peer.Pos.Y-1.65, peer.Pos.Z)
			if mod, ok := loadedModels[peer.AvatarHash]; ok {
				drawPos := rl.NewVector3(peerFeetPos.X, peerFeetPos.Y+modelFeetOffsetY(mod), peerFeetPos.Z)
				rl.DrawModelEx(mod, drawPos, rl.NewVector3(0, 1, 0), peer.Yaw, rl.NewVector3(1, 1, 1), rl.White)
			} else {
				rl.DrawCapsule(
					rl.NewVector3(peerFeetPos.X, peerFeetPos.Y+0.35, peerFeetPos.Z),
					rl.NewVector3(peerFeetPos.X, peerFeetPos.Y+1.45, peerFeetPos.Z),
					0.35, 8, 8, rl.SkyBlue,
				)
			}
			speaking := time.Since(peer.Speaking) < 250*time.Millisecond
			if speaking {
				rl.DrawCircle3D(
					rl.NewVector3(peerFeetPos.X, peerFeetPos.Y+0.05, peerFeetPos.Z),
					0.65, rl.NewVector3(1, 0, 0), 90, rl.Lime,
				)
			}
			if cfg.ShowTags {
				toPeer := rl.Vector3Subtract(peer.Pos, cam.Position)
				dist := rl.Vector3Length(toPeer)
				if dist > 0.5 && dist < 45.0 && rl.Vector3DotProduct(lookVec, toPeer) > 0 {
					tagWorld := rl.NewVector3(peer.Pos.X, peer.Pos.Y+0.65, peer.Pos.Z)
					sp := rl.GetWorldToScreen(tagWorld, cam)
					screenLabels = append(screenLabels, screenLabel{
						screenPos: sp,
						label:     formatPeerTag(peer.Nick, peer.PubKey),
						speaking:  speaking,
					})
				}
			}
		}
		netState.mu.RUnlock()
		rl.EndMode3D()

		for _, sl := range screenLabels {
			if sl.screenPos.X < 0 || sl.screenPos.X > float32(sw) || sl.screenPos.Y < 0 || sl.screenPos.Y > float32(sh) {
				continue
			}
			txt := sl.label
			borderCol := rl.NewColor(70, 130, 180, 180)
			txtCol := rl.RayWhite
			if sl.isPortal {
				borderCol = rl.SkyBlue
				txtCol = rl.SkyBlue
			} else if sl.speaking {
				txt = "[MIC] " + txt
				borderCol = rl.Lime
				txtCol = rl.Lime
			}
			tw := rl.MeasureText(txt, 15)
			bx := int32(sl.screenPos.X) - tw/2 - 6
			by := int32(sl.screenPos.Y) - 11
			rl.DrawRectangle(bx, by, tw+12, 22, rl.NewColor(10, 14, 22, 190))
			rl.DrawRectangleLines(bx, by, tw+12, 22, borderCol)
			rl.DrawText(txt, bx+6, by+3, 15, txtCol)
		}

		// Center crosshair + look-at target tooltip
		if cfg.ShowCross && cursorLocked && !anyOverlay {
			cx, cy := sw/2, sh/2
			crossCol := rl.Fade(rl.RayWhite, 0.55)
			if lookedAt != nil {
				crossCol = rl.Gold
			}
			rl.DrawLine(cx-6, cy, cx+6, cy, crossCol)
			rl.DrawLine(cx, cy-6, cx, cy+6, crossCol)

			if lookedAt != nil {
				w1 := rl.MeasureText(lookedAt.Label, 16)
				w2 := rl.MeasureText(lookedAt.SubLabel, 14)
				boxW := w1
				if w2 > boxW {
					boxW = w2
				}
				boxW += 20
				bx := cx - boxW/2
				by := cy + 16
				rl.DrawRectangle(bx, by, boxW, 44, rl.NewColor(12, 16, 26, 215))
				rl.DrawRectangleLines(bx, by, boxW, 44, rl.Gold)
				rl.DrawText(lookedAt.Label, bx+10, by+5, 16, rl.Gold)
				rl.DrawText(lookedAt.SubLabel, bx+10, by+24, 14, rl.RayWhite)
			}
		}

		netState.mu.RLock()
		peerCount := len(netState.peers)
		propCount := len(netState.props)
		curNick := netState.nick
		isMuted := netState.muted
		isConn := netState.connected
		rHost := netState.relayHost
		lines := append([]ChatEntry(nil), netState.chatLog...)
		var activeDownloads []AssemblyProgress
		for h, asm := range netState.assemblies {
			activeDownloads = append(activeDownloads, AssemblyProgress{
				Hash:     h,
				Received: len(asm.Chunks),
				Total:    asm.Total,
			})
		}
		netState.mu.RUnlock()

		connDot := rl.Red
		connLabel := "OFFLINE"
		if isConn {
			connDot = rl.Lime
			connLabel = "ONLINE (" + rHost + ")"
		}
		headerLine := fmt.Sprintf("%s  |  %s  |  %s", connLabel, activeWorld.Manifest.World.Title, formatPeerTag(curNick, kp.PubHex()))

		modeBadge := "WALK"
		if !walkMode {
			modeBadge = "FLY"
		} else if thirdPerson {
			modeBadge = "WALK 3P"
		}
		statsLine := fmt.Sprintf("FPS: %d  |  Mode: %s  |  Peers: %d  |  Props: %d", rl.GetFPS(), modeBadge, peerCount, propCount)

		buildLine := ""
		if ghostActive && activeProp.Name != "" {
			snapTxt := "OFF"
			if snapToGrid {
				snapTxt = "ON"
			}
			buildLine = fmt.Sprintf("[BUILD: %s] Snap[B]:%s | Yaw:%.0f | Y:%.2f | Scale:%.2fx | [F4] Spawn | [G/Esc] Exit", activeProp.Name, snapTxt, ghostYawOffset, ghostYOffset, ghostScale)
		}

		// Auto-size HUD width to fit content cleanly while reserving space for top-right status/radar
		desiredW := rl.MeasureText(headerLine, 17) + 42
		if w2 := rl.MeasureText(statsLine, 16) + 24; w2 > desiredW {
			desiredW = w2
		}
		if luaHUD != "" {
			if w3 := rl.MeasureText(luaHUD, 16) + 24; w3 > desiredW {
				desiredW = w3
			}
		}
		if buildLine != "" {
			if w4 := rl.MeasureText(buildLine, 15) + 24; w4 > desiredW {
				desiredW = w4
			}
		}
		maxHudW := sw - 295
		if maxHudW < 340 {
			maxHudW = 340
		}
		hudW := desiredW
		if hudW < 420 {
			hudW = 420
		}
		if hudW > maxHudW {
			hudW = maxHudW
		}

		hudH := int32(68)
		if luaHUD != "" {
			hudH += 24
		}
		if buildLine != "" {
			hudH += 24
		}
		rl.DrawRectangle(12, 12, hudW, hudH, rl.NewColor(14, 18, 28, 195))
		rl.DrawRectangleLines(12, 12, hudW, hudH, rl.NewColor(55, 75, 105, 200))

		rl.BeginScissorMode(14, 14, hudW-4, hudH-4)
		rl.DrawCircle(26, 26, 5, connDot)
		rl.DrawText(truncateToWidth(headerLine, 17, hudW-38), 38, 18, 17, rl.RayWhite)
		rl.DrawText(truncateToWidth(statsLine, 16, hudW-22), 22, 44, 16, rl.LightGray)

		nextHudY := int32(68)
		if luaHUD != "" {
			rl.DrawText(truncateToWidth(luaHUD, 16, hudW-22), 22, nextHudY, 16, rl.Gold)
			nextHudY += 24
		}
		if buildLine != "" {
			rl.DrawText(truncateToWidth(buildLine, 15, hudW-22), 22, nextHudY, 15, rl.Lime)
		}
		rl.EndScissorMode()

		if rl.IsKeyDown(rl.KeyV) && !chatActive {
			rl.DrawRectangle(sw-260, 12, 246, 28, rl.NewColor(20, 45, 25, 215))
			rl.DrawRectangleLines(sw-260, 12, 246, 28, rl.Lime)
			rl.DrawText("[MIC LIVE - SPATIAL TX]", sw-248, 18, 16, rl.Lime)
		} else if isMuted {
			rl.DrawRectangle(sw-220, 12, 206, 28, rl.NewColor(50, 18, 22, 215))
			rl.DrawRectangleLines(sw-220, 12, 206, 28, rl.Red)
			rl.DrawText("[AUDIO MUTED - M]", sw-206, 18, 16, rl.Red)
		} else if !cursorLocked && !anyOverlay {
			rl.DrawRectangle(sw-275, 12, 261, 28, rl.NewColor(35, 30, 16, 215))
			rl.DrawRectangleLines(sw-275, 12, 261, 28, rl.Gold)
			rl.DrawText("[CLICK WINDOW TO LOOK]", sw-262, 18, 16, rl.Gold)
		}

		if cfg.ShowRadar {
			drawMiniRadar(sw-84, 112, 64, cam, lookVec, netState)
		}

		// Active P2P .glb download progress bar(s) below radar
		if len(activeDownloads) > 0 {
			sort.Slice(activeDownloads, func(i, j int) bool {
				return activeDownloads[i].Hash < activeDownloads[j].Hash
			})
			dlY := int32(190)
			if !cfg.ShowRadar {
				dlY = 50
			}
			for i, dl := range activeDownloads {
				if i >= 3 || dl.Total <= 0 {
					continue
				}
				pct := float32(dl.Received) / float32(dl.Total)
				if pct > 1.0 {
					pct = 1.0
				}
				bw := int32(190)
				bx := sw - bw - 14
				rl.DrawRectangle(bx, dlY, bw, 32, rl.NewColor(14, 18, 28, 215))
				rl.DrawRectangleLines(bx, dlY, bw, 32, rl.SkyBlue)
				rl.DrawRectangle(bx+4, dlY+20, int32(float32(bw-8)*pct), 7, rl.SkyBlue)
				rl.DrawText(fmt.Sprintf("DL %s.. %d/%d", dl.Hash[:6], dl.Received, dl.Total), bx+6, dlY+4, 13, rl.RayWhite)
				dlY += 38
			}
		}

		chatBoxW := int32(640)
		if sw-32 < chatBoxW {
			chatBoxW = sw - 32
		}
		if chatBoxW < 280 {
			chatBoxW = 280
		}
		const (
			chatX        = int32(12)
			chatFontSize = int32(16)
			chatLineH    = int32(20)
			chatPadX     = int32(10)
			chatPadY     = int32(8)
			maxVisLines  = 9
		)
		innerTextW := chatBoxW - chatPadX*2

		var wrappedLines []renderedChatLine
		for _, entry := range lines {
			for _, seg := range wrapChatLine(entry.Text, chatFontSize, innerTextW) {
				wrappedLines = append(wrappedLines, renderedChatLine{
					text:  seg,
					color: entry.Color,
				})
			}
		}
		if len(wrappedLines) > maxVisLines {
			wrappedLines = wrappedLines[len(wrappedLines)-maxVisLines:]
		}

		if len(wrappedLines) > 0 || chatActive {
			headerH := int32(0)
			if chatActive {
				headerH = 22
			}
			bodyH := int32(len(wrappedLines))*chatLineH + chatPadY*2
			if bodyH < 34 {
				bodyH = 34
			}
			panelH := headerH + bodyH
			panelY := sh - 46 - panelH

			bgAlpha := uint8(190)
			borderCol := rl.NewColor(55, 75, 105, 190)
			if chatActive {
				bgAlpha = 228
				borderCol = rl.NewColor(80, 175, 130, 230)
			}
			rl.DrawRectangle(chatX, panelY, chatBoxW, panelH, rl.NewColor(11, 15, 23, bgAlpha))
			rl.DrawRectangleLines(chatX, panelY, chatBoxW, panelH, borderCol)
			// Subtle left accent bar for a crisp terminal/HUD silhouette
			rl.DrawRectangle(chatX, panelY, 3, panelH, borderCol)

			if chatActive {
				rl.DrawRectangle(chatX+3, panelY+1, chatBoxW-4, headerH-1, rl.NewColor(18, 25, 38, 240))
				rl.DrawText("COMMS  |  /help /clear /nick /bookmark /copykey  |  [Up/Dn] History", chatX+chatPadX, panelY+4, 13, rl.SkyBlue)
			}

			rl.BeginScissorMode(chatX+4, panelY+headerH+2, chatBoxW-8, bodyH-4)
			if len(wrappedLines) == 0 {
				rl.DrawText("No messages yet. Type below and press [Enter] to send...", chatX+chatPadX, panelY+headerH+chatPadY, 15, rl.Gray)
			} else {
				cy := panelY + headerH + chatPadY
				for _, wl := range wrappedLines {
					rl.DrawText(wl.text, chatX+chatPadX, cy, chatFontSize, wl.color)
					cy += chatLineH
				}
			}
			rl.EndScissorMode()
		}

		if chatActive {
			inputY := sh - 40
			inputH := int32(28)
			rl.DrawRectangle(chatX, inputY, chatBoxW, inputH, rl.NewColor(18, 26, 38, 245))
			rl.DrawRectangleLines(chatX, inputY, chatBoxW, inputH, rl.Lime)
			cursorBlink := "_"
			if int(rl.GetTime()*2.2)%2 == 1 {
				cursorBlink = " "
			}
			renderedInput := fitInputTail("> ", chatInput, cursorBlink, 17, chatBoxW-18)
			rl.BeginScissorMode(chatX+4, inputY+2, chatBoxW-8, inputH-4)
			rl.DrawText(renderedInput, chatX+9, inputY+6, 17, rl.Lime)
			rl.EndScissorMode()
		} else {
			hintTxt := "[I] Inv/Trade | [C] 3rd-Person | [F] Walk/Fly | [R] Spawn | [T] Chat | [V] Voice | [F1] Help | [F2] Settings | [F3] Peers"
			if rl.MeasureText(hintTxt, 15) > sw-28 {
				hintTxt = "[I] Inv | [C] 3P | [F] Walk/Fly | [T] Chat | [V] Voice | [F1] Help | [F2] Settings | [F3] Peers"
			}
			rl.DrawText(hintTxt, 14, sh-30, 15, rl.Gray)
		}

		if invOpen {
			mw, mh := int32(720), int32(450)
			mx, my := (sw-mw)/2, (sh-mh)/2
			rl.DrawRectangle(mx, my, mw, mh, rl.NewColor(14, 18, 26, 248))
			rl.DrawRectangleLines(mx, my, mw, mh, rl.SkyBlue)
			rl.DrawText("LOCAL INVENTORY & KEY-SIGNED TRANSFERS (~/.vektorweb-client/)", mx+24, my+18, 18, rl.SkyBlue)

			propCol := rl.Gray
			avCol := rl.Gray
			activeList := propInv
			if invTab == "prop" {
				propCol = rl.Lime
			} else {
				avCol = rl.Lime
				activeList = avatarInv
			}
			rl.DrawText(fmt.Sprintf("[1] Props (%d)", len(propInv)), mx+24, my+50, 18, propCol)
			rl.DrawText(fmt.Sprintf("[2] Avatars (%d)", len(avatarInv)), mx+195, my+50, 18, avCol)

			if transferModalOpen && len(activeList) > 0 {
				chosen := activeList[invCursor%len(activeList)]
				rl.DrawText("TRANSFER ITEM: "+chosen.Name, mx+24, my+88, 18, rl.Gold)
				lockLabel := "VENDOR-LOCKED (Recipient CANNOT resell/redistribute)"
				lockCol := rl.SkyBlue
				if transferAllowResale {
					lockLabel = "TRANSFERABLE (Recipient MAY transfer to others)"
					lockCol = rl.Lime
				}
				rl.DrawText("[L] License Mode: "+lockLabel, mx+24, my+116, 16, lockCol)

				netState.mu.RLock()
				var onlinePeers []*RemotePeer
				for _, p := range netState.peers {
					if time.Since(p.LastSeen) <= 15*time.Second {
						onlinePeers = append(onlinePeers, p)
					}
				}
				netState.mu.RUnlock()

				if len(onlinePeers) == 0 {
					rl.DrawText("No online peers in this world to transfer to.", mx+24, my+158, 17, rl.LightGray)
				} else {
					rl.DrawText("Select Recipient Peer:", mx+24, my+150, 17, rl.RayWhite)
					for i, p := range onlinePeers {
						if i >= 6 {
							break
						}
						prefix := "   "
						col := rl.RayWhite
						if i == transferPeerCursor%len(onlinePeers) {
							prefix = "-> "
							col = rl.Gold
						}
						rl.DrawText(fmt.Sprintf("%s%s (%s...)", prefix, formatPeerTag(p.Nick, p.PubKey), p.PubKey[:12]), mx+24, my+int32(178+i*28), 17, col)
					}
				}
				rl.DrawText("[Up/Down] Recipient | [L] Toggle Vendor-Lock | [Enter] Sign & Send | [Esc] Back", mx+24, my+mh-34, 15, rl.Gray)
			} else {
				if len(activeList) == 0 {
					rl.DrawText("No .glb files found. Drag & drop a .glb onto the window!", mx+24, my+98, 17, rl.LightGray)
				} else {
					for i, item := range activeList {
						if i >= 9 {
							break
						}
						prefix := "   "
						col := rl.RayWhite
						if i == invCursor%len(activeList) {
							prefix = "-> "
							col = rl.Gold
						}
						equipped := ""
						if invTab == "avatar" && item.Hash == localAvHash {
							equipped = " [EQUIPPED]"
						}
						rl.DrawText(fmt.Sprintf("%s%s (%s...)%s%s", prefix, item.Name, item.Hash[:8], item.StatusTag, equipped), mx+24, my+int32(90+i*28), 16, col)
					}
				}
				rl.DrawText("[Up/Down] Select | [Enter] Equip/Arm | [R] Transfer/Sell to Peer | [I/Esc] Close", mx+24, my+mh-34, 15, rl.Gray)
			}
		}

		if settingsOpen {
			mw, mh := int32(580), int32(375)
			mx, my := (sw-mw)/2, (sh-mh)/2
			rl.DrawRectangle(mx, my, mw, mh, rl.NewColor(14, 18, 26, 248))
			rl.DrawRectangleLines(mx, my, mw, mh, rl.SkyBlue)
			rl.DrawText("CLIENT SETTINGS (Saved to settings.toml)", mx+24, my+18, 19, rl.SkyBlue)

			opts := []string{
				fmt.Sprintf("Field of View (FOV) : %.0f deg", cfg.FOV),
				fmt.Sprintf("Mouse Sensitivity   : %.4f", cfg.MouseSens),
				fmt.Sprintf("Voice Volume        : %.0f%%", cfg.VoiceVolume*100),
				fmt.Sprintf("Mini-Radar HUD      : %v", cfg.ShowRadar),
				fmt.Sprintf("3D Nameplates       : %v", cfg.ShowTags),
				fmt.Sprintf("Center Crosshair    : %v", cfg.ShowCross),
				fmt.Sprintf("Depth Fog & Lighting: %v", cfg.EnableFog),
			}
			for i, line := range opts {
				prefix := "   "
				col := rl.RayWhite
				if i == settingsCursor {
					prefix = "-> "
					col = rl.Gold
				}
				rl.DrawText(prefix+line, mx+24, my+int32(62+i*34), 17, col)
			}
			rl.DrawText("[Up/Down] Select  |  [Left/Right] Adjust  |  [F2 / Esc] Close", mx+24, my+mh-34, 15, rl.Gray)
		}

		if peersOpen {
			mw, mh := int32(640), int32(380)
			mx, my := (sw-mw)/2, (sh-mh)/2
			rl.DrawRectangle(mx, my, mw, mh, rl.NewColor(14, 18, 26, 248))
			rl.DrawRectangleLines(mx, my, mw, mh, rl.SkyBlue)
			rl.DrawText("ONLINE PEERS IN WORLD", mx+24, my+18, 19, rl.SkyBlue)

			netState.mu.RLock()
			var onlinePeers []*RemotePeer
			for _, p := range netState.peers {
				if time.Since(p.LastSeen) <= 15*time.Second {
					onlinePeers = append(onlinePeers, p)
				}
			}
			blockedCopy := make(map[string]bool, len(netState.blockedPeers))
			for k, v := range netState.blockedPeers {
				blockedCopy[k] = v
			}
			netState.mu.RUnlock()

			if len(onlinePeers) == 0 {
				rl.DrawText("No other peers currently connected.", mx+24, my+70, 17, rl.LightGray)
			} else {
				for i, p := range onlinePeers {
					if i >= 8 {
						break
					}
					prefix := "   "
					col := rl.RayWhite
					if i == peerCursor%len(onlinePeers) {
						prefix = "-> "
						col = rl.Gold
					}
					tag := ""
					if blockedCopy[p.PubKey] {
						tag = " [BLOCKED]"
					}
					dist := rl.Vector3Distance(cam.Position, p.Pos)
					rl.DrawText(fmt.Sprintf("%s%s (%s...) — %.1fm%s", prefix, formatPeerTag(p.Nick, p.PubKey), p.PubKey[:12], dist, tag), mx+24, my+int32(60+i*30), 17, col)
				}
			}
			rl.DrawText("[Up/Down] Select | [Enter] Teleport To | [B] Block/Mute | [F3 / Esc] Close", mx+24, my+mh-34, 15, rl.Gray)
		}

		if hasIncomingOffer && pendingDrop == nil {
			netState.mu.RLock()
			offer := netState.incomingXfers[0]
			netState.mu.RUnlock()

			mw, mh := int32(660), int32(220)
			mx, my := (sw-mw)/2, (sh-mh)/2
			rl.DrawRectangle(mx, my, mw, mh, rl.NewColor(14, 20, 30, 248))
			rl.DrawRectangleLines(mx, my, mw, mh, rl.Gold)
			rl.DrawText("INCOMING KEY-SIGNED ASSET TRANSFER", mx+24, my+20, 19, rl.Gold)
			rl.DrawText(fmt.Sprintf("From: %s (%s...)", offer.FromNick, offer.FromPub[:12]), mx+24, my+52, 17, rl.RayWhite)
			rl.DrawText(fmt.Sprintf("Item: %s (%s)", offer.FileName, offer.AssetKind), mx+24, my+78, 17, rl.SkyBlue)
			licInfo := "License: VENDOR-LOCKED (Bound to your Ed25519 key; non-transferable)"
			if offer.License != nil && offer.License.Transferable {
				licInfo = "License: TRANSFERABLE (Bound to your Ed25519 key; resale allowed)"
			}
			rl.DrawText(licInfo, mx+24, my+106, 15, rl.Lime)
			rl.DrawText("[Y] Accept & Download   |   [N / Esc] Decline", mx+24, my+175, 18, rl.Gold)
		}

		if pendingDrop != nil {
			mw, mh := int32(600), int32(210)
			mx, my := (sw-mw)/2, (sh-mh)/2
			rl.DrawRectangle(mx, my, mw, mh, rl.NewColor(14, 20, 30, 248))
			rl.DrawRectangleLines(mx, my, mw, mh, rl.Lime)
			rl.DrawText("IMPORTED .GLB (VALIDATED BY GLBGUARD)", mx+24, my+20, 19, rl.Lime)
			rl.DrawText("File: "+pendingDrop.FileName, mx+24, my+52, 18, rl.RayWhite)
			rl.DrawText("[1] Equip as My Live Avatar", mx+24, my+95, 18, rl.SkyBlue)
			rl.DrawText("[2] Add to Prop Inventory & Place", mx+24, my+125, 18, rl.Gold)
			rl.DrawText("[Esc] Cancel", mx+24, my+165, 16, rl.Gray)
		}

		if dirOpen {
			mw, mh := int32(620), int32(360)
			mx, my := (sw-mw)/2, (sh-mh)/2
			rl.DrawRectangle(mx, my, mw, mh, rl.NewColor(16, 20, 30, 245))
			rl.DrawRectangleLines(mx, my, mw, mh, rl.SkyBlue)
			rl.DrawText("WORLD DIRECTORY (~/.vektorweb-client/worlds.toml)", mx+24, my+20, 19, rl.SkyBlue)
			for i, w := range directory.Worlds {
				if i >= 9 {
					break
				}
				rl.DrawText(fmt.Sprintf("[%d] %s", i+1, w.Name), mx+24, my+int32(58+i*30), 18, rl.RayWhite)
				rl.DrawText(w.URL, mx+54, my+int32(76+i*30), 14, rl.Gray)
			}
			rl.DrawText("Press [1-9] to hop portal or [Tab / Esc] to close", mx+24, my+mh-34, 16, rl.Gold)
		}

		if helpOpen {
			mw, mh := int32(720), int32(530)
			mx, my := (sw-mw)/2, (sh-mh)/2
			rl.DrawRectangle(mx, my, mw, mh, rl.NewColor(14, 18, 26, 248))
			rl.DrawRectangleLines(mx, my, mw, mh, rl.Gold)
			rl.DrawText("VEKTORWEB CONTROLS & SLASH COMMANDS", mx+26, my+18, 19, rl.Gold)
			helpLines := []string{
				"[I]              : Local Inventory ([R] to Transfer/Sell signed item)",
				"Drag & Drop .glb : Equip avatar or import prop into inventory",
				"WASD + Mouse     : Walk / Look ([Shift] Sprint, [Space] Jump, [F] Walk/Fly)",
				"[C] / [R]        : Toggle 3rd-Person View (Wheel zooms) / Return to Spawn",
				"Look-At Target   : [Del] Remove your prop | [E] Offer trade to peer",
				"[T] or [/]       : Chat (/help, /clear, /nick, /bookmark, /copykey, /setspawn, /mint)",
				"[V] / [M]        : Hold Push-to-Talk voice (35m) / Mute incoming voice",
				"[G]/[Esc] [B] [N]: Toggle/Exit Build Mode / Snap-to-Grid / Next prop ([F4] Set Spawn)",
				"[Q]/[E] [PgUp/Dn]: Rotate & nudge prop height ([+/-] scale, [P]/[X] place/undo)",
				"[Tab] / [F12]    : World Portal Directory / Save Screenshot to exports/",
				"[F2] / [F3]      : Client Settings (FOV, Sens, Fog) / Online Peers List",
			}
			for i, hl := range helpLines {
				rl.DrawText(hl, mx+26, my+int32(56+i*38), 16, rl.RayWhite)
			}
		}

		rl.EndDrawing()
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func clampFloat32(v, minV, maxV float32) float32 {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

func sanitizeFileName(raw string) string {
	base := filepath.Base(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "asset.glb"
	}
	return out
}

type renderedChatLine struct {
	text  string
	color rl.Color
}

func wrapChatLine(text string, fontSize, maxWidth int32) []string {
	if text == "" {
		return nil
	}
	if maxWidth <= 40 || rl.MeasureText(text, fontSize) <= maxWidth {
		return []string{text}
	}
	var out []string
	words := strings.Fields(text)
	cur := ""
	for _, w := range words {
		candidate := w
		if cur != "" {
			candidate = cur + " " + w
		}
		if rl.MeasureText(candidate, fontSize) <= maxWidth {
			cur = candidate
			continue
		}
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
		// Break oversized single tokens (such as 64-char pubkeys or URLs) by rune width
		if rl.MeasureText(w, fontSize) > maxWidth {
			var chunk strings.Builder
			for _, r := range w {
				test := chunk.String() + string(r)
				if rl.MeasureText(test, fontSize) > maxWidth && chunk.Len() > 0 {
					out = append(out, chunk.String())
					chunk.Reset()
				}
				chunk.WriteRune(r)
			}
			cur = chunk.String()
		} else {
			cur = w
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func truncateToWidth(text string, fontSize, maxWidth int32) string {
	if text == "" || maxWidth <= 24 || rl.MeasureText(text, fontSize) <= maxWidth {
		return text
	}
	runes := []rune(text)
	for i := len(runes) - 1; i >= 1; i-- {
		cand := string(runes[:i]) + "..."
		if rl.MeasureText(cand, fontSize) <= maxWidth {
			return cand
		}
	}
	return "..."
}

func fitInputTail(prefix, input, suffix string, fontSize, maxWidth int32) string {
	full := prefix + input + suffix
	if maxWidth <= 40 || rl.MeasureText(full, fontSize) <= maxWidth {
		return full
	}
	runes := []rune(input)
	for i := 1; i <= len(runes); i++ {
		cand := prefix + "..." + string(runes[i:]) + suffix
		if rl.MeasureText(cand, fontSize) <= maxWidth {
			return cand
		}
	}
	return prefix + suffix
}

func drawMiniRadar(cx, cy, radius int32, cam rl.Camera3D, lookVec rl.Vector3, ns *NetState) {
	rl.DrawCircle(cx, cy, float32(radius), rl.NewColor(14, 18, 28, 210))
	rl.DrawCircleLines(cx, cy, float32(radius), rl.NewColor(65, 85, 115, 220))
	rl.DrawCircle(cx, cy, 3, rl.Lime)

	flatLook := rl.Vector3Normalize(rl.NewVector3(lookVec.X, 0, lookVec.Z))
	rl.DrawLine(cx, cy, cx+int32(flatLook.X*14), cy+int32(flatLook.Z*14), rl.Lime)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	for _, peer := range ns.peers {
		if time.Since(peer.LastSeen) > 10*time.Second || ns.blockedPeers[peer.PubKey] {
			continue
		}
		dx := (peer.Pos.X - cam.Position.X) * 1.5
		dz := (peer.Pos.Z - cam.Position.Z) * 1.5
		dist := float32(math.Sqrt(float64(dx*dx + dz*dz)))
		if dist > float32(radius-5) {
			scale := float32(radius-5) / dist
			dx *= scale
			dz *= scale
		}
		col := rl.SkyBlue
		if time.Since(peer.Speaking) < 250*time.Millisecond {
			col = rl.Gold
		}
		rl.DrawCircle(cx+int32(dx), cy+int32(dz), 3.5, col)
	}
}

func handleChatCommand(
	input string,
	kp *identity.Keypair,
	ns *NetState,
	hopFn func(string),
	avatarInv *[]InventoryItem,
	propInv *[]InventoryItem,
	localAvHash *string,
	exportsDir string,
	spawn *rl.Vector3,
	curPos rl.Vector3,
	activeWorldURL string,
	activeWorldTitle string,
	worldModel rl.Model,
	cfg *ClientSettings,
	settingsPath string,
	directory *WorldDirectory,
	dirPath string,
) {
	if input == "/help" || input == "/?" || strings.HasPrefix(input, "/help ") {
		ns.addChat("[HELP]: Commands: /help, /clear, /nick <name>, /say <msg>, /w <peer> <msg>, /portal <url>, /bookmark [name], /setspawn [x y z], /pubkey, /copykey, /mint <pub> <lock|free>, /chatlog")
		ns.addChat("[HELP]: Keys: [F1] Full Help | [I] Inventory | [G]/[Esc] Build Mode | [C] 3rd-Person | [F] Walk/Fly | [R] Spawn | [V] Voice | [Up/Dn] Chat History")
		return
	}
	if input == "/clear" {
		ns.clearChat()
		return
	}
	if input == "/copykey" {
		rl.SetClipboardText(kp.PubHex())
		ns.addChat("[PUBKEY]: Copied " + kp.PubHex()[:16] + "... to clipboard!")
		return
	}
	if input == "/bookmark" || strings.HasPrefix(input, "/bookmark ") {
		if activeWorldURL == "" || activeWorldURL == "local://sandbox" {
			ns.addChat("[BOOKMARK]: Cannot bookmark the built-in local://sandbox")
			return
		}
		bmName := sanitizeDisplayText(strings.TrimSpace(strings.TrimPrefix(input, "/bookmark")), 48)
		if bmName == "" {
			bmName = sanitizeDisplayText(activeWorldTitle, 48)
		}
		if bmName == "" {
			bmName = activeWorldURL
		}
		if directory != nil {
			updated := false
			for i := range directory.Worlds {
				if normalizeWorldURL(directory.Worlds[i].URL) == normalizeWorldURL(activeWorldURL) {
					directory.Worlds[i].Name = bmName
					updated = true
					break
				}
			}
			if !updated {
				directory.Worlds = append(directory.Worlds, struct {
					Name string `toml:"name"`
					URL  string `toml:"url"`
				}{Name: bmName, URL: activeWorldURL})
			}
			if err := saveWorldDirectory(dirPath, *directory); err == nil {
				ns.addChat(fmt.Sprintf("[BOOKMARK]: Saved %q to [Tab] World Directory", bmName))
			} else {
				ns.addChat("[BOOKMARK ERR]: " + err.Error())
			}
		}
		return
	}
	if input == "/setspawn" || strings.HasPrefix(input, "/setspawn ") {
		targetPos := curPos
		if strings.HasPrefix(input, "/setspawn ") {
			var x, y, z float32
			if _, err := fmt.Sscanf(strings.TrimPrefix(input, "/setspawn "), "%f %f %f", &x, &y, &z); err != nil {
				ns.addChat("[USAGE]: /setspawn or /setspawn <x> <y> <z>")
				return
			}
			targetPos = rl.NewVector3(x, y, z)
		}
		safe := safeSpawnPosition(worldModel, targetPos)
		*spawn = safe
		_ = ns.sendJSON(WireMsg{
			Type:    "CHAT",
			Channel: "global",
			Text:    fmt.Sprintf("/setspawn %.2f %.2f %.2f", safe.X, safe.Y, safe.Z),
		})
		if savedPath, ok := updateLocalWorldTOMLSpawn(activeWorldURL, safe); ok {
			ns.addChat(fmt.Sprintf("[SPAWN]: Saved [%.2f, %.2f, %.2f] to %s", safe.X, safe.Y, safe.Z, savedPath))
		} else {
			ns.addChat(fmt.Sprintf("[SPAWN]: Set spawn to [%.2f, %.2f, %.2f]", safe.X, safe.Y, safe.Z))
		}
		return
	}
	if input == "/pubkey" {
		ns.addChat("[PUBKEY]: " + kp.PubHex())
		log.Printf("Your Ed25519 Public Key: %s", kp.PubHex())
		return
	}
	if input == "/chatlog" {
		ns.mu.RLock()
		var sb strings.Builder
		for _, entry := range ns.chatLog {
			fmt.Fprintf(&sb, "[%s] %s\n", entry.Timestamp.Format("15:04:05"), entry.Text)
		}
		ns.mu.RUnlock()
		fname := fmt.Sprintf("chatlog_%s.txt", time.Now().Format("20060102_150405"))
		if err := os.WriteFile(filepath.Join(exportsDir, fname), []byte(sb.String()), 0644); err == nil {
			ns.addChat("[SYS]: Exported chat log to exports/" + fname)
		}
		return
	}
	if strings.HasPrefix(input, "/w ") {
		rest := strings.TrimSpace(strings.TrimPrefix(input, "/w "))
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			ns.addChat("[USAGE]: /w <nick-or-pubkey-prefix> <message>")
			return
		}
		query := strings.ToLower(parts[0])
		msgText := strings.TrimSpace(parts[1])
		ns.mu.RLock()
		var targetPub, targetNick string
		for pub, p := range ns.peers {
			if strings.HasPrefix(strings.ToLower(pub), query) || strings.EqualFold(p.Nick, query) {
				targetPub = pub
				targetNick = p.Nick
				break
			}
		}
		n := ns.nick
		ns.mu.RUnlock()
		if targetPub == "" {
			ns.addChat("[SYS]: No online peer matching " + parts[0])
			return
		}
		_ = ns.sendJSON(WireMsg{Type: "CHAT", Channel: "whisper", TargetPub: targetPub, Text: msgText})
		ns.addChat(fmt.Sprintf("[WHISPER -> %s (%s)]: %s", formatPeerTag(targetNick, targetPub), formatPeerTag(n, kp.PubHex()), msgText))
		return
	}
	if strings.HasPrefix(input, "/mint ") {
		parts := strings.Fields(strings.TrimPrefix(input, "/mint "))
		if len(parts) < 2 {
			ns.addChat("[USAGE]: /mint <64-char-buyer-pubkey> <lock|free>")
			return
		}
		buyerPub := strings.ToLower(parts[0])
		transferable := strings.EqualFold(parts[1], "free")
		targetHash := *localAvHash
		if targetHash == "" || targetHash == "none" {
			if len(*propInv) > 0 {
				targetHash = (*propInv)[0].Hash
			}
		}
		if !isValidHex64(targetHash) {
			ns.addChat("[MINT ERR]: Equip an avatar or select a prop .glb before minting a license")
			return
		}
		lic, err := kp.MintAssetLicense(targetHash, buyerPub, transferable)
		if err != nil {
			ns.addChat("[MINT ERR]: " + err.Error())
			return
		}
		outLicPath := filepath.Join(ns.cacheDir, fmt.Sprintf("%s_%s.license.json", targetHash[:12], buyerPub[:8]))
		if err := identity.SaveAssetLicense(outLicPath, lic); err != nil {
			ns.addChat("[MINT ERR]: " + err.Error())
			return
		}
		ns.addChat("[MINTED]: Saved " + filepath.Base(outLicPath))
		return
	}
	if strings.HasPrefix(input, "/nick ") {
		newNick := sanitizeDisplayText(strings.TrimSpace(strings.TrimPrefix(input, "/nick ")), 24)
		if newNick != "" {
			ns.mu.Lock()
			ns.nick = newNick
			pos := ns.localPos
			av := ns.localAvHash
			ns.mu.Unlock()
			if cfg != nil && settingsPath != "" {
				cfg.Nick = newNick
				saveClientSettings(settingsPath, *cfg)
			}
			_ = ns.sendJSON(WireMsg{
				Type:       "STATE",
				Nick:       newNick,
				Pos:        []float32{pos.X, pos.Y, pos.Z},
				AvatarHash: av,
			})
			ns.addChat("[SYS]: Nickname saved to settings.toml -> " + newNick)
		}
		return
	}
	if strings.HasPrefix(input, "/portal ") {
		u := strings.TrimSpace(strings.TrimPrefix(input, "/portal "))
		if u != "" {
			hopFn(u)
		}
		return
	}
	if strings.HasPrefix(input, "/say ") {
		txt := strings.TrimSpace(strings.TrimPrefix(input, "/say "))
		if txt != "" {
			_ = ns.sendJSON(WireMsg{Type: "CHAT", Channel: "proximity", Text: txt})
			ns.mu.RLock()
			n := ns.nick
			ns.mu.RUnlock()
			ns.addChat(fmt.Sprintf("[LOCAL %s]: %s", formatPeerTag(n, kp.PubHex()), txt))
		}
		return
	}

	_ = ns.sendJSON(WireMsg{Type: "CHAT", Channel: "global", Text: input})
	ns.mu.RLock()
	n := ns.nick
	ns.mu.RUnlock()
	ns.addChat(fmt.Sprintf("[%s]: %s", formatPeerTag(n, kp.PubHex()), input))
}

func runRelayLoop(ctx context.Context, relayURL string, kp *identity.Keypair, ns *NetState) {
	u, err := url.Parse(relayURL)
	if err != nil {
		log.Printf("invalid relay URL: %v", err)
		return
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		log.Printf("rejected unsupported relay scheme %q (expected ws or wss)", u.Scheme)
		return
	}

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		dialer := websocket.Dialer{
			HandshakeTimeout: 8 * time.Second,
			Subprotocols:     []string{"vektorweb.v1"},
		}
		conn, _, err := dialer.DialContext(ctx, relayURL, nil)
		if err != nil {
			ns.mu.Lock()
			ns.connected = false
			ns.relayHost = u.Hostname()
			ns.mu.Unlock()

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				if backoff < 15*time.Second {
					backoff *= 2
				}
				continue
			}
		}

		backoff = time.Second
		runRelaySession(ctx, conn, u.Hostname(), kp, ns)

		ns.mu.Lock()
		ns.connected = false
		ns.conn = nil
		ns.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
			ns.addChat("[SYS]: Reconnecting to relay...")
		}
	}
}

func runRelaySession(ctx context.Context, conn *websocket.Conn, relayHost string, kp *identity.Keypair, ns *NetState) {
	defer conn.Close()
	conn.SetReadLimit(512 * 1024)

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var challenge WireMsg
	if err := conn.ReadJSON(&challenge); err != nil || challenge.Type != "CHALLENGE" {
		return
	}

	ns.mu.RLock()
	curNick := ns.nick
	ns.mu.RUnlock()

	sig := kp.SignChallenge(challenge.Nonce, challenge.Timestamp, relayHost)
	reply := WireMsg{
		Type:         "AUTH",
		ProtoVersion: 1,
		Capabilities: []string{"3d_glb", "voice_pcm", "props", "transfers"},
		Nonce:        challenge.Nonce,
		Timestamp:    challenge.Timestamp,
		PubKey:       kp.PubHex(),
		Nick:         curNick,
		Signature:    sig,
		AvatarHash:   ns.localAvHash,
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(reply); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	ns.mu.Lock()
	ns.conn = conn
	ns.connected = true
	ns.relayHost = relayHost
	ns.mu.Unlock()
	ns.addChat("[SYS]: Connected to relay " + relayHost)

	doneCh := make(chan struct{})
	defer close(doneCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-doneCh:
		}
	}()

	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		if msgType == websocket.BinaryMessage {
			if len(raw) > 33 && raw[0] == 0x02 {
				senderPub := hex.EncodeToString(raw[1:33])
				pcm := raw[33:]
				numSamples := len(pcm) / 2

				ns.mu.Lock()
				if ns.muted || ns.blockedPeers[senderPub] {
					ns.mu.Unlock()
					continue
				}
				gain := float32(1.0)
				if p, ok := ns.peers[senderPub]; ok {
					p.Speaking = time.Now()
					dx := p.Pos.X - ns.localPos.X
					dy := p.Pos.Y - ns.localPos.Y
					dz := p.Pos.Z - ns.localPos.Z
					dist := float32(math.Sqrt(float64(dx*dx + dy*dy + dz*dz)))
					if dist >= 35.0 {
						gain = 0.0
					} else if dist > 2.0 {
						gain = 1.0 - ((dist - 2.0) / 33.0)
					}
				}
				if gain > 0.01 && len(ns.voiceQueue) < 16 {
					samples := make([]float32, numSamples)
					for i := 0; i < numSamples; i++ {
						s := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
						samples[i] = (float32(s) / 32768.0) * gain
					}
					ns.voiceQueue = append(ns.voiceQueue, samples)
				}
				ns.mu.Unlock()
			}
			continue
		}

		var msg WireMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "WELCOME":

		case "SPAWN_UPDATE":
			if finiteVec3(msg.Pos) {
				ns.mu.Lock()
				ns.liveSpawn = []float32{msg.Pos[0], msg.Pos[1], msg.Pos[2]}
				ns.mu.Unlock()
			}

		case "ERROR":
			ns.addChat(fmt.Sprintf("[RELAY %s]: %s", sanitizeDisplayText(msg.Code, 32), sanitizeDisplayText(msg.Message, 120)))

		case "STATE":
			if !finiteVec3(msg.Pos) || !isValidHex64(msg.PubKey) {
				continue
			}
			if math.IsNaN(float64(msg.Yaw)) || math.IsInf(float64(msg.Yaw), 0) {
				msg.Yaw = 0
			}
			ns.mu.Lock()
			if ns.blockedPeers[msg.PubKey] {
				ns.mu.Unlock()
				continue
			}
			p, ok := ns.peers[msg.PubKey]
			if !ok {
				p = &RemotePeer{
					PubKey:    msg.PubKey,
					Pos:       rl.NewVector3(msg.Pos[0], msg.Pos[1], msg.Pos[2]),
					Yaw:       msg.Yaw,
					TargetPos: rl.NewVector3(msg.Pos[0], msg.Pos[1], msg.Pos[2]),
					TargetYaw: msg.Yaw,
					HasPos:    true,
				}
				ns.peers[msg.PubKey] = p
			}
			if msg.Nick != "" {
				p.Nick = sanitizeDisplayText(msg.Nick, 24)
			} else if p.Nick == "" {
				p.Nick = msg.PubKey[:8]
			}
			p.TargetPos = rl.NewVector3(msg.Pos[0], msg.Pos[1], msg.Pos[2])
			p.TargetYaw = msg.Yaw
			p.HasPos = true
			p.AvatarHash = msg.AvatarHash
			p.LastSeen = time.Now()

			needAsset := ensureAssetQueued(ns, msg.AvatarHash)
			ns.mu.Unlock()

			if needAsset {
				_ = ns.sendJSON(WireMsg{Type: "REQ_ASSET", TargetPub: msg.PubKey, Hash: msg.AvatarHash})
			}

		case "PROP_SPAWN":
			if !finiteVec3(msg.Pos) || !isValidHex64(msg.Hash) || msg.PropID == "" || len(msg.PropID) > 64 {
				continue
			}
			if math.IsNaN(float64(msg.Yaw)) || math.IsInf(float64(msg.Yaw), 0) {
				msg.Yaw = 0
			}
			scale := msg.Scale
			if math.IsNaN(float64(scale)) || math.IsInf(float64(scale), 0) || scale <= 0 {
				scale = 1.0
			} else if scale > 25.0 {
				scale = 25.0
			}

			ns.mu.Lock()
			if ns.blockedPeers[msg.PubKey] {
				ns.mu.Unlock()
				continue
			}
			ns.props[msg.PropID] = &WorldProp{
				PropID: msg.PropID,
				Owner:  msg.PubKey,
				Hash:   msg.Hash,
				Pos:    rl.NewVector3(msg.Pos[0], msg.Pos[1], msg.Pos[2]),
				Yaw:    msg.Yaw,
				Scale:  scale,
			}
			needAsset := ensureAssetQueued(ns, msg.Hash)
			ns.mu.Unlock()

			if needAsset {
				_ = ns.sendJSON(WireMsg{Type: "REQ_ASSET", TargetPub: msg.PubKey, Hash: msg.Hash})
			}

		case "PROP_REMOVE":
			ns.mu.Lock()
			delete(ns.props, msg.PropID)
			ns.mu.Unlock()

		case "REQ_ASSET":
			if !isValidHex64(msg.Hash) || !isValidHex64(msg.PubKey) {
				continue
			}
			ns.mu.RLock()
			if ns.blockedPeers[msg.PubKey] {
				ns.mu.RUnlock()
				continue
			}
			data, ok := ns.localAssets[msg.Hash]
			ns.mu.RUnlock()
			if ok && len(data) > 0 {
				go streamAssetChunks(ns, msg.PubKey, msg.Hash, data)
			}

		case "ASSET_CHUNK":
			handleIncomingChunk(ns, msg)

		case "CHAT":
			ns.mu.RLock()
			isBlocked := ns.blockedPeers[msg.PubKey]
			ns.mu.RUnlock()
			if isBlocked {
				continue
			}

			if msg.Channel == "whisper" && strings.HasPrefix(msg.Text, transferWhisperPrefix) {
				handleTransferWhisper(ns, msg)
				continue
			}

			label := formatPeerTag(msg.Nick, msg.PubKey)
			prefix := ""
			if msg.Channel == "proximity" {
				prefix = "LOCAL "
			} else if msg.Channel == "whisper" {
				prefix = "WHISPER "
			}
			ns.addChat(fmt.Sprintf("[%s%s]: %s", prefix, label, msg.Text))

		case "LEAVE":
			ns.mu.Lock()
			delete(ns.peers, msg.PubKey)
			ns.mu.Unlock()
		}
	}
}

func handleTransferWhisper(ns *NetState, msg WireMsg) {
	rawJSON := strings.TrimPrefix(msg.Text, transferWhisperPrefix)
	var xfer TransferWhisperPayload
	if err := json.Unmarshal([]byte(rawJSON), &xfer); err != nil {
		return
	}

	switch xfer.Action {
	case "OFFER":
		if xfer.License == nil || !isValidHex64(xfer.Hash) {
			return
		}
		ns.mu.RLock()
		myPub := ns.localPub
		ns.mu.RUnlock()

		if err := xfer.License.Verify(xfer.Hash, myPub); err != nil {
			log.Printf("Rejected invalid transfer offer license from %s: %v", msg.PubKey[:8], err)
			return
		}
		senderNick := formatPeerTag(msg.Nick, msg.PubKey)
		safeName := sanitizeFileName(xfer.FileName)
		kind := "prop"
		if xfer.AssetKind == "avatar" {
			kind = "avatar"
		}
		ns.mu.Lock()
		ns.incomingXfers = append(ns.incomingXfers, &IncomingTransfer{
			TransferID: xfer.TransferID,
			FromPub:    msg.PubKey,
			FromNick:   senderNick,
			AssetKind:  kind,
			FileName:   safeName,
			Hash:       xfer.Hash,
			License:    xfer.License,
		})
		ns.mu.Unlock()
		ns.addChat(fmt.Sprintf("[TRANSFER]: %s offered %s (%s) — press [Y] to accept or [N] to decline!", senderNick, safeName, kind))

	case "ACCEPT":
		ns.mu.Lock()
		out, ok := ns.outgoingXfers[xfer.TransferID]
		if !ok || !strings.EqualFold(out.TargetPub, msg.PubKey) {
			ns.mu.Unlock()
			return
		}
		delete(ns.outgoingXfers, xfer.TransferID)

		isCreator := out.License != nil && strings.EqualFold(out.License.CreatorPub, ns.localPub)
		if !isCreator && out.FilePath != "" {
			_ = os.Remove(out.FilePath)
			_ = os.Remove(licensePathForGLB(out.FilePath))
			delete(ns.localAssets, out.Hash)
			ns.invDirty = true
		}
		ns.mu.Unlock()

		ns.addChat(fmt.Sprintf("[TRANSFER]: Recipient accepted %s!", out.FileName))

	case "DECLINE":
		ns.mu.Lock()
		out, ok := ns.outgoingXfers[xfer.TransferID]
		if ok && strings.EqualFold(out.TargetPub, msg.PubKey) {
			delete(ns.outgoingXfers, xfer.TransferID)
			ns.mu.Unlock()
			ns.addChat(fmt.Sprintf("[TRANSFER]: Recipient declined %s.", out.FileName))
		} else {
			ns.mu.Unlock()
		}
	}
}

func ensureAssetQueued(ns *NetState, hash string) bool {
	if !isValidHex64(hash) || ns.requested[hash] {
		return false
	}
	cachedPath := filepath.Join(ns.cacheDir, strings.ToLower(hash)+".glb")
	if _, err := os.Stat(cachedPath); err == nil {
		ns.requested[hash] = true
		ns.readyModels = append(ns.readyModels, strings.ToLower(hash))
		return false
	}
	ns.requested[hash] = true
	return true
}

func streamAssetChunks(ns *NetState, targetPub, hash string, data []byte) {
	const chunkSize = 48 * 1024
	total := (len(data) + chunkSize - 1) / chunkSize
	for i := 0; i < total; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		_ = ns.sendJSON(WireMsg{
			Type:      "ASSET_CHUNK",
			TargetPub: targetPub,
			Hash:      hash,
			Seq:       i,
			Total:     total,
			Data:      base64.StdEncoding.EncodeToString(data[start:end]),
		})
		time.Sleep(10 * time.Millisecond)
	}
}

func handleIncomingChunk(ns *NetState, msg WireMsg) {
	if !isValidHex64(msg.Hash) || msg.Total <= 0 || msg.Total > 512 || msg.Seq < 0 || msg.Seq >= msg.Total {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		return
	}

	hashLower := strings.ToLower(msg.Hash)

	ns.mu.Lock()
	if !ns.requested[hashLower] && !ns.requested[msg.Hash] && ns.pendingRecvFiles[hashLower] == nil && ns.pendingRecvFiles[msg.Hash] == nil {
		ns.mu.Unlock()
		return
	}

	asm, ok := ns.assemblies[hashLower]
	if !ok {
		asm = &AssetAssembler{
			Total:     msg.Total,
			Chunks:    make(map[int][]byte),
			CreatedAt: time.Now(),
		}
		ns.assemblies[hashLower] = asm
	}
	if asm.Total != msg.Total {
		ns.mu.Unlock()
		return
	}
	asm.Chunks[msg.Seq] = raw
	complete := len(asm.Chunks) == asm.Total
	var full []byte
	if complete {
		for i := 0; i < asm.Total; i++ {
			chunk, exists := asm.Chunks[i]
			if !exists {
				ns.mu.Unlock()
				return
			}
			full = append(full, chunk...)
		}
		delete(ns.assemblies, hashLower)
	}
	ns.mu.Unlock()

	if !complete {
		return
	}

	if !strings.EqualFold(identity.FileSHA256(full), hashLower) {
		log.Printf("Rejected peer asset %s: SHA-256 mismatch", hashLower[:12])
		return
	}
	if err := glbguard.ValidateGLB(full, ns.maxAssetB); err != nil {
		log.Printf("Rejected peer asset %s via glbguard: %v", hashLower[:12], err)
		return
	}

	outPath := filepath.Join(ns.cacheDir, hashLower+".glb")
	if err := os.WriteFile(outPath, full, 0644); err == nil {
		ns.mu.Lock()
		ns.readyModels = append(ns.readyModels, hashLower)
		pending := ns.pendingRecvFiles[hashLower]
		if pending == nil {
			pending = ns.pendingRecvFiles[msg.Hash]
		}
		delete(ns.pendingRecvFiles, hashLower)
		delete(ns.pendingRecvFiles, msg.Hash)
		ns.localAssets[hashLower] = full
		ns.mu.Unlock()

		if pending != nil && pending.License != nil {
			destDir := ns.itemsDir
			if pending.AssetKind == "avatar" {
				destDir = ns.avatarsDir
			}
			safeName := sanitizeFileName(pending.FileName)
			if !strings.HasSuffix(strings.ToLower(safeName), ".glb") {
				safeName = hashLower[:12] + ".glb"
			}
			destGLB := filepath.Join(destDir, safeName)
			_ = os.WriteFile(destGLB, full, 0644)
			_ = identity.SaveAssetLicense(licensePathForGLB(destGLB), pending.License)
			_ = identity.SaveAssetLicense(filepath.Join(ns.cacheDir, hashLower+".license.json"), pending.License)

			ns.mu.Lock()
			ns.invDirty = true
			ns.mu.Unlock()
			ns.addChat(fmt.Sprintf("[TRANSFER]: Saved %s + signed license to your %s inventory!", safeName, pending.AssetKind))
		}
	}
}

func loadWorldDirectory(path, fallbackURL string) WorldDirectory {
	var d WorldDirectory
	b, err := os.ReadFile(path)
	if err == nil {
		_, _ = toml.Decode(string(b), &d)
	}
	if len(d.Worlds) == 0 {
		d.Worlds = append(d.Worlds, struct {
			Name string `toml:"name"`
			URL  string `toml:"url"`
		}{Name: "Default World", URL: fallbackURL})
	}
	return d
}

func saveWorldDirectory(path string, d WorldDirectory) error {
	var sb strings.Builder
	for i, w := range d.Worlds {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "[[worlds]]\nname = %q\nurl = %q\n", w.Name, w.URL)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fetchManifest(rawURL string) (*WorldManifest, string, error) {
	b, err := fetchBytes(rawURL, MaxWorldManifestBytes)
	if err != nil {
		return nil, "", err
	}
	var m WorldManifest
	if _, err := toml.Decode(string(b), &m); err != nil {
		return nil, "", err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", err
	}
	parsed.Path = filepath.ToSlash(filepath.Dir(parsed.Path)) + "/"
	return &m, parsed.String(), nil
}

func fetchBytes(targetURL string, maxBytes int64) ([]byte, error) {
	parsed, err := url.Parse(targetURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("unsupported URL scheme in %q", targetURL)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(targetURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, targetURL)
	}
	limited := io.LimitReader(resp.Body, maxBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("remote resource exceeds %d byte limit", maxBytes)
	}
	return b, nil
}
