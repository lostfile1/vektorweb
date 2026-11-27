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
	"strconv"
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

type WorldBakedProp struct {
	ID    string    `toml:"id"`
	Hash  string    `toml:"hash"`
	Owner string    `toml:"owner"`
	Pos   []float32 `toml:"pos"`
	Yaw   float32   `toml:"yaw"`
	Pitch float32   `toml:"pitch"`
	Roll     float32   `toml:"roll"`
	Scale    float32   `toml:"scale"`
	AnimClip int       `toml:"anim_clip"`
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
	Audio struct {
		StreamURL string `toml:"stream_url"`
		Title     string `toml:"title"`
	} `toml:"audio"`
	Portals []WorldPortal    `toml:"portals"`
	Props   []WorldBakedProp `toml:"props"`
}

type PortalStatusInfo struct {
	Checked   bool
	Online    bool
	Title     string
	PeerCount int
	PropCount int
}

type WorldDirectory struct {
	Worlds []struct {
		Name string `toml:"name"`
		URL  string `toml:"url"`
	} `toml:"worlds"`
}

type OutfitPreset struct {
	Nick       string `toml:"nick"`
	AvatarHash string `toml:"avatar_hash"`
	AvatarName string `toml:"avatar_name"`
}

type ClientSettings struct {
	Nick         string                  `toml:"nick"`
	AvatarHash   string                  `toml:"avatar_hash"`
	AvatarScale  float32                 `toml:"avatar_scale"`
	LastWorld    string                  `toml:"last_world"`
	FOV          float32                 `toml:"fov"`
	MouseSens    float32                 `toml:"mouse_sens"`
	VoiceVolume  float32                 `toml:"voice_volume"`
	AmbientVol   float32                 `toml:"ambient_volume"`
	ShowRadar    bool                    `toml:"show_radar"`
	ShowTags     bool                    `toml:"show_tags"`
	ShowCross    bool                    `toml:"show_crosshair"`
	EnableFog    bool                    `toml:"enable_fog"`
	ShowWaypoint bool                    `toml:"show_waypoint"`
	Outfits      map[string]OutfitPreset `toml:"outfits"`
}

func defaultClientSettings() ClientSettings {
	return ClientSettings{
		AvatarScale:  1.0,
		FOV:          72.0,
		MouseSens:    0.0025,
		VoiceVolume:  1.0,
		AmbientVol:   0.65,
		ShowRadar:    true,
		ShowTags:     true,
		ShowCross:    true,
		EnableFog:    true,
		ShowWaypoint: true,
		Outfits:      make(map[string]OutfitPreset),
	}
}

func loadClientSettings(path string) ClientSettings {
	cfg := defaultClientSettings()
	if b, err := os.ReadFile(path); err == nil {
		_, _ = toml.Decode(string(b), &cfg)
	}
	if cfg.AvatarScale < 0.25 || cfg.AvatarScale > 4.0 {
		cfg.AvatarScale = 1.0
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
	if cfg.AmbientVol < 0 || cfg.AmbientVol > 1.0 {
		cfg.AmbientVol = 0.65
	}
	if cfg.Outfits == nil {
		cfg.Outfits = make(map[string]OutfitPreset)
	}
	return cfg
}

func saveClientSettings(path string, cfg ClientSettings) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "nick = %q\n", cfg.Nick)
	fmt.Fprintf(&sb, "avatar_hash = %q\n", cfg.AvatarHash)
	fmt.Fprintf(&sb, "avatar_scale = %.2f\n", cfg.AvatarScale)
	fmt.Fprintf(&sb, "last_world = %q\n", cfg.LastWorld)
	fmt.Fprintf(&sb, "fov = %.1f\n", cfg.FOV)
	fmt.Fprintf(&sb, "mouse_sens = %.4f\n", cfg.MouseSens)
	fmt.Fprintf(&sb, "voice_volume = %.2f\n", cfg.VoiceVolume)
	fmt.Fprintf(&sb, "ambient_volume = %.2f\n", cfg.AmbientVol)
	fmt.Fprintf(&sb, "show_radar = %v\n", cfg.ShowRadar)
	fmt.Fprintf(&sb, "show_tags = %v\n", cfg.ShowTags)
	fmt.Fprintf(&sb, "show_crosshair = %v\n", cfg.ShowCross)
	fmt.Fprintf(&sb, "enable_fog = %v\n", cfg.EnableFog)
	fmt.Fprintf(&sb, "show_waypoint = %v\n", cfg.ShowWaypoint)
	if len(cfg.Outfits) > 0 {
		slots := make([]string, 0, len(cfg.Outfits))
		for k := range cfg.Outfits {
			slots = append(slots, k)
		}
		sort.Strings(slots)
		for _, slot := range slots {
			op := cfg.Outfits[slot]
			fmt.Fprintf(&sb, "\n[outfits.%q]\nnick = %q\navatar_hash = %q\navatar_name = %q\n",
				slot, op.Nick, op.AvatarHash, op.AvatarName)
		}
	}
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
	AnimClip     int       `json:"anim_clip,omitempty"`
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
	AvatarHash  string
	AnimClip    int
	AvatarScale float32
	LastSeen    time.Time
	Speaking   time.Time
}

type WorldProp struct {
	PropID string
	Owner  string
	Hash   string
	Pos    rl.Vector3
	Yaw    float32
	Pitch  float32
	Roll     float32
	Scale    float32
	AnimClip int
}

type InventoryItem struct {
	Name      string
	Path      string
	Hash      string
	Kind      string
	SizeBytes int64
	Bounds    [3]float32
	HasBounds bool
	License   *identity.AssetLicense
	StatusTag string
	CanSend   bool
	IsCreator bool
}

func formatByteSize(b int64) string {
	if b >= 1024*1024 {
		return fmt.Sprintf("%.2f MB", float64(b)/(1024.0*1024.0))
	}
	if b >= 1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024.0)
	}
	return fmt.Sprintf("%d B", b)
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
	ClipIdx    int
	Frame      int32
	LastUpdate float64
}

func poseModelAnimClip(m rl.Model, animSet *ModelAnimSet, clipIdx int, phaseOffset int32) {
	if animSet == nil || len(animSet.Anims) == 0 {
		return
	}
	if clipIdx < 0 {
		anim := animSet.Anims[0]
		if anim.KeyframeCount > 0 {
			rl.UpdateModelAnimation(m, anim, 0)
		}
		return
	}
	if clipIdx >= len(animSet.Anims) {
		clipIdx = 0
	}
	anim := animSet.Anims[clipIdx]
	if anim.KeyframeCount <= 0 {
		return
	}
	frame := (animSet.Frame + phaseOffset) % anim.KeyframeCount
	if frame < 0 {
		frame += anim.KeyframeCount
	}
	rl.UpdateModelAnimation(m, anim, float32(frame))
}

func populateInventoryBounds(items []InventoryItem, models map[string]rl.Model) {
	for i := range items {
		if items[i].HasBounds {
			continue
		}
		m, ok := models[items[i].Hash]
		if !ok || m.MeshCount <= 0 {
			continue
		}
		bb := rl.GetModelBoundingBox(m)
		w := bb.Max.X - bb.Min.X
		h := bb.Max.Y - bb.Min.Y
		d := bb.Max.Z - bb.Min.Z
		if !math.IsNaN(float64(w)) && !math.IsNaN(float64(h)) && !math.IsNaN(float64(d)) &&
			!math.IsInf(float64(w), 0) && !math.IsInf(float64(h), 0) && !math.IsInf(float64(d), 0) {
			items[i].Bounds = [3]float32{w, h, d}
			items[i].HasBounds = true
		}
	}
}

type ModelMetrics struct {
	FeetOffsetY float32
	BaseScale   float32
}

func computeModelMetrics(m rl.Model) ModelMetrics {
	met := ModelMetrics{BaseScale: 1.0}
	if m.MeshCount <= 0 {
		return met
	}
	bb := rl.GetModelBoundingBox(m)
	if !math.IsNaN(float64(bb.Min.Y)) && !math.IsInf(float64(bb.Min.Y), 0) {
		met.FeetOffsetY = -bb.Min.Y
	}
	extY := bb.Max.Y - bb.Min.Y
	extX := bb.Max.X - bb.Min.X
	extZ := bb.Max.Z - bb.Min.Z
	if math.IsNaN(float64(extY)) || math.IsInf(float64(extY), 0) {
		return met
	}
	refDim := extY
	if extX > refDim*1.6 {
		refDim = extX * 0.75
	}
	if extZ > refDim*1.6 {
		refDim = extZ * 0.75
	}
	if refDim > 2.4 || (refDim > 0.001 && refDim < 0.45) {
		met.BaseScale = 1.75 / refDim
	}
	return met
}

func getAvatarMetrics(metrics map[string]ModelMetrics, hash string, m rl.Model) ModelMetrics {
	if met, ok := metrics[hash]; ok {
		return met
	}
	met := computeModelMetrics(m)
	metrics[hash] = met
	return met
}

func drawModelEuler(m rl.Model, pos rl.Vector3, yawDeg, pitchDeg, rollDeg, scale float32, tint rl.Color) {
	if scale <= 0 {
		scale = 1.0
	}
	if math.Abs(float64(pitchDeg)) < 0.01 && math.Abs(float64(rollDeg)) < 0.01 {
		rl.DrawModelEx(m, pos, rl.NewVector3(0, 1, 0), yawDeg, rl.NewVector3(scale, scale, scale), tint)
		return
	}
	const deg2rad = float32(math.Pi / 180.0)
	orig := m.Transform
	rot := rl.MatrixRotateXYZ(rl.NewVector3(pitchDeg*deg2rad, yawDeg*deg2rad, rollDeg*deg2rad))
	m.Transform = rl.MatrixMultiply(orig, rot)
	rl.DrawModelEx(m, pos, rl.NewVector3(0, 1, 0), 0, rl.NewVector3(scale, scale, scale), tint)
	m.Transform = orig
}

type AmbientRadioPlayer struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	url     string
	muted   bool
	vol     float32
}

func (a *AmbientRadioPlayer) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
		_ = a.cmd.Wait()
	}
	a.cmd = nil
	a.url = ""
}

func (a *AmbientRadioPlayer) Start(streamURL string, vol float32) (string, bool) {
	a.Stop()
	clean := strings.TrimSpace(streamURL)
	if clean == "" {
		return "", false
	}
	u, err := url.Parse(clean)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	a.mu.Lock()
	a.vol = vol
	if a.muted || vol <= 0.01 {
		a.url = clean
		a.mu.Unlock()
		return clean, true
	}
	pct := int(vol * 100)
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	var cmd *exec.Cmd
	if _, err := exec.LookPath("mpv"); err == nil {
		cmd = exec.Command("mpv", "--no-video", "--really-quiet", fmt.Sprintf("--volume=%d", pct), clean)
	} else if _, err := exec.LookPath("ffplay"); err == nil {
		cmd = exec.Command("ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", "-volume", strconv.Itoa(pct), clean)
	} else if _, err := exec.LookPath("mpg123"); err == nil {
		cmd = exec.Command("mpg123", "-q", clean)
	}
	if cmd == nil {
		a.mu.Unlock()
		return "", false
	}
	if err := cmd.Start(); err != nil {
		a.mu.Unlock()
		return "", false
	}
	a.cmd = cmd
	a.url = clean
	a.mu.Unlock()
	go func(c *exec.Cmd) { _ = c.Wait() }(cmd)
	return clean, true
}

type RayTarget struct {
	Kind      string // "prop" | "peer"
	ID        string // propID or peer PubKey
	PropHash  string
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
	case strings.HasPrefix(line, "[WORLD]"), strings.HasPrefix(line, "[PORTAL]"), strings.HasPrefix(line, "[MOTD]"), strings.HasPrefix(line, "[RELAY]"):
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

type starterMeshBuilder struct {
	pos  []float32
	norm []float32
	col  []float32
	idx  []uint16
	minV [3]float32
	maxV [3]float32
}

func newStarterMeshBuilder() *starterMeshBuilder {
	return &starterMeshBuilder{
		minV: [3]float32{1e9, 1e9, 1e9},
		maxV: [3]float32{-1e9, -1e9, -1e9},
	}
}

func (mb *starterMeshBuilder) addQuad(p0, p1, p2, p3, n [3]float32, c [4]float32) {
	base := uint16(len(mb.pos) / 3)
	for _, p := range [4][3]float32{p0, p1, p2, p3} {
		mb.pos = append(mb.pos, p[0], p[1], p[2])
		mb.norm = append(mb.norm, n[0], n[1], n[2])
		mb.col = append(mb.col, c[0], c[1], c[2], c[3])
		for k := 0; k < 3; k++ {
			if p[k] < mb.minV[k] {
				mb.minV[k] = p[k]
			}
			if p[k] > mb.maxV[k] {
				mb.maxV[k] = p[k]
			}
		}
	}
	mb.idx = append(mb.idx, base, base+1, base+2, base, base+2, base+3)
}

func (mb *starterMeshBuilder) addBox(cx, cy, cz, sx, sy, sz float32, cTop, cSide [4]float32) {
	hx, hy, hz := sx*0.5, sy*0.5, sz*0.5
	x0, x1 := cx-hx, cx+hx
	y0, y1 := cy-hy, cy+hy
	z0, z1 := cz-hz, cz+hz
	mb.addQuad([3]float32{x0, y1, z1}, [3]float32{x1, y1, z1}, [3]float32{x1, y1, z0}, [3]float32{x0, y1, z0}, [3]float32{0, 1, 0}, cTop)
	mb.addQuad([3]float32{x0, y0, z0}, [3]float32{x1, y0, z0}, [3]float32{x1, y0, z1}, [3]float32{x0, y0, z1}, [3]float32{0, -1, 0}, cSide)
	mb.addQuad([3]float32{x0, y0, z1}, [3]float32{x1, y0, z1}, [3]float32{x1, y1, z1}, [3]float32{x0, y1, z1}, [3]float32{0, 0, 1}, cSide)
	mb.addQuad([3]float32{x1, y0, z0}, [3]float32{x0, y0, z0}, [3]float32{x0, y1, z0}, [3]float32{x1, y1, z0}, [3]float32{0, 0, -1}, cSide)
	mb.addQuad([3]float32{x1, y0, z1}, [3]float32{x1, y0, z0}, [3]float32{x1, y1, z0}, [3]float32{x1, y1, z1}, [3]float32{1, 0, 0}, cSide)
	mb.addQuad([3]float32{x0, y0, z0}, [3]float32{x0, y0, z1}, [3]float32{x0, y1, z1}, [3]float32{x0, y1, z0}, [3]float32{-1, 0, 0}, cSide)
}

func (mb *starterMeshBuilder) encodeGLB() []byte {
	vCount := len(mb.pos) / 3
	pBytes := make([]byte, len(mb.pos)*4)
	for i, v := range mb.pos {
		binary.LittleEndian.PutUint32(pBytes[i*4:], math.Float32bits(v))
	}
	nBytes := make([]byte, len(mb.norm)*4)
	for i, v := range mb.norm {
		binary.LittleEndian.PutUint32(nBytes[i*4:], math.Float32bits(v))
	}
	cBytes := make([]byte, len(mb.col)*4)
	for i, v := range mb.col {
		binary.LittleEndian.PutUint32(cBytes[i*4:], math.Float32bits(v))
	}
	iBytes := make([]byte, len(mb.idx)*2)
	for i, id := range mb.idx {
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
			map[string]int{"buffer": 0, "byteOffset": iOff, "byteLength": len(mb.idx) * 2, "target": 34963},
		},
		"accessors": []any{
			map[string]any{"bufferView": 0, "componentType": 5126, "count": vCount, "type": "VEC3", "min": []float32{mb.minV[0], mb.minV[1], mb.minV[2]}, "max": []float32{mb.maxV[0], mb.maxV[1], mb.maxV[2]}},
			map[string]any{"bufferView": 1, "componentType": 5126, "count": vCount, "type": "VEC3"},
			map[string]any{"bufferView": 2, "componentType": 5126, "count": vCount, "type": "VEC4"},
			map[string]any{"bufferView": 3, "componentType": 5123, "count": len(mb.idx), "type": "SCALAR"},
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

func buildStarterAvatarGLB(style string) []byte {
	mb := newStarterMeshBuilder()
	var primary, second, accent, dark [4]float32
	switch style {
	case "nomad":
		primary = [4]float32{0.25, 0.20, 0.14, 1.0}
		second = [4]float32{0.36, 0.29, 0.20, 1.0}
		accent = [4]float32{0.96, 0.72, 0.18, 1.0}
		dark = [4]float32{0.11, 0.09, 0.07, 1.0}
	case "oracle":
		primary = [4]float32{0.24, 0.13, 0.17, 1.0}
		second = [4]float32{0.34, 0.18, 0.24, 1.0}
		accent = [4]float32{0.96, 0.26, 0.42, 1.0}
		dark = [4]float32{0.11, 0.06, 0.08, 1.0}
	default: // "sentinel"
		primary = [4]float32{0.14, 0.20, 0.29, 1.0}
		second = [4]float32{0.22, 0.29, 0.40, 1.0}
		accent = [4]float32{0.15, 0.86, 0.96, 1.0}
		dark = [4]float32{0.07, 0.10, 0.15, 1.0}
	}

	// Boots & Legs (Y: 0.00 -> 0.78)
	mb.addBox(-0.12, 0.07, 0.03, 0.14, 0.14, 0.19, second, dark)
	mb.addBox(0.12, 0.07, 0.03, 0.14, 0.14, 0.19, second, dark)
	mb.addBox(-0.12, 0.46, 0, 0.12, 0.64, 0.12, primary, dark)
	mb.addBox(0.12, 0.46, 0, 0.12, 0.64, 0.12, primary, dark)

	// Pelvis, Torso & Chest Core (Y: 0.78 -> 1.34)
	mb.addBox(0, 0.84, 0, 0.42, 0.14, 0.24, dark, dark)
	mb.addBox(0, 1.12, 0, 0.46, 0.44, 0.26, primary, second)
	mb.addBox(0, 1.15, 0.145, 0.20, 0.18, 0.03, accent, accent)
	mb.addBox(0, 1.14, -0.18, 0.26, 0.30, 0.12, second, dark)

	// Shoulders & Arms (Y: 0.80 -> 1.32)
	mb.addBox(-0.31, 1.26, 0, 0.18, 0.16, 0.18, second, accent)
	mb.addBox(0.31, 1.26, 0, 0.18, 0.16, 0.18, second, accent)
	mb.addBox(-0.31, 0.98, 0, 0.11, 0.42, 0.11, primary, dark)
	mb.addBox(0.31, 0.98, 0, 0.11, 0.42, 0.11, primary, dark)

	// Neck & Head (Y: 1.34 -> 1.70)
	mb.addBox(0, 1.37, 0, 0.14, 0.08, 0.14, dark, dark)
	mb.addBox(0, 1.54, 0, 0.30, 0.28, 0.30, primary, second)
	mb.addBox(0, 1.55, 0.16, 0.26, 0.08, 0.03, accent, accent)

	switch style {
	case "nomad":
		mb.addBox(0, 1.72, 0, 0.06, 0.10, 0.32, accent, accent)
	case "oracle":
		mb.addBox(0, 1.75, 0, 0.34, 0.03, 0.04, accent, accent)
		mb.addBox(0, 1.75, 0, 0.04, 0.03, 0.34, accent, accent)
	default:
		mb.addBox(-0.17, 1.55, 0, 0.04, 0.12, 0.12, second, accent)
		mb.addBox(0.17, 1.55, 0, 0.04, 0.12, 0.12, second, accent)
	}
	return mb.encodeGLB()
}

func buildDefaultGLB(w, h, d float32) []byte {
	mb := newStarterMeshBuilder()
	mb.addBox(0, h*0.5, 0, w, h, d,
		[4]float32{0.20, 0.28, 0.39, 1.0},
		[4]float32{0.12, 0.17, 0.25, 1.0})
	return mb.encodeGLB()
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
		m.Rules.MaxAvatarMB = 24
		m.Rules.MaxItemMB = 20
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

func worldOnlyMarkerPath(cacheDir, hash string) string {
	return filepath.Join(cacheDir, strings.ToLower(hash)+".world_only")
}

func isWorldOnlyAsset(cacheDir, hash string) bool {
	if !isValidHex64(hash) {
		return false
	}
	_, err := os.Stat(worldOnlyMarkerPath(cacheDir, hash))
	return err == nil
}

func licensePathForGLB(glbPath string) string {
	ext := filepath.Ext(glbPath)
	if ext != "" {
		return strings.TrimSuffix(glbPath, ext) + ".license.json"
	}
	return glbPath + ".license.json"
}

var verboseLog bool

func vlogf(format string, args ...any) {
	if verboseLog {
		log.Printf("[VERBOSE] "+format, args...)
	}
}

func scanInventoryFolder(folder, kind, cacheDir, localPubKey string, maxBytes int, localAssets map[string][]byte) []InventoryItem {
	var out []InventoryItem
	entries, err := os.ReadDir(folder)
	if err != nil {
		log.Printf("Inventory scan failed on %s: %v", folder, err)
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".glb") {
			continue
		}
		fullPath := filepath.Join(folder, e.Name())
		b, err := os.ReadFile(fullPath)
		if err != nil {
			log.Printf("Skipping %s (%s): read error: %v", e.Name(), kind, err)
			continue
		}
		if err := glbguard.ValidateGLB(b, maxBytes); err != nil {
			log.Printf("Skipping %s (%s, %s): rejected by glbguard/size limit (%v)", e.Name(), kind, formatByteSize(int64(len(b))), err)
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
			_ = os.Remove(worldOnlyMarkerPath(cacheDir, h))
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
		} else if isWorldOnlyAsset(cacheDir, h) {
			log.Printf("Skipping %s: display-only world/peer asset (no ownership license for %s)", e.Name(), localPubKey[:8])
			continue
		}

		localAssets[h] = b
		_ = os.WriteFile(filepath.Join(cacheDir, h+".glb"), b, 0644)
		out = append(out, InventoryItem{
			Name:      e.Name(),
			Path:      fullPath,
			Hash:      h,
			Kind:      kind,
			SizeBytes: int64(len(b)),
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
	var fallback rl.Shader
	if !enable || sh.ID == 0 {
		fallback = rl.LoadShader("", "")
	}
	for i := range mats {
		if enable && sh.ID > 0 {
			mats[i].Shader = sh
		} else {
			mats[i].Shader = fallback
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

func modelTotalVerts(m rl.Model) int {
	if m.MeshCount <= 0 || m.Meshes == nil {
		return 0
	}
	meshes := unsafe.Slice(m.Meshes, m.MeshCount)
	total := 0
	for i := range meshes {
		total += int(meshes[i].VertexCount)
	}
	return total
}

func loadModelWithAnims(glbPath string, sh rl.Shader, useFog bool) (rl.Model, *ModelAnimSet) {
	m := rl.LoadModel(glbPath)
	if useFog && sh.ID > 0 {
		applyShaderToModel(m, sh, true)
	}
	verts := modelTotalVerts(m)
	hasAnim := glbHasAnimations(glbPath)
	if hasAnim && verts <= 80000 {
		var anims []rl.ModelAnimation
		func() {
			defer func() { _ = recover() }()
			anims = rl.LoadModelAnimations(glbPath)
		}()
		if len(anims) > 0 {
			vlogf("Loaded animated model %s (meshes=%d verts=%d clips=%d)", filepath.Base(glbPath), m.MeshCount, verts, len(anims))
			return m, &ModelAnimSet{Anims: anims, Frame: 0, LastUpdate: rl.GetTime()}
		}
	} else if hasAnim && verts > 80000 {
		log.Printf("Skipped skeletal animations on %s: vertex count %d exceeds 80000 safety threshold", filepath.Base(glbPath), verts)
	}
	vlogf("Loaded static model %s (meshes=%d verts=%d)", filepath.Base(glbPath), m.MeshCount, verts)
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
	verboseFlag := flag.Bool("verbose", false, "Enable verbose P2P asset, model, and network debug logging")
	flag.Parse()
	verboseLog = *verboseFlag
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	worldExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "world" {
			worldExplicit = true
		}
	})

	targetWorld := *worldURL
	if flag.NArg() > 0 && flag.Arg(0) != "" {
		targetWorld = flag.Arg(0)
		worldExplicit = true
	}

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

	clientLogPath := filepath.Join(baseDir, "client.log")
	if st, err := os.Stat(clientLogPath); err == nil && st.Size() > 5*1024*1024 {
		_ = os.Rename(clientLogPath, clientLogPath+".1")
	}
	if lf, err := os.OpenFile(clientLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, lf))
		defer lf.Close()
	}
	log.Printf("Starting vektorweb-client (%s/%s, go=%s, dir=%s, verbose=%v)",
		runtime.GOOS, runtime.GOARCH, runtime.Version(), baseDir, verboseLog)

	settingsPath := filepath.Join(baseDir, "settings.toml")
	_, settingsStatErr := os.Stat(settingsPath)
	isFirstRun := os.IsNotExist(settingsStatErr)
	cfg := loadClientSettings(settingsPath)
	if isFirstRun {
		saveClientSettings(settingsPath, cfg)
	}

	if !worldExplicit {
		if savedW := strings.TrimSpace(cfg.LastWorld); savedW != "" && normalizeWorldURL(savedW) != "local://sandbox" {
			targetWorld = savedW
		} else if preDir := loadWorldDirectory(filepath.Join(baseDir, "worlds.toml"), ""); len(preDir.Worlds) > 0 && strings.TrimSpace(preDir.Worlds[0].URL) != "" {
			targetWorld = strings.TrimSpace(preDir.Worlds[0].URL)
		}
	}
	targetWorld = normalizeWorldURL(targetWorld)

	kp, err := identity.LoadOrCreate(baseDir)
	if err != nil {
		log.Fatalf("identity error: %v", err)
	}
	log.Printf("Loaded identity pubkey=%s... targetWorld=%s", kp.PubHex()[:12], targetWorld)
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

	defaultAvPath := filepath.Join(avatarsDir, "sentinel_cyan.glb")
	starterAvatars := []struct {
		file  string
		style string
	}{
		{"sentinel_cyan.glb", "sentinel"},
		{"nomad_amber.glb", "nomad"},
		{"oracle_crimson.glb", "oracle"},
	}
	for _, sa := range starterAvatars {
		p := filepath.Join(avatarsDir, sa.file)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			_ = os.WriteFile(p, buildStarterAvatarGLB(sa.style), 0644)
		}
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
	} else {
		log.Printf("Loaded world %q (%s) placement=%s max_avatar=%dMB max_item=%dMB",
			activeWorld.Manifest.World.Title, activeWorld.URL, activeWorld.Manifest.Rules.PlacementMode,
			activeWorld.Manifest.Rules.MaxAvatarMB, activeWorld.Manifest.Rules.MaxItemMB)
	}
	if activeWorld.URL != "" && activeWorld.URL != "local://sandbox" && cfg.LastWorld != activeWorld.URL {
		cfg.LastWorld = activeWorld.URL
		saveClientSettings(settingsPath, cfg)
	}

	maxAvMB := activeWorld.Manifest.Rules.MaxAvatarMB
	if maxAvMB <= 0 {
		maxAvMB = 24
	}
	if maxAvMB > 24 {
		maxAvMB = 24
	}
	maxItemMB := activeWorld.Manifest.Rules.MaxItemMB
	if maxItemMB <= 0 {
		maxItemMB = 20
	}
	if maxItemMB > 24 {
		maxItemMB = 24
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

	localAvHash := "none"
	var localAvBytes []byte
	tryEquipAvatarBytes := func(b []byte, sidecarPath string) bool {
		if err := glbguard.ValidateGLB(b, maxAvMB*1024*1024); err != nil {
			return false
		}
		h := identity.FileSHA256(b)
		if sidecarPath != "" {
			if lic, err := identity.LoadAssetLicense(sidecarPath); err == nil {
				if err := lic.Verify(h, kp.PubHex()); err != nil {
					log.Printf("Avatar license rejected: %v", err)
					return false
				}
				_ = os.Remove(worldOnlyMarkerPath(cacheDir, h))
			}
		}
		if lic, err := identity.LoadAssetLicense(filepath.Join(cacheDir, h+".license.json")); err == nil {
			if err := lic.Verify(h, kp.PubHex()); err != nil {
				log.Printf("Cached avatar license rejected: %v", err)
				return false
			}
			_ = os.Remove(worldOnlyMarkerPath(cacheDir, h))
		} else if isWorldOnlyAsset(cacheDir, h) {
			log.Printf("Avatar rejected: display-only world/peer asset")
			return false
		}
		localAvHash = h
		localAvBytes = b
		localAssets[localAvHash] = b
		_ = os.WriteFile(filepath.Join(cacheDir, localAvHash+".glb"), b, 0644)
		return true
	}

	if *avatarFile != "" {
		if b, err := os.ReadFile(*avatarFile); err == nil {
			if tryEquipAvatarBytes(b, licensePathForGLB(*avatarFile)) && cfg.AvatarHash != localAvHash {
				cfg.AvatarHash = localAvHash
				saveClientSettings(settingsPath, cfg)
			}
		}
	} else {
		savedAv := strings.ToLower(strings.TrimSpace(cfg.AvatarHash))
		if isValidHex64(savedAv) {
			for _, it := range avatarInv {
				if strings.EqualFold(it.Hash, savedAv) {
					if b, ok := localAssets[it.Hash]; ok && tryEquipAvatarBytes(b, licensePathForGLB(it.Path)) {
						break
					}
				}
			}
			if localAvHash == "none" {
				if b, err := os.ReadFile(filepath.Join(cacheDir, savedAv+".glb")); err == nil {
					_ = tryEquipAvatarBytes(b, "")
				}
			}
		}
	}
	if localAvHash == "none" {
		if b, err := os.ReadFile(defaultAvPath); err == nil {
			_ = tryEquipAvatarBytes(b, licensePathForGLB(defaultAvPath))
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

	seedBakedProps := func(w *LoadedWorld) {
		if w == nil || w.Manifest == nil {
			return
		}
		var toFetch []string
		netState.mu.Lock()
		for idx, bp := range w.Manifest.Props {
			hLower := strings.ToLower(strings.TrimSpace(bp.Hash))
			if !isValidHex64(hLower) || !finiteVec3(bp.Pos) {
				continue
			}
			propID := sanitizeDisplayText(strings.TrimSpace(bp.ID), 48)
			if propID == "" {
				propID = fmt.Sprintf("baked_%d_%s", idx, hLower[:8])
			} else if !strings.HasPrefix(propID, "baked_") {
				propID = "baked_" + propID
			}
			scale := bp.Scale
			if scale <= 0 {
				scale = 1.0
			}
			owner := strings.ToLower(strings.TrimSpace(bp.Owner))
			if !isValidHex64(owner) {
				owner = "0000000000000000000000000000000000000000000000000000000000000000"
			}
			netState.props[propID] = &WorldProp{
				PropID: propID,
				Owner:  owner,
				Hash:   hLower,
				Pos:    rl.NewVector3(bp.Pos[0], bp.Pos[1], bp.Pos[2]),
				Yaw:    bp.Yaw,
				Pitch:  bp.Pitch,
				Roll:     bp.Roll,
				Scale:    scale,
				AnimClip: bp.AnimClip,
			}
			if ensureAssetQueued(netState, hLower) {
				toFetch = append(toFetch, hLower)
			}
		}
		netState.mu.Unlock()

		// Also try fetching baked prop .glb files directly over HTTP relative to world.toml if not cached
		if w.URL != "" && w.URL != "local://sandbox" && len(toFetch) > 0 {
			parsed, err := url.Parse(w.URL)
			if err == nil {
				parsed.Path = filepath.ToSlash(filepath.Dir(parsed.Path)) + "/"
				baseURL := parsed.String()
				for _, h := range toFetch {
					go func(hashHex string) {
						glbURL := strings.TrimRight(baseURL, "/") + "/" + hashHex + ".glb"
						b, err := fetchBytes(glbURL, int64(maxItemMB)*1024*1024)
						if err != nil {
							_ = netState.sendJSON(WireMsg{Type: "REQ_ASSET", Hash: hashHex})
							return
						}
						if !strings.EqualFold(identity.FileSHA256(b), hashHex) {
							return
						}
						if glbguard.ValidateGLB(b, maxItemMB*1024*1024) != nil {
							return
						}
						_ = os.WriteFile(filepath.Join(cacheDir, hashHex+".glb"), b, 0644)
						if _, err := os.Stat(filepath.Join(cacheDir, hashHex+".license.json")); os.IsNotExist(err) {
							_ = os.WriteFile(worldOnlyMarkerPath(cacheDir, hashHex), []byte("display_only\n"), 0644)
						}
						netState.mu.Lock()
						netState.localAssets[hashHex] = b
						netState.readyModels = append(netState.readyModels, hashHex)
						netState.mu.Unlock()
					}(h)
				}
			}
		}
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
	ambientRadio := &AmbientRadioPlayer{vol: cfg.AmbientVol}
	defer ambientRadio.Stop()
	startWorldAudio := func(w *LoadedWorld) {
		ambientRadio.Stop()
		if w != nil && strings.TrimSpace(w.Manifest.Audio.StreamURL) != "" {
			if _, ok := ambientRadio.Start(w.Manifest.Audio.StreamURL, cfg.AmbientVol); ok {
				label := sanitizeDisplayText(w.Manifest.Audio.Title, 40)
				if label == "" {
					label = sanitizeDisplayText(w.Manifest.Audio.StreamURL, 48)
				}
				netState.addChat("[RADIO]: Streaming world audio -> " + label + " ([U] mute/unmute)")
			}
		}
	}

	startRelayForWorld(activeWorld)
	seedBakedProps(activeWorld)
	startWorldAudio(activeWorld)

	rl.SetConfigFlags(rl.FlagWindowResizable | rl.FlagMsaa4xHint)
	rl.InitWindow(1280, 720, "VektorWeb - "+activeWorld.Manifest.World.Title)
	defer rl.CloseWindow()
	uiFont, hasUIFont = loadBestUIFont(baseDir)
	if hasUIFont {
		defer rl.UnloadFont(uiFont)
	}
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

	invPreviewRT := rl.LoadRenderTexture(260, 260)
	defer rl.UnloadRenderTexture(invPreviewRT)

	loadedModels := make(map[string]rl.Model)
	loadedAnims := make(map[string]*ModelAnimSet)
	modelMetrics := make(map[string]ModelMetrics)
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
	populateInventoryBounds(propInv, loadedModels)
	populateInventoryBounds(avatarInv, loadedModels)
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
	ghostActive := false
	var ownedPropIDs []string
	nearProps := make(map[string]bool)
	visitedWorlds := []string{activeWorld.URL}
	var hopToWorld func(targetURL string, recordHistory bool)
	hopToWorld = func(targetURL string, recordHistory bool) {
		targetURL = normalizeWorldURL(targetURL)
		prevURL := activeWorld.URL
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
		if activeWorld.Manifest.Rules.MaxAvatarMB > 0 {
			maxAvMB = activeWorld.Manifest.Rules.MaxAvatarMB
		} else {
			maxAvMB = 24
		}
		if maxAvMB > 24 {
			maxAvMB = 24
		}
		if activeWorld.Manifest.Rules.MaxItemMB > 0 {
			maxItemMB = activeWorld.Manifest.Rules.MaxItemMB
		} else {
			maxItemMB = 20
		}
		if maxItemMB > 24 {
			maxItemMB = 24
		}
		netState.mu.Lock()
		netState.maxAssetB = maxAvMB * 1024 * 1024
		netState.mu.Unlock()
		ghostActive = false
		ownedPropIDs = nil
		nearProps = make(map[string]bool)

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
		if recordHistory && prevURL != "" && normalizeWorldURL(prevURL) != activeWorld.URL {
			if len(visitedWorlds) == 0 || normalizeWorldURL(visitedWorlds[len(visitedWorlds)-1]) != normalizeWorldURL(prevURL) {
				visitedWorlds = append(visitedWorlds, prevURL)
				if len(visitedWorlds) > 16 {
					visitedWorlds = visitedWorlds[len(visitedWorlds)-16:]
				}
			}
		}
		if activeWorld.URL != "" && activeWorld.URL != "local://sandbox" && cfg.LastWorld != activeWorld.URL {
			cfg.LastWorld = activeWorld.URL
			saveClientSettings(settingsPath, cfg)
		}
		startRelayForWorld(activeWorld)
		seedBakedProps(activeWorld)
		startWorldAudio(activeWorld)
		netState.addChat("[PORTAL]: Arrived at " + activeWorld.Manifest.World.Title)
	}

	chatActive := false
	dirOpen := false
	var dirStatusMu sync.RWMutex
	dirStatuses := make(map[string]PortalStatusInfo)
	refreshPortalStatuses := func(dir WorldDirectory) {
		// Discover live announced worlds from active world's /directory.json if available
		if activeWorld != nil && activeWorld.URL != "" && activeWorld.URL != "local://sandbox" {
			if u, err := url.Parse(normalizeWorldURL(activeWorld.URL)); err == nil {
				u.Path = "/directory.json"
				u.RawQuery = ""
				go func(dirEndpoint string) {
					httpClient := &http.Client{Timeout: 4 * time.Second}
					resp, err := httpClient.Get(dirEndpoint)
					if err != nil || resp.StatusCode != http.StatusOK {
						if resp != nil {
							_ = resp.Body.Close()
						}
						return
					}
					defer resp.Body.Close()
					var payload struct {
						Worlds []struct {
							Title     string `json:"title"`
							WorldURL  string `json:"world_url"`
							Online    int    `json:"online"`
							Props     int    `json:"props"`
						} `json:"worlds"`
					}
					if json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&payload) == nil {
						for _, aw := range payload.Worlds {
							wu := strings.TrimSpace(aw.WorldURL)
							if wu == "" {
								continue
							}
							cleanTitle := sanitizeDisplayText(aw.Title, 40)
							dirStatusMu.Lock()
							dirStatuses[wu] = PortalStatusInfo{
								Checked:   true,
								Online:    true,
								Title:     cleanTitle,
								PeerCount: aw.Online,
								PropCount: aw.Props,
							}
							existsInDir := false
							for _, existingW := range directory.Worlds {
								if normalizeWorldURL(existingW.URL) == normalizeWorldURL(wu) {
									existsInDir = true
									break
								}
							}
							if !existsInDir && len(directory.Worlds) < 8 {
								directory.Worlds = append(directory.Worlds, struct {
									Name string `toml:"name"`
									URL  string `toml:"url"`
								}{Name: cleanTitle, URL: wu})
							}
							dirStatusMu.Unlock()
						}
					}
				}(u.String())
			}
		}
		for _, w := range dir.Worlds {
			rawU := strings.TrimSpace(w.URL)
			if rawU == "" {
				continue
			}
			normU := normalizeWorldURL(rawU)
			if normU == "local://sandbox" {
				dirStatusMu.Lock()
				dirStatuses[rawU] = PortalStatusInfo{
					Checked: true,
					Online:  true,
					Title:   "Offline Sandbox",
				}
				dirStatusMu.Unlock()
				continue
			}
			dirStatusMu.Lock()
			dirStatuses[rawU] = PortalStatusInfo{Checked: false}
			dirStatusMu.Unlock()

			go func(origURL, httpWorldURL string) {
				u, err := url.Parse(httpWorldURL)
				if err != nil {
					return
				}
				u.Path = "/status.json"
				u.RawQuery = ""
				httpClient := &http.Client{Timeout: 4 * time.Second}
				resp, err := httpClient.Get(u.String())
				if err != nil {
					dirStatusMu.Lock()
					dirStatuses[origURL] = PortalStatusInfo{Checked: true, Online: false}
					dirStatusMu.Unlock()
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					dirStatusMu.Lock()
					dirStatuses[origURL] = PortalStatusInfo{Checked: true, Online: false}
					dirStatusMu.Unlock()
					return
				}
				var st struct {
					Title  string `json:"title"`
					Online int    `json:"online"`
					Props  int    `json:"props"`
				}
				if err := json.NewDecoder(io.LimitReader(resp.Body, 32*1024)).Decode(&st); err != nil {
					dirStatusMu.Lock()
					dirStatuses[origURL] = PortalStatusInfo{Checked: true, Online: false}
					dirStatusMu.Unlock()
					return
				}
				dirStatusMu.Lock()
				dirStatuses[origURL] = PortalStatusInfo{
					Checked:   true,
					Online:    true,
					Title:     sanitizeDisplayText(st.Title, 40),
					PeerCount: st.Online,
					PropCount: st.Props,
				}
				dirStatusMu.Unlock()
			}(rawU, normU)
		}
	}
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
	snapToGrid := false
	ghostYawOffset := float32(0)
	ghostPitchOffset := float32(0)
	ghostRollOffset := float32(0)
	ghostYOffset := float32(0)
	ghostScale := float32(1.0)
	ghostAnimClip := 0
	localManualClip := -1
	localActiveClip := 0
	showRangeRing := false
	dirCursor := 0
	dirConnectActive := false
	dirConnectInput := ""
	welcomeOpen := isFirstRun

	chatInput := ""
	var chatHistory []string
	chatHistIdx := -1
	chatDraft := ""
	luaHUD := ""
	lastSync := time.Now()
	lastJanitor := time.Now()
	lastActivity := time.Now()
	isAFK := false

	var pendingDrop *PendingDrop
	type dialogPickResult struct {
		path string
		kind string
	}
	pickedGLBCh := make(chan dialogPickResult, 2)
	var dialogBusyMu sync.Mutex
	dialogBusy := false
	launchAsyncGLBPicker := func(title, kind string) {
		dialogBusyMu.Lock()
		if dialogBusy {
			dialogBusyMu.Unlock()
			netState.addChat("[UPLOAD]: File picker is already open...")
			return
		}
		dialogBusy = true
		dialogBusyMu.Unlock()

		netState.addChat("[UPLOAD]: Opening file picker (or drag & drop a .glb onto the window)...")
		go func() {
			defer func() {
				dialogBusyMu.Lock()
				dialogBusy = false
				dialogBusyMu.Unlock()
			}()
			p := pickLocalGLBFileDialog(title)
			pickedGLBCh <- dialogPickResult{path: p, kind: kind}
		}()
	}

	installValidatedGLB := func(fileName, h string, b []byte, kind string, dropLic *identity.AssetLicense) {
		safeName := sanitizeFileName(fileName)
		if kind == "avatar" {
			destP := filepath.Join(avatarsDir, safeName)
			_ = os.WriteFile(destP, b, 0644)
			cachedP := filepath.Join(cacheDir, h+".glb")
			_ = os.WriteFile(cachedP, b, 0644)
			if dropLic != nil {
				_ = identity.SaveAssetLicense(licensePathForGLB(destP), dropLic)
			}
			if _, exists := loadedModels[h]; !exists {
				m, a := loadModelWithAnims(cachedP, fogShader, cfg.EnableFog)
				loadedModels[h] = m
				if a != nil {
					loadedAnims[h] = a
				}
			}
			netState.mu.Lock()
			netState.localAvHash = h
			netState.localAvBytes = b
			netState.localAssets[h] = b
			netState.mu.Unlock()
			localAvHash = h
			localManualClip = -1
			cfg.AvatarHash = h
			saveClientSettings(settingsPath, cfg)
			avatarInv = scanInventoryFolder(avatarsDir, "avatar", cacheDir, kp.PubHex(), maxAvMB*1024*1024, localAssets)
			populateInventoryBounds(avatarInv, loadedModels)
			netState.addChat("[AVATAR]: Equipped & saved " + safeName + "!")
			return
		}
		destP := filepath.Join(itemsDir, safeName)
		_ = os.WriteFile(destP, b, 0644)
		cachedP := filepath.Join(cacheDir, h+".glb")
		_ = os.WriteFile(cachedP, b, 0644)
		if dropLic != nil {
			_ = identity.SaveAssetLicense(licensePathForGLB(destP), dropLic)
		}
		netState.mu.Lock()
		netState.localAssets[h] = b
		netState.mu.Unlock()
		if _, exists := loadedModels[h]; !exists {
			m, a := loadModelWithAnims(cachedP, fogShader, cfg.EnableFog)
			loadedModels[h] = m
			if a != nil {
				loadedAnims[h] = a
			}
		}
		propInv = scanInventoryFolder(itemsDir, "prop", cacheDir, kp.PubHex(), maxItemMB*1024*1024, localAssets)
		populateInventoryBounds(propInv, loadedModels)
		for i, it := range propInv {
			if it.Hash == h {
				propIdx = i
				break
			}
		}
		ghostActive = true
		netState.addChat("[INVENTORY]: Added " + safeName + " & armed Ghost [P]!")
	}

	importLocalGLB := func(fpath string, autoKind string) {
		fpath = strings.TrimSpace(strings.Trim(fpath, `"'`))
		if fpath == "" {
			return
		}
		if !strings.HasSuffix(strings.ToLower(fpath), ".glb") {
			netState.addChat("[UPLOAD ERR]: Only .glb files are supported")
			return
		}
		b, err := os.ReadFile(fpath)
		if err != nil {
			netState.addChat("[UPLOAD ERR]: Could not read " + filepath.Base(fpath))
			return
		}
		limitBytes := maxAvMB * 1024 * 1024
		if autoKind == "prop" {
			limitBytes = maxItemMB * 1024 * 1024
		}
		if err := glbguard.ValidateGLB(b, limitBytes); err != nil {
			netState.addChat("[GLBGUARD]: " + err.Error())
			return
		}
		h := identity.FileSHA256(b)
		var dropLic *identity.AssetLicense
		if lic, err := identity.LoadAssetLicense(licensePathForGLB(fpath)); err == nil {
			if err := lic.Verify(h, kp.PubHex()); err != nil {
				netState.addChat("[LICENSE ERR]: " + err.Error())
				return
			}
			dropLic = lic
			_ = os.Remove(worldOnlyMarkerPath(cacheDir, h))
			_ = identity.SaveAssetLicense(filepath.Join(cacheDir, h+".license.json"), lic)
		} else if lic, err := identity.LoadAssetLicense(filepath.Join(cacheDir, h+".license.json")); err == nil && lic.Verify(h, kp.PubHex()) == nil {
			dropLic = lic
			_ = os.Remove(worldOnlyMarkerPath(cacheDir, h))
		}
		if dropLic == nil && isWorldOnlyAsset(cacheDir, h) {
			netState.addChat("[DRM]: Cannot import display-only world/peer asset without a creator license!")
			return
		}
		safeName := sanitizeFileName(filepath.Base(fpath))
		if autoKind == "avatar" || autoKind == "prop" {
			installValidatedGLB(safeName, h, b, autoKind, dropLic)
			return
		}
		pendingDrop = &PendingDrop{
			FileName: safeName,
			Hash:     h,
			Bytes:    b,
		}
	}

	rl.SetExitKey(0)
	cursorLocked := true
	rl.DisableCursor()

	for !rl.WindowShouldClose() {
		sw := int32(rl.GetScreenWidth())
		sh := int32(rl.GetScreenHeight())

		md := rl.GetMouseDelta()
		if rl.GetKeyPressed() != 0 || rl.GetCharPressed() != 0 ||
			rl.IsMouseButtonPressed(rl.MouseButtonLeft) || rl.IsMouseButtonPressed(rl.MouseButtonRight) ||
			rl.GetMouseWheelMove() != 0 || md.X != 0 || md.Y != 0 {
			lastActivity = time.Now()
			if isAFK {
				isAFK = false
				netState.addChat("[SYS]: Welcome back (AFK cleared)")
			}
		} else if !isAFK && time.Since(lastActivity) >= 3*time.Minute {
			isAFK = true
			netState.addChat("[SYS]: You are now marked [AFK] after 3m of inactivity")
		}

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

		anyOverlay := chatActive || invOpen || dirOpen || settingsOpen || peersOpen || helpOpen || welcomeOpen || hasIncomingOffer || pendingDrop != nil

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

		// Advance embedded glTF animation clocks (~30 fps) without redundant skinning passes
		nowSec := rl.GetTime()
		advanceAnimClock := func(animSet *ModelAnimSet) bool {
			if animSet == nil || len(animSet.Anims) == 0 {
				return false
			}
			if nowSec-animSet.LastUpdate >= 1.0/30.0 {
				animSet.LastUpdate = nowSec
				animSet.Frame = (animSet.Frame + 1) % 1000000
				return true
			}
			return false
		}
		if advanceAnimClock(worldAnims) {
			poseModelAnimClip(worldModel, worldAnims, worldAnims.ClipIdx, 0)
		}
		for _, animSet := range loadedAnims {
			advanceAnimClock(animSet)
		}
		if animSet, ok := loadedAnims[localAvHash]; ok && len(animSet.Anims) > 0 {
			if localManualClip >= 0 && localManualClip < len(animSet.Anims) {
				localActiveClip = localManualClip
			} else {
				moving := rl.IsKeyDown(rl.KeyW) || rl.IsKeyDown(rl.KeyA) || rl.IsKeyDown(rl.KeyS) || rl.IsKeyDown(rl.KeyD)
				if !moving || len(animSet.Anims) == 1 {
					localActiveClip = 0
				} else if rl.IsKeyDown(rl.KeyLeftShift) && len(animSet.Anims) >= 3 {
					localActiveClip = 2
				} else {
					localActiveClip = 1
				}
			}
			animSet.ClipIdx = localActiveClip
		} else {
			localActiveClip = 0
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

		// Non-blocking check for async OS file picker result
		select {
		case res := <-pickedGLBCh:
			if res.path != "" {
				importLocalGLB(res.path, res.kind)
			} else {
				targetFolder := avatarsDir
				if res.kind == "prop" {
					targetFolder = itemsDir
				}
				netState.addChat("[UPLOAD]: No file chosen (or dialog unavailable). Drag & drop a .glb or use [O] to open " + targetFolder)
			}
		default:
		}

		// File drop handler (.glb) — if [I] Inventory is open on Avatars or Props tab, imports directly into that tab!
		if rl.IsFileDropped() {
			dropped := rl.LoadDroppedFiles()
			for _, fpath := range dropped {
				autoKind := ""
				if invOpen {
					autoKind = invTab
				}
				importLocalGLB(fpath, autoKind)
				break
			}
			rl.UnloadDroppedFiles()
		}

		if pendingDrop != nil {
			if rl.IsKeyPressed(rl.KeyOne) {
				lic, _ := identity.LoadAssetLicense(filepath.Join(cacheDir, pendingDrop.Hash+".license.json"))
				installValidatedGLB(pendingDrop.FileName, pendingDrop.Hash, pendingDrop.Bytes, "avatar", lic)
				pendingDrop = nil
			} else if rl.IsKeyPressed(rl.KeyTwo) {
				lic, _ := identity.LoadAssetLicense(filepath.Join(cacheDir, pendingDrop.Hash+".license.json"))
				installValidatedGLB(pendingDrop.FileName, pendingDrop.Hash, pendingDrop.Bytes, "prop", lic)
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
						hopToWorld(act.WorldURL, true)
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
					for _, it := range propInv {
						if _, ok := loadedModels[it.Hash]; !ok {
							m, a := loadModelWithAnims(filepath.Join(cacheDir, it.Hash+".glb"), fogShader, cfg.EnableFog)
							loadedModels[it.Hash] = m
							if a != nil {
								loadedAnims[it.Hash] = a
							}
						}
					}
					for _, it := range avatarInv {
						if _, ok := loadedModels[it.Hash]; !ok {
							m, a := loadModelWithAnims(filepath.Join(cacheDir, it.Hash+".glb"), fogShader, cfg.EnableFog)
							loadedModels[it.Hash] = m
							if a != nil {
								loadedAnims[it.Hash] = a
							}
						}
					}
					populateInventoryBounds(propInv, loadedModels)
					populateInventoryBounds(avatarInv, loadedModels)
					invCursor = 0
				}
			}
			if rl.IsKeyPressed(rl.KeyTab) {
				dirOpen = !dirOpen
				dirConnectActive = false
				dirConnectInput = ""
				invOpen, transferModalOpen, helpOpen, settingsOpen, peersOpen, welcomeOpen = false, false, false, false, false, false
				if dirOpen {
					dirCursor = 0
					refreshPortalStatuses(directory)
				}
			}
		}

		forward := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))
		ghostPos := rl.Vector3Add(playerHeadPos, rl.Vector3Scale(forward, 3.2))
		ghostPos.Y = (playerHeadPos.Y - 1.65) + ghostYOffset
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
					animTag := ""
					if animSet, hasAnim := loadedAnims[prop.Hash]; hasAnim && len(animSet.Anims) > 0 {
						if prop.AnimClip < 0 {
							animTag = fmt.Sprintf("  |  Anim: Paused/%d", len(animSet.Anims))
						} else {
							animTag = fmt.Sprintf("  |  Anim: %d/%d", (prop.AnimClip%len(animSet.Anims))+1, len(animSet.Anims))
						}
					}
					sub := "Owner: " + ownerLabel + animTag + "  |  [E] Interact"
					if animTag != "" {
						sub = "Owner: " + ownerLabel + animTag + "  |  [E] Cycle Anim"
					}
					if canDel {
						sub += "  |  [C] Clone  |  [Del] Remove"
					}
					lookedAt = &RayTarget{
						Kind:      "prop",
						ID:        prop.PropID,
						PropHash:  prop.Hash,
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
		} else if welcomeOpen {
			if rl.IsKeyPressed(rl.KeyEscape) || rl.IsKeyPressed(rl.KeyEnter) || rl.IsKeyPressed(rl.KeySpace) {
				welcomeOpen = false
			}
		} else if helpOpen {
			if rl.IsKeyPressed(rl.KeyEscape) {
				helpOpen = false
			}
		} else if settingsOpen {
			// 0: FOV, 1: Mouse Sens, 2: Voice Volume, 3: Ambient Radio Vol, 4: Avatar Scale, 5: Mini-Radar, 6: Nameplates, 7: Crosshair, 8: Distance Fog
			if rl.IsKeyPressed(rl.KeyDown) || rl.IsKeyPressed(rl.KeyS) {
				settingsCursor = (settingsCursor + 1) % 9
			}
			if rl.IsKeyPressed(rl.KeyUp) || rl.IsKeyPressed(rl.KeyW) {
				settingsCursor = (settingsCursor + 8) % 9
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
					cfg.AmbientVol = clampFloat32(cfg.AmbientVol+dir*0.1, 0.0, 1.0)
					startWorldAudio(activeWorld)
				case 4:
					cfg.AvatarScale = clampFloat32(cfg.AvatarScale+dir*0.1, 0.25, 4.0)
				case 5:
					cfg.ShowRadar = !cfg.ShowRadar
				case 6:
					cfg.ShowTags = !cfg.ShowTags
				case 7:
					cfg.ShowCross = !cfg.ShowCross
				case 8:
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
				} else if rl.IsKeyPressed(rl.KeyU) {
					launchAsyncGLBPicker("Select .glb "+invTab+" to upload", invTab)
				} else if rl.IsKeyPressed(rl.KeyO) {
					targetFolder := itemsDir
					if invTab == "avatar" {
						targetFolder = avatarsDir
					}
					openLocalFolderOS(targetFolder)
					netState.addChat("[INVENTORY]: Opened folder " + targetFolder)
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
				if rl.IsKeyPressed(rl.KeyD) && len(activeList) > 0 {
					chosen := activeList[invCursor%len(activeList)]
					if chosen.Path == defaultAvPath || chosen.Path == defaultPropPath {
						netState.addChat("[INVENTORY]: Cannot delete built-in default " + chosen.Name)
					} else if chosen.Path != "" {
						_ = os.Remove(chosen.Path)
						_ = os.Remove(licensePathForGLB(chosen.Path))
						if invTab == "avatar" {
							if chosen.Hash == localAvHash {
								if b, err := os.ReadFile(defaultAvPath); err == nil {
									h := identity.FileSHA256(b)
									localAvHash = h
									cfg.AvatarHash = h
									saveClientSettings(settingsPath, cfg)
									netState.mu.Lock()
									netState.localAvHash = h
									netState.localAvBytes = b
									netState.mu.Unlock()
								}
							}
							avatarInv = scanInventoryFolder(avatarsDir, "avatar", cacheDir, kp.PubHex(), maxAvMB*1024*1024, localAssets)
							populateInventoryBounds(avatarInv, loadedModels)
						} else {
							propInv = scanInventoryFolder(itemsDir, "prop", cacheDir, kp.PubHex(), maxItemMB*1024*1024, localAssets)
							populateInventoryBounds(propInv, loadedModels)
						}
						if invCursor > 0 {
							invCursor--
						}
						netState.addChat("[INVENTORY]: Deleted local item " + chosen.Name)
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
						localManualClip = -1
						cfg.AvatarHash = chosen.Hash
						saveClientSettings(settingsPath, cfg)
						netState.addChat("[AVATAR]: Equipped & saved " + chosen.Name)
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
			if dirConnectActive {
				if (rl.IsKeyDown(rl.KeyLeftControl) || rl.IsKeyDown(rl.KeyRightControl)) && rl.IsKeyPressed(rl.KeyV) {
					clip := sanitizeDisplayText(getClipboardSafe(), 200)
					if clip != "" {
						dirConnectInput += clip
						if len(dirConnectInput) > 200 {
							dirConnectInput = dirConnectInput[:200]
						}
					}
				}
				for key := rl.GetCharPressed(); key > 0; key = rl.GetCharPressed() {
					if key >= 32 && key <= 126 && len(dirConnectInput) < 200 {
						dirConnectInput += string(rune(key))
					}
				}
				if (rl.IsKeyPressed(rl.KeyBackspace) || rl.IsKeyPressedRepeat(rl.KeyBackspace)) && len(dirConnectInput) > 0 {
					runes := []rune(dirConnectInput)
					dirConnectInput = string(runes[:len(runes)-1])
				}
				if rl.IsKeyPressed(rl.KeyEnter) {
					rawEntry := strings.TrimSpace(dirConnectInput)
					dirConnectActive = false
					dirConnectInput = ""
					if rawEntry != "" {
						if !strings.Contains(rawEntry, "://") {
							rawEntry = "vektorweb://" + rawEntry
						}
						if !strings.HasSuffix(strings.ToLower(rawEntry), ".toml") && !strings.HasPrefix(rawEntry, "local://") {
							rawEntry = strings.TrimRight(rawEntry, "/") + "/world.toml"
						}
						dirStatusMu.Lock()
						exists := false
						for _, ew := range directory.Worlds {
							if normalizeWorldURL(ew.URL) == normalizeWorldURL(rawEntry) {
								exists = true
								break
							}
						}
						if !exists {
							directory.Worlds = append(directory.Worlds, struct {
								Name string `toml:"name"`
								URL  string `toml:"url"`
							}{Name: rawEntry, URL: rawEntry})
							_ = saveWorldDirectory(dirPath, directory)
						}
						dirStatusMu.Unlock()
						dirOpen = false
						hopToWorld(rawEntry, true)
					}
				} else if rl.IsKeyPressed(rl.KeyEscape) {
					dirConnectActive = false
					dirConnectInput = ""
				}
			} else {
				dirStatusMu.Lock()
				visDirCount := len(directory.Worlds)
				if visDirCount > 8 {
					visDirCount = 8
				}
				var hopTarget string
				if rl.IsKeyPressed(rl.KeyN) {
					dirConnectActive = true
					dirConnectInput = ""
				}
				if rl.IsKeyPressed(rl.KeyDown) || rl.IsKeyPressed(rl.KeyS) {
					if visDirCount > 0 {
						dirCursor = (dirCursor + 1) % visDirCount
					}
				}
				if rl.IsKeyPressed(rl.KeyUp) || rl.IsKeyPressed(rl.KeyW) {
					if visDirCount > 0 {
						dirCursor = (dirCursor - 1 + visDirCount) % visDirCount
					}
				}
				if rl.IsKeyPressed(rl.KeyEnter) && visDirCount > 0 {
					idx := dirCursor % visDirCount
					hopTarget = directory.Worlds[idx].URL
					dirOpen = false
				}
				if rl.IsKeyPressed(rl.KeyDelete) && len(directory.Worlds) > 1 && visDirCount > 0 {
					idx := dirCursor % visDirCount
					removed := directory.Worlds[idx].Name
					directory.Worlds = append(directory.Worlds[:idx], directory.Worlds[idx+1:]...)
					if dirCursor >= len(directory.Worlds) && dirCursor > 0 {
						dirCursor--
					}
					_ = saveWorldDirectory(dirPath, directory)
					netState.addChat(fmt.Sprintf("[DIRECTORY]: Removed %q from worlds.toml", removed))
				}
				if rl.IsKeyPressed(rl.KeyF) && visDirCount > 0 {
					idx := dirCursor % visDirCount
					if idx > 0 {
						pinned := directory.Worlds[idx]
						copy(directory.Worlds[1:idx+1], directory.Worlds[0:idx])
						directory.Worlds[0] = pinned
						dirCursor = 0
						if err := saveWorldDirectory(dirPath, directory); err == nil {
							netState.addChat(fmt.Sprintf("[DIRECTORY]: Pinned %q to slot [1]", pinned.Name))
						}
					} else {
						netState.addChat(fmt.Sprintf("[DIRECTORY]: %q is already pinned at slot [1]", directory.Worlds[0].Name))
					}
				}
				for i := 0; i < visDirCount; i++ {
					if rl.IsKeyPressed(int32(rl.KeyOne + i)) {
						hopTarget = directory.Worlds[i].URL
						dirOpen = false
						break
					}
				}
				dirStatusMu.Unlock()
				if hopTarget != "" {
					hopToWorld(hopTarget, true)
				}
				if rl.IsKeyPressed(rl.KeyR) {
					refreshPortalStatuses(directory)
				}
				if rl.IsKeyPressed(rl.KeyEscape) {
					dirOpen = false
				}
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
				if rl.IsKeyPressed(rl.KeyC) && walkMode && (lookedAt == nil || lookedAt.Kind != "prop" || !lookedAt.CanDelete) {
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

				// Look-at interaction: [Delete] removes your targeted prop; [C] clones your prop into Build Mode; [E] interacts with prop or offers trade to peer
				if lookedAt != nil {
					if lookedAt.Kind == "prop" {
						if lookedAt.CanDelete && rl.IsKeyPressed(rl.KeyDelete) {
							netState.mu.Lock()
							delete(netState.props, lookedAt.ID)
							netState.mu.Unlock()
							for i := len(ownedPropIDs) - 1; i >= 0; i-- {
								if ownedPropIDs[i] == lookedAt.ID {
									ownedPropIDs = append(ownedPropIDs[:i], ownedPropIDs[i+1:]...)
								}
							}
							_ = netState.sendJSON(WireMsg{Type: "PROP_REMOVE", PropID: lookedAt.ID})
							netState.addChat("[PROP]: Removed " + lookedAt.Label)
						} else if lookedAt.CanDelete && rl.IsKeyPressed(rl.KeyC) {
							netState.mu.RLock()
							srcProp, ok := netState.props[lookedAt.ID]
							netState.mu.RUnlock()
							if ok {
								for idx, it := range propInv {
									if strings.EqualFold(it.Hash, srcProp.Hash) {
										propIdx = idx
										break
									}
								}
								ghostScale = srcProp.Scale
								if ghostScale <= 0 {
									ghostScale = 1.0
								}
								ghostPitchOffset = srcProp.Pitch
								ghostRollOffset = srcProp.Roll
								ghostAnimClip = srcProp.AnimClip
								ghostActive = true
								netState.addChat("[BUILD]: Cloned " + lookedAt.Label + " into Build Mode ([P] to place)")
							}
						} else if rl.IsKeyPressed(rl.KeyE) {
							netState.mu.Lock()
							pr, ok := netState.props[lookedAt.ID]
							newClip := 0
							cycledAnim := false
							clipTotal := 0
							if ok {
								if animSet, hasAnim := loadedAnims[pr.Hash]; hasAnim && len(animSet.Anims) > 0 {
									clipTotal = len(animSet.Anims)
									if pr.AnimClip < 0 {
										pr.AnimClip = 0
									} else if pr.AnimClip+1 < clipTotal {
										pr.AnimClip++
									} else {
										pr.AnimClip = -1
									}
									newClip = pr.AnimClip
									cycledAnim = true
								}
							}
							netState.mu.Unlock()
							if ok {
								if cycledAnim {
									_ = netState.sendJSON(WireMsg{
										Type:     "PROP_ANIM",
										PropID:   pr.PropID,
										AnimClip: newClip,
									})
									if newClip < 0 {
										netState.addChat(fmt.Sprintf("[ANIM]: %s animation paused", lookedAt.Label))
									} else {
										netState.addChat(fmt.Sprintf("[ANIM]: %s clip %d/%d", lookedAt.Label, newClip+1, clipTotal))
									}
								}
								if activeWorld.Sandbox != nil {
									if hooks, hasHooks := any(activeWorld.Sandbox).(interface {
										NotifyPropInteract(propID, hash, owner string)
									}); hasHooks {
										hooks.NotifyPropInteract(pr.PropID, pr.Hash, pr.Owner)
									}
								}
								if !cycledAnim {
									netState.addChat(fmt.Sprintf("[INTERACT]: Activated %s", lookedAt.Label))
								}
							}
						}
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
					ghostAnimClip = 0
				}
				if rl.IsKeyPressed(rl.KeyZero) {
					localManualClip = -1
					netState.addChat("[ANIM]: Avatar auto-locomotion (Idle/Walk/Run) ON")
				}
				for clipKey := 0; clipKey < 9; clipKey++ {
					if rl.IsKeyPressed(int32(rl.KeyOne + clipKey)) {
						if animSet, ok := loadedAnims[localAvHash]; ok && len(animSet.Anims) > 0 {
							if clipKey < len(animSet.Anims) {
								localManualClip = clipKey
								animSet.ClipIdx = clipKey
								animSet.Frame = 0
								netState.addChat(fmt.Sprintf("[ANIM]: Pinned avatar clip %d/%d ([0] for Auto)", clipKey+1, len(animSet.Anims)))
							} else {
								netState.addChat(fmt.Sprintf("[ANIM]: Avatar only has %d animation clip(s)", len(animSet.Anims)))
							}
						}
					}
				}
				if rl.IsKeyPressed(rl.KeyH) {
					cfg.ShowWaypoint = !cfg.ShowWaypoint
					saveClientSettings(settingsPath, cfg)
					if cfg.ShowWaypoint {
						netState.addChat("[HUD]: Spawn waypoint marker ON")
					} else {
						netState.addChat("[HUD]: Spawn waypoint marker OFF")
					}
				}
				if rl.IsKeyPressed(rl.KeyU) {
					ambientRadio.mu.Lock()
					ambientRadio.muted = !ambientRadio.muted
					isAmbMuted := ambientRadio.muted
					ambientRadio.mu.Unlock()
					if isAmbMuted {
						ambientRadio.Stop()
						netState.addChat("[RADIO]: Ambient world audio muted")
					} else {
						startWorldAudio(activeWorld)
						netState.addChat("[RADIO]: Ambient world audio unmuted")
					}
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
						ghostPitchOffset = 0
						ghostRollOffset = 0
						ghostYOffset = 0
						ghostScale = 1.0
						ghostAnimClip = 0
						netState.addChat("[BUILD]: Reset prop rotation, tilt, height, scale, and anim")
					}
					if rl.IsKeyPressed(rl.KeyA) && activeProp.Hash != "" {
						if animSet, ok := loadedAnims[activeProp.Hash]; ok && len(animSet.Anims) > 0 {
							if ghostAnimClip < 0 {
								ghostAnimClip = 0
							} else if ghostAnimClip+1 < len(animSet.Anims) {
								ghostAnimClip++
							} else {
								ghostAnimClip = -1
							}
							if ghostAnimClip < 0 {
								netState.addChat("[BUILD]: Prop animation set to PAUSED")
							} else {
								netState.addChat(fmt.Sprintf("[BUILD]: Prop animation clip %d/%d", ghostAnimClip+1, len(animSet.Anims)))
							}
						}
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
					if rl.IsKeyDown(rl.KeyZ) {
						ghostPitchOffset += 90.0 * float32(dt)
					}
					if rl.IsKeyDown(rl.KeyX) {
						ghostRollOffset += 90.0 * float32(dt)
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
						PropID:   propID,
						Owner:    kp.PubHex(),
						Hash:     activeProp.Hash,
						Pos:      ghostPos,
						Yaw:      ghostYaw,
						Pitch:    ghostPitchOffset,
						Roll:     ghostRollOffset,
						Scale:    ghostScale,
						AnimClip: ghostAnimClip,
					}
					netState.mu.Lock()
					netState.props[propID] = wp
					netState.mu.Unlock()
					ownedPropIDs = append(ownedPropIDs, propID)

					_ = netState.sendJSON(WireMsg{
						Type:     "PROP_SPAWN",
						PropID:   propID,
						Hash:     activeProp.Hash,
						Pos:      []float32{ghostPos.X, ghostPos.Y, ghostPos.Z},
						Yaw:      ghostYaw,
						Pitch:    ghostPitchOffset,
						Roll:     ghostRollOffset,
						Scale:    ghostScale,
						AnimClip: ghostAnimClip,
					})
					netState.addChat(fmt.Sprintf("[PROP]: Placed %s (%s...)", activeProp.Name, propID[:6]))
				}

				if !ghostActive && rl.IsKeyPressed(rl.KeyX) && len(ownedPropIDs) > 0 {
					netState.mu.Lock()
					var lastID string
					for len(ownedPropIDs) > 0 {
						cand := ownedPropIDs[len(ownedPropIDs)-1]
						ownedPropIDs = ownedPropIDs[:len(ownedPropIDs)-1]
						if _, exists := netState.props[cand]; exists {
							lastID = cand
							delete(netState.props, cand)
							break
						}
					}
					netState.mu.Unlock()
					if lastID != "" {
						_ = netState.sendJSON(WireMsg{Type: "PROP_REMOVE", PropID: lastID})
						netState.addChat("[PROP]: Undid prop " + lastID[:minInt(6, len(lastID))])
					}
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
				clip := sanitizeDisplayText(getClipboardSafe(), 240)
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
					if strings.HasPrefix(strings.TrimSpace(chatInput), "/uploadavatar") {
						argPath := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(chatInput), "/uploadavatar"))
						if argPath != "" {
							importLocalGLB(argPath, "avatar")
						} else {
							launchAsyncGLBPicker("Select Avatar .glb to Upload", "avatar")
						}
					} else if strings.TrimSpace(chatInput) == "/openavatars" {
						openLocalFolderOS(avatarsDir)
						netState.addChat("[AVATAR]: Opened " + avatarsDir)
					} else {
						handleChatCommand(chatInput, kp, netState, hopToWorld, &visitedWorlds, &showRangeRing, &avatarInv, &propInv, &localAvHash, exportsDir, &spawn, playerHeadPos, activeWorld.URL, activeWorld.Manifest.World.Title, worldModel, &cfg, settingsPath, &directory, dirPath)
					}
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
					hopToWorld(portal.URL, true)
					break
				}
			}
		}

		if time.Since(lastSync) >= 33*time.Millisecond {
			lastSync = time.Now()
			netState.mu.RLock()
			curNick := netState.nick
			netState.mu.RUnlock()
			txNick := curNick
			if isAFK {
				txNick = "[AFK] " + curNick
			}

			_ = netState.sendJSON(WireMsg{
				Type:       "STATE",
				Nick:       txNick,
				Pos:        []float32{bodyHeadPos.X, bodyHeadPos.Y, bodyHeadPos.Z},
				Yaw:        bodyYaw,
				AvatarHash: localAvHash,
				AnimClip:   localActiveClip,
				Scale:      cfg.AvatarScale,
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
					label:     fmt.Sprintf("PORTAL: %s (%.1fm)", sanitizeDisplayText(labelName, 28), dist),
					isPortal:  true,
				})
			}
		}

		if (rl.IsKeyDown(rl.KeyV) && !chatActive) || showRangeRing {
			ringPos := rl.NewVector3(bodyHeadPos.X, bodyHeadPos.Y-1.62, bodyHeadPos.Z)
			ringCol := rl.Fade(rl.SkyBlue, 0.45)
			if rl.IsKeyDown(rl.KeyV) && !chatActive {
				ringCol = rl.Fade(rl.Lime, 0.65)
			}
			rl.DrawCircle3D(ringPos, 35.0, rl.NewVector3(1, 0, 0), 90, ringCol)
		}

		if thirdPerson && walkMode {
			feetPos := rl.NewVector3(bodyHeadPos.X, bodyHeadPos.Y-1.65, bodyHeadPos.Z)
			userAvSc := cfg.AvatarScale
			if userAvSc <= 0 {
				userAvSc = 1.0
			}
			rl.DrawCircle3D(rl.NewVector3(feetPos.X, feetPos.Y+0.02, feetPos.Z), 0.42*userAvSc, rl.NewVector3(1, 0, 0), 90, rl.Fade(rl.SkyBlue, 0.7))
			if mod, ok := loadedModels[localAvHash]; ok {
				met := getAvatarMetrics(modelMetrics, localAvHash, mod)
				poseModelAnimClip(mod, loadedAnims[localAvHash], localActiveClip, 0)
				avSc := met.BaseScale * userAvSc
				drawPos := rl.NewVector3(feetPos.X, feetPos.Y+met.FeetOffsetY*avSc, feetPos.Z)
				rl.DrawModelEx(mod, drawPos, rl.NewVector3(0, 1, 0), bodyYaw, rl.NewVector3(avSc, avSc, avSc), rl.White)
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
					poseModelAnimClip(mod, loadedAnims[activeProp.Hash], ghostAnimClip, 0)
					drawModelEuler(mod, ghostPos, ghostYaw, ghostPitchOffset, ghostRollOffset, ghostScale, rl.Fade(rl.Lime, 0.55))
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
				poseModelAnimClip(mod, loadedAnims[prop.Hash], prop.AnimClip, int32(len(prop.PropID)*7))
				drawModelEuler(mod, prop.Pos, prop.Yaw, prop.Pitch, prop.Roll, s, rl.White)
			} else {
				rl.DrawCubeWires(prop.Pos, 0.8, 0.8, 0.8, rl.Gold)
			}
			if activeWorld.Sandbox != nil {
				if hooks, hasHooks := any(activeWorld.Sandbox).(interface {
					NotifyPropProximity(propID, hash string, entered bool)
				}); hasHooks {
					dProp := rl.Vector3Distance(bodyHeadPos, prop.Pos)
					inRange := dProp <= 2.4
					if inRange && !nearProps[prop.PropID] {
						nearProps[prop.PropID] = true
						hooks.NotifyPropProximity(prop.PropID, prop.Hash, true)
					} else if !inRange && nearProps[prop.PropID] {
						delete(nearProps, prop.PropID)
						hooks.NotifyPropProximity(prop.PropID, prop.Hash, false)
					}
				}
			}
		}

		for _, peer := range netState.peers {
			if time.Since(peer.LastSeen) > 10*time.Second || netState.blockedPeers[peer.PubKey] {
				continue
			}
			peerFeetPos := rl.NewVector3(peer.Pos.X, peer.Pos.Y-1.65, peer.Pos.Z)
			if mod, ok := loadedModels[peer.AvatarHash]; ok {
				met := getAvatarMetrics(modelMetrics, peer.AvatarHash, mod)
				poseModelAnimClip(mod, loadedAnims[peer.AvatarHash], peer.AnimClip, int32(len(peer.Nick)*11))
				uSc := peer.AvatarScale
				if uSc < 0.25 || uSc > 4.0 {
					uSc = 1.0
				}
				peerSc := met.BaseScale * uSc
				drawPos := rl.NewVector3(peerFeetPos.X, peerFeetPos.Y+met.FeetOffsetY*peerSc, peerFeetPos.Z)
				rl.DrawModelEx(mod, drawPos, rl.NewVector3(0, 1, 0), peer.Yaw, rl.NewVector3(peerSc, peerSc, peerSc), rl.White)
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

		if cfg.ShowWaypoint {
			toSpawn := rl.Vector3Subtract(spawn, cam.Position)
			spawnDist := rl.Vector3Length(toSpawn)
			if spawnDist > 6.0 && rl.Vector3DotProduct(lookVec, toSpawn) > 0 {
				sp := rl.GetWorldToScreen(rl.NewVector3(spawn.X, spawn.Y+0.8, spawn.Z), cam)
				screenLabels = append(screenLabels, screenLabel{
					screenPos: sp,
					label:     fmt.Sprintf("SPAWN (%.0fm)", spawnDist),
					isPortal:  true,
				})
			}
		}
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
			tw := uiTextWidth(txt, 16)
			bx := int32(sl.screenPos.X) - tw/2 - 8
			by := int32(sl.screenPos.Y) - 12
			pill := rl.NewRectangle(float32(bx), float32(by), float32(tw+16), 24)
			rl.DrawRectangleRounded(pill, 0.35, 6, rl.NewColor(10, 14, 24, 215))
			rl.DrawRectangleRoundedLinesEx(pill, 0.35, 6, 1.1, borderCol)
			drawUITextShadow(txt, bx+8, by+4, 16, txtCol)
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
				w1 := uiTextWidth(lookedAt.Label, 16)
				w2 := uiTextWidth(lookedAt.SubLabel, 14)
				boxW := w1
				if w2 > boxW {
					boxW = w2
				}
				boxW += 20
				bx := cx - boxW/2
				by := cy + 16
				drawGlassPanel(bx, by, boxW, 46, rl.NewColor(56, 189, 248, 255), 225)
				drawUITextShadow(lookedAt.Label, bx+10, by+6, 16, rl.NewColor(125, 211, 252, 255))
				drawUIText(lookedAt.SubLabel, bx+10, by+25, 14, rl.NewColor(215, 224, 238, 255))
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
			buildLine = fmt.Sprintf("[BUILD: %s] Snap[B]:%s | Y/P/R:%.0f/%.0f/%.0f | H:%.2f | %.2fx | [G/Esc] Exit", activeProp.Name, snapTxt, ghostYawOffset, ghostPitchOffset, ghostRollOffset, ghostYOffset, ghostScale)
		}

		// Auto-size HUD width to fit content cleanly while reserving space for top-right status/radar
		desiredW := uiTextWidth(headerLine, 17) + 42
		if w2 := uiTextWidth(statsLine, 16) + 24; w2 > desiredW {
			desiredW = w2
		}
		if luaHUD != "" {
			if w3 := uiTextWidth(luaHUD, 16) + 24; w3 > desiredW {
				desiredW = w3
			}
		}
		if buildLine != "" {
			if w4 := uiTextWidth(buildLine, 15) + 24; w4 > desiredW {
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
		drawGlassPanel(12, 12, hudW, hudH, rl.NewColor(56, 189, 248, 255), 222)

		rl.BeginScissorMode(14, 14, hudW-4, hudH-4)
		rl.DrawCircle(26, 27, 5, connDot)
		drawUITextShadow(truncateToWidth(headerLine, 17, hudW-38), 38, 18, 17, rl.RayWhite)
		drawUIText(truncateToWidth(statsLine, 16, hudW-22), 22, 44, 16, rl.LightGray)

		nextHudY := int32(68)
		if luaHUD != "" {
			drawUIText(truncateToWidth(luaHUD, 16, hudW-22), 22, nextHudY, 16, rl.Gold)
			nextHudY += 24
		}
		if buildLine != "" {
			drawUIText(truncateToWidth(buildLine, 15, hudW-22), 22, nextHudY, 15, rl.Lime)
		}
		rl.EndScissorMode()

		if rl.IsKeyDown(rl.KeyV) && !chatActive {
			lbl := "MIC LIVE  •  35m SPATIAL TX"
			bw := uiTextWidth(lbl, 15) + 22
			drawKeyBadge(lbl, sw-bw-14, 12, 15, rl.Lime, rl.NewColor(18, 44, 26, 225), rl.Lime)
		} else if isMuted {
			lbl := "VOICE MUTED  [M]"
			bw := uiTextWidth(lbl, 15) + 22
			drawKeyBadge(lbl, sw-bw-14, 12, 15, rl.NewColor(255, 115, 115, 255), rl.NewColor(52, 18, 22, 225), rl.Red)
		} else if !cursorLocked && !anyOverlay {
			lbl := "CLICK WINDOW TO LOOK"
			bw := uiTextWidth(lbl, 15) + 22
			drawKeyBadge(lbl, sw-bw-14, 12, 15, rl.Gold, rl.NewColor(36, 30, 16, 225), rl.Gold)
		}

		if cfg.ShowRadar {
			drawMiniRadar(sw-84, 112, 64, cam, lookVec, netState, activeWorld.Manifest.Portals)
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
				dlRec := rl.NewRectangle(float32(bx), float32(dlY), float32(bw), 34)
				rl.DrawRectangleRounded(dlRec, 0.22, 6, rl.NewColor(13, 17, 27, 225))
				rl.DrawRectangleRoundedLinesEx(dlRec, 0.22, 6, 1.1, rl.NewColor(56, 189, 248, 220))
				trackRec := rl.NewRectangle(float32(bx+6), float32(dlY+22), float32(bw-12), 6)
				rl.DrawRectangleRounded(trackRec, 0.5, 4, rl.NewColor(28, 38, 56, 255))
				fillRec := rl.NewRectangle(float32(bx+6), float32(dlY+22), float32(bw-12)*pct, 6)
				rl.DrawRectangleRounded(fillRec, 0.5, 4, rl.NewColor(56, 189, 248, 255))
				drawUIText(fmt.Sprintf("DL %s.. %d/%d", dl.Hash[:6], dl.Received, dl.Total), bx+8, dlY+4, 13, rl.RayWhite)
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

			bgAlpha := uint8(196)
			borderCol := rl.NewColor(62, 82, 118, 200)
			if chatActive {
				bgAlpha = 232
				borderCol = rl.NewColor(56, 189, 248, 235)
			}
			chatRec := rl.NewRectangle(float32(chatX), float32(panelY), float32(chatBoxW), float32(panelH))
			rl.DrawRectangleRounded(chatRec, 0.04, 8, rl.NewColor(13, 17, 27, bgAlpha))
			rl.DrawRectangleRoundedLinesEx(chatRec, 0.04, 8, 1.2, borderCol)

			if chatActive {
				hdrRec := rl.NewRectangle(float32(chatX+2), float32(panelY+2), float32(chatBoxW-4), float32(headerH-1))
				rl.DrawRectangleRounded(hdrRec, 0.18, 6, rl.NewColor(20, 28, 44, 240))
				drawUIText("COMMS  •  /help /me /outfit /range /back /copykey  •  [Up/Dn] History", chatX+chatPadX, panelY+4, 13, rl.NewColor(125, 211, 252, 255))
			}

			rl.BeginScissorMode(chatX+4, panelY+headerH+2, chatBoxW-8, bodyH-4)
			if len(wrappedLines) == 0 {
				drawUIText("No messages yet. Type below and press [Enter] to send...", chatX+chatPadX, panelY+headerH+chatPadY, 15, rl.Gray)
			} else {
				cy := panelY + headerH + chatPadY
				for _, wl := range wrappedLines {
					drawUIText(wl.text, chatX+chatPadX, cy, chatFontSize, wl.color)
					cy += chatLineH
				}
			}
			rl.EndScissorMode()
		}

		if chatActive {
			inputY := sh - 41
			inputH := int32(30)
			inRec := rl.NewRectangle(float32(chatX), float32(inputY), float32(chatBoxW), float32(inputH))
			rl.DrawRectangleRounded(inRec, 0.22, 8, rl.NewColor(17, 24, 38, 246))
			rl.DrawRectangleRoundedLinesEx(inRec, 0.22, 8, 1.3, rl.NewColor(56, 189, 248, 245))
			cursorBlink := "|"
			if int(rl.GetTime()*2.2)%2 == 1 {
				cursorBlink = " "
			}
			renderedInput := fitInputTail("> ", chatInput, cursorBlink, 17, chatBoxW-20)
			rl.BeginScissorMode(chatX+6, inputY+2, chatBoxW-12, inputH-4)
			drawUIText(renderedInput, chatX+10, inputY+6, 17, rl.NewColor(165, 243, 252, 255))
			rl.EndScissorMode()
		} else {
			hintTxt := "[I] Inventory  •  [C] 3rd-Person  •  [F] Walk/Fly  •  [R] Spawn  •  [T] Chat  •  [V] Voice  •  [Tab] Worlds  •  [F1] Help  •  [F2] Settings"
			if uiTextWidth(hintTxt, 14) > sw-36 {
				hintTxt = "[I] Inv  •  [C] 3P  •  [F] Walk/Fly  •  [T] Chat  •  [V] Voice  •  [Tab] Worlds  •  [F1] Help  •  [F2] Settings"
			}
			dockW := uiTextWidth(hintTxt, 14) + 22
			dockRec := rl.NewRectangle(12, float32(sh-34), float32(dockW), 24)
			rl.DrawRectangleRounded(dockRec, 0.32, 6, rl.NewColor(13, 17, 27, 195))
			rl.DrawRectangleRoundedLinesEx(dockRec, 0.32, 6, 1.0, rl.NewColor(54, 72, 104, 180))
			drawUIText(hintTxt, 23, sh-29, 14, rl.NewColor(180, 194, 216, 235))
		}

		if invOpen {
			drawModalBackdrop(sw, sh)
			mw, mh := int32(920), int32(465)
			mx, my := (sw-mw)/2, (sh-mh)/2
			drawGlassPanel(mx, my, mw, mh, rl.SkyBlue, 246)
			drawUITextShadow("LOCAL INVENTORY & KEY-SIGNED TRANSFERS (~/.vektorweb-client/)", mx+24, my+18, 18, rl.SkyBlue)

			propCol := rl.Gray
			avCol := rl.Gray
			activeList := propInv
			if invTab == "prop" {
				propCol = rl.Lime
			} else {
				avCol = rl.Lime
				activeList = avatarInv
			}
			drawUIText(fmt.Sprintf("[1] Props (%d)", len(propInv)), mx+24, my+50, 18, propCol)
			drawUIText(fmt.Sprintf("[2] Avatars (%d)", len(avatarInv)), mx+195, my+50, 18, avCol)

			// Render rotating 3D preview of currently highlighted item into invPreviewRT
			if len(activeList) > 0 && invPreviewRT.ID > 0 {
				selItem := activeList[invCursor%len(activeList)]
				px, py, pw, ph := mx+mw-286, my+68, int32(260), int32(260)
				rl.BeginTextureMode(invPreviewRT)
				rl.ClearBackground(rl.NewColor(16, 22, 34, 255))
				if mod, ok := loadedModels[selItem.Hash]; ok && mod.MeshCount > 0 && modelTotalVerts(mod) <= 65000 {
					bb := rl.GetModelBoundingBox(mod)
					cx := (bb.Min.X + bb.Max.X) * 0.5
					cy := (bb.Min.Y + bb.Max.Y) * 0.5
					cz := (bb.Min.Z + bb.Max.Z) * 0.5
					extX := bb.Max.X - bb.Min.X
					extY := bb.Max.Y - bb.Min.Y
					extZ := bb.Max.Z - bb.Min.Z
					maxDim := extX
					if extY > maxDim {
						maxDim = extY
					}
					if extZ > maxDim {
						maxDim = extZ
					}
					if maxDim < 0.25 || math.IsNaN(float64(maxDim)) || math.IsInf(float64(maxDim), 0) {
						maxDim = 1.5
					}
					camDist := maxDim * 1.85
					prevCam := rl.Camera3D{
						Position:   rl.NewVector3(0, maxDim*0.35, camDist),
						Target:     rl.NewVector3(0, 0, 0),
						Up:         rl.NewVector3(0, 1, 0),
						Fovy:       45.0,
						Projection: rl.CameraPerspective,
					}
					if animSet, hasAnim := loadedAnims[selItem.Hash]; hasAnim && len(animSet.Anims) > 0 {
						poseModelAnimClip(mod, animSet, 0, 0)
					}
					if cfg.EnableFog {
						setModelFog(mod, false)
					}
					spinYaw := float32(math.Mod(rl.GetTime()*42.0, 360.0))
					rl.BeginMode3D(prevCam)
					rl.DrawGrid(6, maxDim*0.35)
					rl.DrawModelEx(mod, rl.NewVector3(-cx, -cy, -cz), rl.NewVector3(0, 1, 0), spinYaw, rl.NewVector3(1, 1, 1), rl.White)
					rl.EndMode3D()
					if cfg.EnableFog {
						setModelFog(mod, true)
					}
				}
				rl.EndTextureMode()

				srcRec := rl.NewRectangle(0, 0, float32(invPreviewRT.Texture.Width), -float32(invPreviewRT.Texture.Height))
				dstRec := rl.NewRectangle(float32(px), float32(py), float32(pw), float32(ph))
				rl.DrawTexturePro(invPreviewRT.Texture, srcRec, dstRec, rl.Vector2Zero(), 0, rl.White)
				rl.DrawRectangleLines(px, py, pw, ph, rl.NewColor(56, 189, 248, 220))
				drawUIText(truncateToWidth(selItem.Name, 14, pw), px, py+ph+8, 14, rl.SkyBlue)
				if selItem.HasBounds {
					drawUIText(fmt.Sprintf("Size: %.2f x %.2f x %.2fm", selItem.Bounds[0], selItem.Bounds[1], selItem.Bounds[2]), px, py+ph+26, 13, rl.LightGray)
				}
				shaLine := "SHA: " + selItem.Hash[:12] + "..."
				if animSet, hasAnim := loadedAnims[selItem.Hash]; hasAnim && len(animSet.Anims) > 0 {
					shaLine += fmt.Sprintf(" | Clips: %d", len(animSet.Anims))
				}
				drawUIText(shaLine, px, py+ph+44, 13, rl.Gray)
			}

			if transferModalOpen && len(activeList) > 0 {
				chosen := activeList[invCursor%len(activeList)]
				leftColW := mw - 330
				drawUIText(truncateToWidth("TRANSFER ITEM: "+chosen.Name, 18, leftColW), mx+24, my+88, 18, rl.Gold)
				lockLabel := "VENDOR-LOCKED (No resale)"
				lockCol := rl.SkyBlue
				if transferAllowResale {
					lockLabel = "TRANSFERABLE (Resale allowed)"
					lockCol = rl.Lime
				}
				drawUIText(truncateToWidth("[L] Mode: "+lockLabel, 16, leftColW), mx+24, my+116, 16, lockCol)

				netState.mu.RLock()
				var onlinePeers []*RemotePeer
				for _, p := range netState.peers {
					if time.Since(p.LastSeen) <= 15*time.Second {
						onlinePeers = append(onlinePeers, p)
					}
				}
				netState.mu.RUnlock()

				if len(onlinePeers) == 0 {
					drawUIText("No online peers in this world to transfer to.", mx+24, my+158, 17, rl.LightGray)
				} else {
					drawUIText("Select Recipient Peer:", mx+24, my+150, 17, rl.RayWhite)
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
						rowPeer := fmt.Sprintf("%s%s (%s...)", prefix, formatPeerTag(p.Nick, p.PubKey), p.PubKey[:12])
						drawUIText(truncateToWidth(rowPeer, 17, leftColW), mx+24, my+int32(178+i*28), 17, col)
					}
				}
				drawUIText("[Up/Down] Recipient | [L] Toggle Vendor-Lock | [Enter] Sign & Send | [Esc] Back", mx+24, my+mh-34, 15, rl.Gray)
			} else {
				if len(activeList) == 0 {
					drawUIText("No .glb files found. Drag & drop a .glb onto the window!", mx+24, my+98, 17, rl.LightGray)
				} else {
					curIdx := invCursor % len(activeList)
					startIdx := 0
					if curIdx >= 8 {
						startIdx = curIdx - 7
					}
					for row := 0; row < 8 && startIdx+row < len(activeList); row++ {
						i := startIdx + row
						item := activeList[i]
						prefix := "   "
						col := rl.RayWhite
						if i == curIdx {
							prefix = "-> "
							col = rl.Gold
						}
						equipped := ""
						if invTab == "avatar" && item.Hash == localAvHash {
							equipped = " [EQUIPPED]"
						}
						meta := formatByteSize(item.SizeBytes)
						if item.HasBounds {
							meta += fmt.Sprintf(" | %.2fx%.2fx%.2fm", item.Bounds[0], item.Bounds[1], item.Bounds[2])
						}
						rowTxt := fmt.Sprintf("%s%s (%s) [%s]%s%s", prefix, item.Name, item.Hash[:6], meta, item.StatusTag, equipped)
						drawUIText(truncateToWidth(rowTxt, 15, mw-330), mx+24, my+int32(88+row*32), 15, col)
					}
					if len(activeList) > 8 {
						drawUIText(fmt.Sprintf("Showing %d-%d of %d", startIdx+1, minInt(startIdx+8, len(activeList)), len(activeList)), mx+mw-465, my+52, 14, rl.Gray)
					}
				}
				drawUIText("[Up/Dn] Select | [Enter] Equip | [U] Upload .glb | [O] Open Folder | [R] Send | [D] Del", mx+24, my+mh-34, 15, rl.Gray)
			}
		}

		if settingsOpen {
			drawModalBackdrop(sw, sh)
			mw, mh := int32(580), int32(445)
			mx, my := (sw-mw)/2, (sh-mh)/2
			drawGlassPanel(mx, my, mw, mh, rl.SkyBlue, 246)
			drawUITextShadow("CLIENT SETTINGS (Saved to settings.toml)", mx+24, my+18, 19, rl.SkyBlue)

			opts := []string{
				fmt.Sprintf("Field of View (FOV) : %.0f deg", cfg.FOV),
				fmt.Sprintf("Mouse Sensitivity   : %.4f", cfg.MouseSens),
				fmt.Sprintf("Voice Volume        : %.0f%%", cfg.VoiceVolume*100),
				fmt.Sprintf("Ambient Radio Vol   : %.0f%%", cfg.AmbientVol*100),
				fmt.Sprintf("3rd-Person Av Scale : %.2fx", cfg.AvatarScale),
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
				drawUIText(prefix+line, mx+24, my+int32(62+i*34), 17, col)
			}
			drawUIText("[Up/Down] Select  |  [Left/Right] Adjust  |  [F2 / Esc] Close", mx+24, my+mh-34, 15, rl.Gray)
		}

		if peersOpen {
			drawModalBackdrop(sw, sh)
			mw, mh := int32(640), int32(380)
			mx, my := (sw-mw)/2, (sh-mh)/2
			drawGlassPanel(mx, my, mw, mh, rl.SkyBlue, 246)
			drawUITextShadow("ONLINE PEERS IN WORLD", mx+24, my+18, 19, rl.SkyBlue)

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
				drawUIText("No other peers currently connected.", mx+24, my+70, 17, rl.LightGray)
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
					drawUIText(fmt.Sprintf("%s%s (%s...) — %.1fm%s", prefix, formatPeerTag(p.Nick, p.PubKey), p.PubKey[:12], dist, tag), mx+24, my+int32(60+i*30), 17, col)
				}
			}
			drawUIText("[Up/Down] Select | [Enter] Teleport To | [B] Block/Mute | [F3 / Esc] Close", mx+24, my+mh-34, 15, rl.Gray)
		}

		if hasIncomingOffer && pendingDrop == nil {
			drawModalBackdrop(sw, sh)
			netState.mu.RLock()
			offer := netState.incomingXfers[0]
			netState.mu.RUnlock()

			mw, mh := int32(660), int32(220)
			mx, my := (sw-mw)/2, (sh-mh)/2
			drawGlassPanel(mx, my, mw, mh, rl.Gold, 248)
			drawUITextShadow("INCOMING KEY-SIGNED ASSET TRANSFER", mx+24, my+20, 19, rl.Gold)
			drawUIText(fmt.Sprintf("From: %s (%s...)", offer.FromNick, offer.FromPub[:12]), mx+24, my+52, 17, rl.RayWhite)
			drawUIText(fmt.Sprintf("Item: %s (%s)", offer.FileName, offer.AssetKind), mx+24, my+78, 17, rl.SkyBlue)
			licInfo := "License: VENDOR-LOCKED (Bound to your Ed25519 key; non-transferable)"
			if offer.License != nil && offer.License.Transferable {
				licInfo = "License: TRANSFERABLE (Bound to your Ed25519 key; resale allowed)"
			}
			drawUIText(licInfo, mx+24, my+106, 15, rl.Lime)
			drawUIText("[Y] Accept & Download   |   [N / Esc] Decline", mx+24, my+175, 18, rl.Gold)
		}

		if pendingDrop != nil {
			drawModalBackdrop(sw, sh)
			mw, mh := int32(600), int32(210)
			mx, my := (sw-mw)/2, (sh-mh)/2
			drawGlassPanel(mx, my, mw, mh, rl.Lime, 248)
			drawUITextShadow("IMPORTED .GLB (VALIDATED BY GLBGUARD)", mx+24, my+20, 19, rl.Lime)
			drawUIText("File: "+pendingDrop.FileName, mx+24, my+52, 18, rl.RayWhite)
			drawUIText("[1] Equip as My Live Avatar", mx+24, my+95, 18, rl.SkyBlue)
			drawUIText("[2] Add to Prop Inventory & Place", mx+24, my+125, 18, rl.Gold)
			drawUIText("[Esc] Cancel", mx+24, my+165, 16, rl.Gray)
		}

		if dirOpen {
			drawModalBackdrop(sw, sh)
			mw, mh := int32(720), int32(430)
			mx, my := (sw-mw)/2, (sh-mh)/2
			drawGlassPanel(mx, my, mw, mh, rl.SkyBlue, 246)
			drawUITextShadow("WORLD DIRECTORY (~/.vektorweb-client/worlds.toml)", mx+24, my+18, 19, rl.SkyBlue)

			dirStatusMu.RLock()
			for i, w := range directory.Worlds {
				if i >= 8 {
					break
				}
				rowY := my + int32(56+i*40)
				st, hasSt := dirStatuses[strings.TrimSpace(w.URL)]

				badgeTxt := "[CHECKING...]"
				badgeCol := rl.Gray
				dotCol := rl.Gray
				titleDisplay := w.Name
				if hasSt && st.Checked {
					if st.Online {
						dotCol = rl.Lime
						badgeCol = rl.Lime
						if normalizeWorldURL(w.URL) == "local://sandbox" {
							badgeTxt = "[LOCAL SANDBOX]"
						} else {
							badgeTxt = fmt.Sprintf("[ONLINE | %d online | %d props]", st.PeerCount, st.PropCount)
							if st.Title != "" && (titleDisplay == "" || titleDisplay == "Local Relay World") {
								titleDisplay = st.Title
							}
						}
					} else {
						dotCol = rl.Red
						badgeCol = rl.NewColor(255, 110, 110, 255)
						badgeTxt = "[OFFLINE]"
					}
				}

				isSel := (i == dirCursor)
				if isSel {
					rl.DrawRectangle(mx+18, rowY-3, mw-36, 37, rl.NewColor(26, 36, 54, 210))
				}
				rl.DrawCircle(mx+30, rowY+9, 4, dotCol)
				pinTag := ""
				if i == 0 {
					pinTag = " [PINNED]"
				}
				leftTitle := fmt.Sprintf("[%d] %s%s", i+1, sanitizeDisplayText(titleDisplay, 28), pinTag)
				titleCol := rl.RayWhite
				if isSel {
					titleCol = rl.Gold
				}
				drawUIText(leftTitle, mx+42, rowY, 17, titleCol)

				bw := uiTextWidth(badgeTxt, 15)
				drawUIText(badgeTxt, mx+mw-24-bw, rowY+2, 15, badgeCol)
				drawUIText(truncateToWidth(w.URL, 14, mw-70), mx+42, rowY+19, 14, rl.Gray)
			}
			dirStatusMu.RUnlock()

			if dirConnectActive {
				inBox := rl.NewRectangle(float32(mx+20), float32(my+mh-74), float32(mw-40), 32)
				rl.DrawRectangleRounded(inBox, 0.2, 6, rl.NewColor(16, 24, 38, 250))
				rl.DrawRectangleRoundedLinesEx(inBox, 0.2, 6, 1.3, rl.Lime)
				drawUIText(fitInputTail("URL: ", dirConnectInput, "_", 16, mw-56), mx+28, my+mh-67, 16, rl.Lime)
				drawUIText("[Enter] Connect & Save  |  [Ctrl+V] Paste  |  [Esc] Cancel", mx+24, my+mh-34, 15, rl.Gold)
			} else {
				drawUIText("[1-8/Enter] Hop | [N] Connect URL | [F] Pin #1 | [Del] Remove | [R] Refresh", mx+24, my+mh-34, 15, rl.Gold)
			}
		}

		if welcomeOpen {
			drawModalBackdrop(sw, sh)
			mw, mh := int32(660), int32(330)
			mx, my := (sw-mw)/2, (sh-mh)/2
			drawGlassPanel(mx, my, mw, mh, rl.Lime, 248)
			drawUITextShadow("WELCOME TO VEKTORWEB", mx+26, my+20, 21, rl.Lime)
			drawUIText("Your local Ed25519 identity & starter avatars are ready.", mx+26, my+52, 16, rl.RayWhite)
			drawUIText("• [Tab]      Browse live worlds or press [N] to connect to any server URL", mx+26, my+92, 16, rl.SkyBlue)
			drawUIText("• [I]        Switch starter avatars, upload your own .glb ([U]), or arm props", mx+26, my+122, 16, rl.SkyBlue)
			drawUIText("• [G] + [P]  Enter Build Mode and place props ([Q/E/Z/X] rotate & tilt)", mx+26, my+152, 16, rl.SkyBlue)
			drawUIText("• [V] / [T]  Hold [V] for 35m proximity voice or press [T] for chat", mx+26, my+182, 16, rl.SkyBlue)
			drawUIText("• [F1]       View full controls & slash commands anytime", mx+26, my+212, 16, rl.SkyBlue)
			drawUIText("Press [Space], [Enter], or [Esc] to enter the world", mx+26, my+mh-38, 16, rl.Gold)
		}

		if helpOpen {
			drawModalBackdrop(sw, sh)
			mw, mh := int32(720), int32(530)
			mx, my := (sw-mw)/2, (sh-mh)/2
			drawGlassPanel(mx, my, mw, mh, rl.Gold, 246)
			drawUITextShadow("VEKTORWEB CONTROLS & SLASH COMMANDS", mx+26, my+18, 19, rl.Gold)
			helpLines := []string{
				"[I]              : Local Inventory ([U] Upload .glb | [O] Open folder | [R] Send)",
				"Drag & Drop .glb : Equip avatar or import prop into inventory",
				"WASD + Mouse     : Walk / Look ([Shift] Sprint, [Space] Jump, [F] Walk/Fly)",
				"[C] / [R]        : Toggle 3rd-Person View (Wheel zooms) / Return to Spawn",
				"Look-At Target   : [E] Interact/Trade | [C] Clone prop | [Del] Remove prop",
				"[T] or [/]       : Chat (/help, /me, /outfit, /clearmyprops, /exportworld, /nick, /bookmark)",
				"[V] / [M] / [U]  : Hold Push-to-Talk (35m) / Mute voice / Mute world radio",
				"[G]/[Esc] [B] [N]: Toggle/Exit Build Mode / Snap-to-Grid / Next prop ([F4] Set Spawn)",
				"[Q/E] [Z/X] [PgU]: Yaw / Pitch & Roll tilt / Height ([+/-] scale, [P] place)",
				"[Tab] / [F12]    : World Portal Directory / Save Screenshot to exports/",
				"[F2] / [F3]      : Client Settings (FOV, Sens, Fog) / Online Peers List",
			}
			for i, hl := range helpLines {
				drawUIText(hl, mx+26, my+int32(56+i*38), 16, rl.RayWhite)
			}
		}

		rl.EndDrawing()
	}
}

var (
	uiFont    rl.Font
	hasUIFont bool
)

func loadBestUIFont(baseDir string) (rl.Font, bool) {
	candidates := []string{
		filepath.Join(baseDir, "font.ttf"),
		filepath.Join(baseDir, "font.otf"),
		"/usr/share/fonts/TTF/Inter-Regular.ttf",
		"/usr/share/fonts/TTF/JetBrainsMono-Regular.ttf",
		"/usr/share/fonts/TTF/DejaVuSans.ttf",
		"/usr/share/fonts/TTF/DejaVuSansMono.ttf",
		"/usr/share/fonts/TTF/LiberationSans-Regular.ttf",
		"/usr/share/fonts/TTF/Hack-Regular.ttf",
		"/usr/share/fonts/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/noto/NotoSansMono-Regular.ttf",
		"/usr/share/fonts/Adwaita/AdwaitaSans-Regular.ttf",
		"/usr/share/fonts/gnu-free/FreeSans.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/usr/local/share/fonts/dejavu/DejaVuSans.ttf",
		"/usr/local/share/fonts/Liberation/LiberationSans-Regular.ttf",
		"/usr/X11R6/lib/X11/fonts/TTF/DejaVuSans.ttf",
		"C:\\Windows\\Fonts\\segoeui.ttf",
		"C:\\Windows\\Fonts\\consola.ttf",
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			f := rl.LoadFontEx(p, 32, nil, 512)
			if f.BaseSize > 0 && f.Texture.ID > 0 {
				rl.SetTextureFilter(f.Texture, rl.FilterBilinear)
				return f, true
			}
		}
	}
	return rl.GetFontDefault(), false
}

func uiTextWidth(text string, fontSize int32) int32 {
	if hasUIFont {
		sz := rl.MeasureTextEx(uiFont, text, float32(fontSize), 1.0)
		return int32(math.Ceil(float64(sz.X)))
	}
	return rl.MeasureText(text, fontSize)
}

func drawUIText(text string, x, y, fontSize int32, col rl.Color) {
	if hasUIFont {
		rl.DrawTextEx(uiFont, text, rl.NewVector2(float32(x), float32(y)), float32(fontSize), 1.0, col)
		return
	}
	rl.DrawText(text, x, y, fontSize, col)
}

func drawUITextShadow(text string, x, y, fontSize int32, col rl.Color) {
	drawUIText(text, x+1, y+1, fontSize, rl.NewColor(4, 6, 11, 210))
	drawUIText(text, x, y, fontSize, col)
}

func drawGlassPanel(x, y, w, h int32, accent rl.Color, bgAlpha uint8) {
	// Soft drop shadow for depth separation over 3D geometry
	shadowRec := rl.NewRectangle(float32(x+3), float32(y+5), float32(w), float32(h))
	rl.DrawRectangleRounded(shadowRec, 0.035, 10, rl.NewColor(3, 5, 10, 130))

	rec := rl.NewRectangle(float32(x), float32(y), float32(w), float32(h))
	rl.DrawRectangleRounded(rec, 0.035, 10, rl.NewColor(13, 17, 27, bgAlpha))
	rl.DrawRectangleRoundedLinesEx(rec, 0.035, 10, 1.25, rl.NewColor(66, 86, 122, 220))
	// Crisp top accent bar
	rl.DrawRectangle(x+14, y+1, w-28, 2, accent)
}

func drawModalBackdrop(sw, sh int32) {
	rl.DrawRectangle(0, 0, sw, sh, rl.NewColor(5, 7, 13, 145))
}

func drawKeyBadge(text string, x, y, fontSize int32, fg, bg, border rl.Color) int32 {
	tw := uiTextWidth(text, fontSize)
	bw := tw + 14
	bh := fontSize + 6
	rec := rl.NewRectangle(float32(x), float32(y), float32(bw), float32(bh))
	rl.DrawRectangleRounded(rec, 0.28, 6, bg)
	rl.DrawRectangleRoundedLinesEx(rec, 0.28, 6, 1.0, border)
	drawUIText(text, x+7, y+3, fontSize, fg)
	return bw
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

func pickLocalGLBFileDialog(title string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	switch runtime.GOOS {
	case "windows":
		ps := `Add-Type -AssemblyName System.Windows.Forms; $f = New-Object System.Windows.Forms.OpenFileDialog; $f.Filter = 'glTF Binary (*.glb)|*.glb'; $f.Title = '` + strings.ReplaceAll(title, "'", "") + `'; if ($f.ShowDialog() -eq 'OK') { $f.FileName }`
		if out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-STA", "-Command", ps).Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	case "darwin":
		sc := `POSIX path of (choose file with prompt "` + strings.ReplaceAll(title, `"`, `'`) + `" of type {"glb"})`
		if out, err := exec.CommandContext(ctx, "osascript", "-e", sc).Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	default:
		if _, err := exec.LookPath("zenity"); err == nil {
			if out, err := exec.CommandContext(ctx, "zenity", "--file-selection", "--title="+title, "--file-filter=glTF Binary (*.glb) | *.glb *.GLB").Output(); err == nil {
				return strings.TrimSpace(string(out))
			}
		}
		if _, err := exec.LookPath("kdialog"); err == nil {
			if out, err := exec.CommandContext(ctx, "kdialog", "--getopenfilename", ".", "*.glb|glTF Binary (*.glb)").Output(); err == nil {
				return strings.TrimSpace(string(out))
			}
		}
		if _, err := exec.LookPath("yad"); err == nil {
			if out, err := exec.CommandContext(ctx, "yad", "--file", "--title="+title, "--file-filter=*.glb").Output(); err == nil {
				return strings.TrimSpace(string(out))
			}
		}
	}
	return ""
}

func openLocalFolderOS(dir string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		if _, err := exec.LookPath("xdg-open"); err == nil {
			cmd = exec.Command("xdg-open", dir)
		}
	}
	if cmd != nil && cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

func setClipboardSafe(text string) bool {
	rl.SetClipboardText(text)
	var candidates [][]string
	switch runtime.GOOS {
	case "windows":
		candidates = [][]string{{"cmd", "/c", "clip"}}
	case "darwin":
		candidates = [][]string{{"pbcopy"}}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			candidates = append(candidates, []string{"wl-copy"})
		}
		candidates = append(candidates,
			[]string{"xclip", "-selection", "clipboard"},
			[]string{"xsel", "--clipboard", "--input"},
			[]string{"wl-copy"},
		)
	}
	for _, args := range candidates {
		bin, err := exec.LookPath(args[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(bin, args[1:]...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			continue
		}
		if err := cmd.Start(); err != nil {
			_ = stdin.Close()
			continue
		}
		_, _ = io.WriteString(stdin, text)
		_ = stdin.Close()
		go func() { _ = cmd.Wait() }()
		return true
	}
	return rl.GetClipboardText() == text
}

func getClipboardSafe() string {
	if got := rl.GetClipboardText(); got != "" {
		return got
	}
	var candidates [][]string
	switch runtime.GOOS {
	case "windows":
		candidates = [][]string{{"powershell", "-NoProfile", "-Command", "Get-Clipboard"}}
	case "darwin":
		candidates = [][]string{{"pbpaste"}}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			candidates = append(candidates, []string{"wl-paste", "-n"})
		}
		candidates = append(candidates,
			[]string{"xclip", "-selection", "clipboard", "-o"},
			[]string{"xsel", "--clipboard", "--output"},
			[]string{"wl-paste", "-n"},
		)
	}
	for _, args := range candidates {
		bin, err := exec.LookPath(args[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, args[1:]...).Output()
		if err == nil && len(out) > 0 {
			return string(out)
		}
	}
	return ""
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
	if maxWidth <= 40 || uiTextWidth(text, fontSize) <= maxWidth {
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
		if uiTextWidth(candidate, fontSize) <= maxWidth {
			cur = candidate
			continue
		}
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
		// Break oversized single tokens (such as 64-char pubkeys or URLs) by rune width
		if uiTextWidth(w, fontSize) > maxWidth {
			var chunk strings.Builder
			for _, r := range w {
				test := chunk.String() + string(r)
				if uiTextWidth(test, fontSize) > maxWidth && chunk.Len() > 0 {
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
	if text == "" || maxWidth <= 24 || uiTextWidth(text, fontSize) <= maxWidth {
		return text
	}
	runes := []rune(text)
	for i := len(runes) - 1; i >= 1; i-- {
		cand := string(runes[:i]) + "..."
		if uiTextWidth(cand, fontSize) <= maxWidth {
			return cand
		}
	}
	return "..."
}

func fitInputTail(prefix, input, suffix string, fontSize, maxWidth int32) string {
	full := prefix + input + suffix
	if maxWidth <= 40 || uiTextWidth(full, fontSize) <= maxWidth {
		return full
	}
	runes := []rune(input)
	for i := 1; i <= len(runes); i++ {
		cand := prefix + "..." + string(runes[i:]) + suffix
		if uiTextWidth(cand, fontSize) <= maxWidth {
			return cand
		}
	}
	return prefix + suffix
}

func drawMiniRadar(cx, cy, radius int32, cam rl.Camera3D, lookVec rl.Vector3, ns *NetState, portals []WorldPortal) {
	rl.DrawCircle(cx, cy, float32(radius), rl.NewColor(11, 15, 24, 220))
	rl.DrawCircleLines(cx, cy, float32(radius), rl.NewColor(75, 105, 145, 235))
	rl.DrawCircleLines(cx, cy, float32(radius)*0.55, rl.NewColor(45, 62, 88, 160))
	rl.DrawLine(cx-radius+6, cy, cx+radius-6, cy, rl.NewColor(40, 55, 78, 140))
	rl.DrawLine(cx, cy-radius+6, cx, cy+radius-6, rl.NewColor(40, 55, 78, 140))
	rl.DrawCircle(cx, cy, 3.5, rl.Lime)

	flatLook := rl.Vector3Normalize(rl.NewVector3(lookVec.X, 0, lookVec.Z))
	rl.DrawLine(cx, cy, cx+int32(flatLook.X*18), cy+int32(flatLook.Z*18), rl.Lime)

	for _, portal := range portals {
		if len(portal.Pos) != 3 {
			continue
		}
		dx := (portal.Pos[0] - cam.Position.X) * 1.5
		dz := (portal.Pos[2] - cam.Position.Z) * 1.5
		dist := float32(math.Sqrt(float64(dx*dx + dz*dz)))
		if dist > float32(radius-5) {
			scale := float32(radius-5) / dist
			dx *= scale
			dz *= scale
		}
		rl.DrawCircleLines(cx+int32(dx), cy+int32(dz), 4.0, rl.NewColor(110, 220, 255, 230))
	}

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
	hopFn func(string, bool),
	visitedWorlds *[]string,
	showRangeRing *bool,
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
	input = strings.TrimSpace(input)
	if input == "/help" || input == "/?" || strings.HasPrefix(input, "/help ") {
		ns.addChat("[HELP]: Commands: /help, /debug, /uploadavatar, /openavatars, /clear, /range, /back, /history, /clearmyprops, /exportworld, /me <act>, /outfit, /nick, /say, /w, /portal, /bookmark, /setspawn, /copykey")
		ns.addChat("[HELP]: Keys: [F1] Full Help | [I] Inventory | [G]/[Esc] Build | [C] 3P | [F] Walk/Fly | [H] Waypoint | [0-9] Anim Clip | [R] Spawn | [V] Voice")
		return
	}
	if input == "/debug" {
		ns.mu.RLock()
		conn := ns.connected
		host := ns.relayHost
		peers := len(ns.peers)
		props := len(ns.props)
		asms := len(ns.assemblies)
		avH := ns.localAvHash
		ns.mu.RUnlock()
		logPath := filepath.Join(filepath.Dir(ns.cacheDir), "client.log")
		diag := fmt.Sprintf("[DEBUG]: FPS=%d conn=%v (%s) peers=%d props=%d dl_active=%d av=%s.. log=%s",
			rl.GetFPS(), conn, host, peers, props, asms, avH[:minInt(8, len(avH))], logPath)
		log.Print(diag)
		ns.addChat(diag)
		return
	}
	if input == "/range" {
		if showRangeRing != nil {
			*showRangeRing = !*showRangeRing
			if *showRangeRing {
				ns.addChat("[HUD]: 35m spatial voice & /say proximity ring ON")
			} else {
				ns.addChat("[HUD]: 35m spatial voice & /say proximity ring OFF")
			}
		}
		return
	}
	if input == "/history" {
		if visitedWorlds == nil || len(*visitedWorlds) <= 1 {
			ns.addChat("[HISTORY]: Only current world visited this session: " + activeWorldURL)
			return
		}
		for i, u := range *visitedWorlds {
			ns.addChat(fmt.Sprintf("[HISTORY %d]: %s", i+1, u))
		}
		return
	}
	if input == "/back" {
		if visitedWorlds == nil || len(*visitedWorlds) <= 1 {
			ns.addChat("[PORTAL]: No previous world in session history.")
			return
		}
		prev := (*visitedWorlds)[len(*visitedWorlds)-1]
		*visitedWorlds = (*visitedWorlds)[:len(*visitedWorlds)-1]
		hopFn(prev, false)
		return
	}
	if input == "/clear" {
		ns.clearChat()
		return
	}
	if input == "/clearmyprops" {
		myPub := kp.PubHex()
		ns.mu.Lock()
		var removedIDs []string
		for id, pr := range ns.props {
			if strings.EqualFold(pr.Owner, myPub) {
				removedIDs = append(removedIDs, id)
				delete(ns.props, id)
			}
		}
		ns.mu.Unlock()

		for _, id := range removedIDs {
			_ = ns.sendJSON(WireMsg{Type: "PROP_REMOVE", PropID: id})
		}
		_ = ns.sendJSON(WireMsg{Type: "CHAT", Channel: "global", Text: "/clearmyprops"})
		ns.addChat(fmt.Sprintf("[PROP]: Cleared %d of your placed props.", len(removedIDs)))
		return
	}
	if input == "/copykey" {
		pub := kp.PubHex()
		ok := setClipboardSafe(pub)
		if ok {
			ns.addChat("[PUBKEY]: Copied full 64-char key to clipboard: " + pub)
		} else {
			keyTxtPath := filepath.Join(exportsDir, "my_pubkey.txt")
			_ = os.WriteFile(keyTxtPath, []byte(pub+"\n"), 0644)
			ns.addChat("[PUBKEY]: " + pub)
			ns.addChat("[PUBKEY]: Clipboard unavailable — saved full key to exports/my_pubkey.txt")
		}
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
	if input == "/exportworld" {
		ns.mu.RLock()
		type snapProp struct {
			id       string
			hash     string
			owner    string
			pos      rl.Vector3
			yaw      float32
			pitch    float32
			roll     float32
			scale    float32
			animClip int
		}
		var list []snapProp
		for _, pr := range ns.props {
			list = append(list, snapProp{
				id:       pr.PropID,
				hash:     pr.Hash,
				owner:    pr.Owner,
				pos:      pr.Pos,
				yaw:      pr.Yaw,
				pitch:    pr.Pitch,
				roll:     pr.Roll,
				scale:    pr.Scale,
				animClip: pr.AnimClip,
			})
		}
		ns.mu.RUnlock()

		sort.Slice(list, func(i, j int) bool { return list[i].id < list[j].id })

		var sb strings.Builder
		fmt.Fprintf(&sb, "# VektorWeb World Prop Snapshot (%s)\n", time.Now().UTC().Format(time.RFC3339))
		fmt.Fprintf(&sb, "# World: %s (%s)\n", activeWorldTitle, activeWorldURL)
		fmt.Fprintf(&sb, "# Paste these [[props]] blocks into your server's world.toml and reload (SIGUSR1).\n\n")
		for _, sp := range list {
			s := sp.scale
			if s <= 0 {
				s = 1.0
			}
			fmt.Fprintf(&sb, "[[props]]\n")
			fmt.Fprintf(&sb, "id = %q\n", sp.id)
			fmt.Fprintf(&sb, "hash = %q\n", sp.hash)
			fmt.Fprintf(&sb, "owner = %q\n", sp.owner)
			fmt.Fprintf(&sb, "pos = [%.3f, %.3f, %.3f]\n", sp.pos.X, sp.pos.Y, sp.pos.Z)
			fmt.Fprintf(&sb, "yaw = %.2f\n", sp.yaw)
			if sp.pitch != 0 || sp.roll != 0 {
				fmt.Fprintf(&sb, "pitch = %.2f\n", sp.pitch)
				fmt.Fprintf(&sb, "roll = %.2f\n", sp.roll)
			}
			fmt.Fprintf(&sb, "scale = %.2f\n", s)
			if sp.animClip != 0 {
				fmt.Fprintf(&sb, "anim_clip = %d\n", sp.animClip)
			}
			fmt.Fprintf(&sb, "\n")
		}

		fname := fmt.Sprintf("world_snapshot_%s.toml", time.Now().Format("20060102_150405"))
		outPath := filepath.Join(exportsDir, fname)
		if err := os.WriteFile(outPath, []byte(sb.String()), 0644); err != nil {
			ns.addChat("[EXPORT ERR]: " + err.Error())
			return
		}
		if len(list) > 0 {
			if setClipboardSafe(sb.String()) {
				ns.addChat(fmt.Sprintf("[EXPORT]: Saved %d props to exports/%s & copied TOML to clipboard!", len(list), fname))
			} else {
				ns.addChat(fmt.Sprintf("[EXPORT]: Saved %d props to exports/%s", len(list), fname))
			}
		} else {
			ns.addChat(fmt.Sprintf("[EXPORT]: Saved empty snapshot to exports/%s (0 props in world).", fname))
		}
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
			fullTag := strings.ToLower(formatPeerTag(p.Nick, pub))
			if strings.HasPrefix(strings.ToLower(pub), query) || strings.EqualFold(p.Nick, query) || fullTag == query {
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
			hopFn(u, true)
		}
		return
	}
	if strings.HasPrefix(input, "/me ") {
		act := sanitizeDisplayText(strings.TrimSpace(strings.TrimPrefix(input, "/me ")), 160)
		if act != "" {
			ns.mu.RLock()
			n := ns.nick
			ns.mu.RUnlock()
			formatted := fmt.Sprintf("* %s %s", formatPeerTag(n, kp.PubHex()), act)
			_ = ns.sendJSON(WireMsg{Type: "CHAT", Channel: "global", Text: "* " + act})
			ns.addChatColor(formatted, rl.NewColor(235, 195, 120, 255))
		}
		return
	}
	if input == "/outfit" || strings.HasPrefix(input, "/outfit ") {
		if cfg == nil {
			return
		}
		if cfg.Outfits == nil {
			cfg.Outfits = make(map[string]OutfitPreset)
		}
		fields := strings.Fields(strings.TrimPrefix(input, "/outfit"))
		if len(fields) == 0 || fields[0] == "list" {
			if len(cfg.Outfits) == 0 {
				ns.addChat("[OUTFIT]: No saved outfits yet. Use /outfit save <slot> and /outfit <slot>.")
				return
			}
			var slots []string
			for k := range cfg.Outfits {
				slots = append(slots, k)
			}
			sort.Strings(slots)
			for _, sl := range slots {
				op := cfg.Outfits[sl]
				avLabel := op.AvatarName
				if avLabel == "" && len(op.AvatarHash) >= 8 {
					avLabel = op.AvatarHash[:8]
				}
				ns.addChat(fmt.Sprintf("[OUTFIT %s]: nick=%q avatar=%s", sl, op.Nick, avLabel))
			}
			return
		}
		if fields[0] == "save" {
			if len(fields) < 2 {
				ns.addChat("[USAGE]: /outfit save <slot-name-or-number>")
				return
			}
			slot := sanitizeFileName(fields[1])
			ns.mu.RLock()
			curN := ns.nick
			curH := ns.localAvHash
			ns.mu.RUnlock()
			avName := ""
			for _, it := range *avatarInv {
				if it.Hash == curH {
					avName = it.Name
					break
				}
			}
			cfg.Outfits[slot] = OutfitPreset{
				Nick:       curN,
				AvatarHash: curH,
				AvatarName: avName,
			}
			saveClientSettings(settingsPath, *cfg)
			ns.addChat(fmt.Sprintf("[OUTFIT]: Saved slot %q (nick=%s, avatar=%s)", slot, curN, avName))
			return
		}
		slot := sanitizeFileName(fields[0])
		if fields[0] == "load" && len(fields) >= 2 {
			slot = sanitizeFileName(fields[1])
		}
		op, ok := cfg.Outfits[slot]
		if !ok {
			ns.addChat(fmt.Sprintf("[OUTFIT]: Slot %q not found. Use /outfit list or /outfit save %s", slot, slot))
			return
		}
		ns.mu.Lock()
		if op.Nick != "" {
			ns.nick = sanitizeDisplayText(op.Nick, 24)
			cfg.Nick = ns.nick
		}
		if isValidHex64(op.AvatarHash) {
			if _, exists := ns.localAssets[op.AvatarHash]; exists {
				ns.localAvHash = op.AvatarHash
				ns.localAvBytes = ns.localAssets[op.AvatarHash]
				*localAvHash = op.AvatarHash
			}
		}
		curN := ns.nick
		curH := ns.localAvHash
		cfg.AvatarHash = curH
		pos := ns.localPos
		ns.mu.Unlock()
		saveClientSettings(settingsPath, *cfg)
		_ = ns.sendJSON(WireMsg{
			Type:       "STATE",
			Nick:       curN,
			Pos:        []float32{pos.X, pos.Y, pos.Z},
			AvatarHash: curH,
		})
		ns.addChat(fmt.Sprintf("[OUTFIT]: Loaded slot %q -> %s", slot, formatPeerTag(curN, kp.PubHex())))
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
			log.Printf("Relay dial failed (%s): %v (retrying in %v)", relayURL, err, backoff)
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
	curAvHash := ns.localAvHash
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
		AvatarHash:   curAvHash,
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
			log.Printf("Relay session closed (%s): %v", relayHost, err)
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
			p.AnimClip = msg.AnimClip
			if msg.Scale >= 0.25 && msg.Scale <= 4.0 {
				p.AvatarScale = msg.Scale
			} else if p.AvatarScale <= 0 {
				p.AvatarScale = 1.0
			}
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
			if math.IsNaN(float64(msg.Pitch)) || math.IsInf(float64(msg.Pitch), 0) {
				msg.Pitch = 0
			}
			if math.IsNaN(float64(msg.Roll)) || math.IsInf(float64(msg.Roll), 0) {
				msg.Roll = 0
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
				PropID:   msg.PropID,
				Owner:    msg.PubKey,
				Hash:     msg.Hash,
				Pos:      rl.NewVector3(msg.Pos[0], msg.Pos[1], msg.Pos[2]),
				Yaw:      msg.Yaw,
				Pitch:    msg.Pitch,
				Roll:     msg.Roll,
				Scale:    scale,
				AnimClip: msg.AnimClip,
			}
			needAsset := ensureAssetQueued(ns, msg.Hash)
			ns.mu.Unlock()

			if needAsset {
				ns.mu.RLock()
				_, ownerOnline := ns.peers[msg.PubKey]
				ns.mu.RUnlock()
				if ownerOnline && isValidHex64(msg.PubKey) {
					_ = ns.sendJSON(WireMsg{Type: "REQ_ASSET", TargetPub: msg.PubKey, Hash: msg.Hash})
				} else {
					_ = ns.sendJSON(WireMsg{Type: "REQ_ASSET", Hash: msg.Hash})
				}
			}

		case "PROP_ANIM":
			ns.mu.Lock()
			if pr, ok := ns.props[msg.PropID]; ok {
				pr.AnimClip = msg.AnimClip
			}
			ns.mu.Unlock()

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
				isSwarm := (msg.TargetPub == "")
				go func(requesterPub, assetHash string, blob []byte, swarm bool) {
					if swarm {
						var jb [1]byte
						_, _ = rand.Read(jb[:])
						time.Sleep(time.Duration(jb[0]%120) * time.Millisecond)
					}
					streamAssetChunks(ns, requesterPub, assetHash, blob)
				}(msg.PubKey, msg.Hash, data, isSwarm)
			}

		case "ASSET_CHUNK":
			handleIncomingChunk(ns, msg)

		case "CHAT":
			if msg.PubKey == "" {
				tag := sanitizeDisplayText(msg.Nick, 16)
				if tag == "" {
					tag = "RELAY"
				}
				ns.addChat(fmt.Sprintf("[%s]: %s", tag, msg.Text))
				continue
			}
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
			if msg.Channel == "global" && strings.HasPrefix(msg.Text, "* ") {
				ns.addChatColor(fmt.Sprintf("* %s %s", label, strings.TrimPrefix(msg.Text, "* ")), rl.NewColor(235, 195, 120, 255))
			} else {
				ns.addChat(fmt.Sprintf("[%s%s]: %s", prefix, label, msg.Text))
			}

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

	log.Printf("Completed P2P asset download %s.glb (%s)", hashLower[:12], formatByteSize(int64(len(full))))
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
		myPub := ns.localPub
		ns.mu.Unlock()

		if pending != nil && pending.License != nil && pending.License.Verify(hashLower, myPub) == nil {
			_ = os.Remove(worldOnlyMarkerPath(ns.cacheDir, hashLower))
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
		} else {
			if _, err := os.Stat(filepath.Join(ns.cacheDir, hashLower+".license.json")); os.IsNotExist(err) {
				_ = os.WriteFile(worldOnlyMarkerPath(ns.cacheDir, hashLower), []byte("display_only\n"), 0644)
			}
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
