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
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	rl "github.com/gen2brain/raylib-go/raylib"
	"github.com/gorilla/websocket"

	"vektorweb/internal/glbguard"
	"vektorweb/internal/identity"
	"vektorweb/internal/luavm"
)

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
}

type WorldDirectory struct {
	Worlds []struct {
		Name string `toml:"name"`
		URL  string `toml:"url"`
	} `toml:"worlds"`
}

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

type RemotePeer struct {
	PubKey     string
	Nick       string
	Pos        rl.Vector3
	Yaw        float32
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
	Name string
	Hash string
	Kind string
}

type PendingDrop struct {
	FileName string
	Hash     string
	Bytes    []byte
}

type AssetAssembler struct {
	Total  int
	Chunks map[int][]byte
}

type NetState struct {
	mu           sync.RWMutex
	conn         *websocket.Conn
	writeMu      sync.Mutex
	peers        map[string]*RemotePeer
	props        map[string]*WorldProp
	requested    map[string]bool
	assemblies   map[string]*AssetAssembler
	readyModels  []string
	chatLog      []string
	nick         string
	muted        bool
	localPos     rl.Vector3
	localAvHash  string
	localAvBytes []byte
	localAssets  map[string][]byte
	cacheDir     string
	maxAssetB    int
	voiceQueue   [][]float32
	relayCancel  context.CancelFunc
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

func (n *NetState) addChat(line string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.chatLog = append(n.chatLog, line)
	if len(n.chatLog) > 9 {
		n.chatLog = n.chatLog[len(n.chatLog)-9:]
	}
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
	n.peers = make(map[string]*RemotePeer)
	n.props = make(map[string]*WorldProp)
	n.requested = make(map[string]bool)
	n.assemblies = make(map[string]*AssetAssembler)
	n.voiceQueue = nil
	n.mu.Unlock()
}

type LoadedWorld struct {
	URL         string
	Manifest    *WorldManifest
	EnvCacheGLB string
	Sandbox     *luavm.WorldVM
}

func normalizeWorldURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "vektorweb://") {
		rest := strings.TrimPrefix(trimmed, "vektorweb://")
		if strings.HasPrefix(rest, "127.0.0.1") || strings.HasPrefix(rest, "localhost") || strings.Contains(strings.Split(rest, "/")[0], ":") {
			return "http://" + rest
		}
		return "https://" + rest
	}
	return trimmed
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

	envURL := baseWorldURL + manifest.Assets.Environment
	envBytes, err := fetchBytes(envURL)
	if err != nil {
		return nil, fmt.Errorf("failed fetching environment.glb: %w", err)
	}
	actualHash := identity.FileSHA256(envBytes)
	if manifest.Assets.EnvironmentSHA256 != "" && actualHash != manifest.Assets.EnvironmentSHA256 {
		return nil, fmt.Errorf("SRI mismatch on environment.glb: got %s, want %s", actualHash, manifest.Assets.EnvironmentSHA256)
	}
	if err := glbguard.ValidateGLB(envBytes, 50*1024*1024); err != nil {
		return nil, fmt.Errorf("glbguard rejected world geometry: %w", err)
	}
	cachedEnvPath := filepath.Join(cacheDir, actualHash+".glb")
	_ = os.WriteFile(cachedEnvPath, envBytes, 0644)

	var sandbox *luavm.WorldVM
	if manifest.Assets.Script != "" {
		luaBytes, err := fetchBytes(baseWorldURL + manifest.Assets.Script)
		if err != nil {
			log.Printf("Warning: could not fetch world.lua: %v", err)
		} else {
			if manifest.Assets.ScriptSHA256 != "" && identity.FileSHA256(luaBytes) != manifest.Assets.ScriptSHA256 {
				return nil, fmt.Errorf("SRI mismatch on world.lua")
			}
			sandbox, err = luavm.NewSandbox(string(luaBytes))
			if err != nil {
				log.Printf("Warning: world.lua rejected by sandbox: %v", err)
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

func scanInventoryFolder(folder, kind, cacheDir string, maxBytes int, localAssets map[string][]byte) []InventoryItem {
	var out []InventoryItem
	entries, err := os.ReadDir(folder)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".glb") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(folder, e.Name()))
		if err != nil {
			continue
		}
		if err := glbguard.ValidateGLB(b, maxBytes); err != nil {
			continue
		}
		h := identity.FileSHA256(b)
		localAssets[h] = b
		_ = os.WriteFile(filepath.Join(cacheDir, h+".glb"), b, 0644)
		out = append(out, InventoryItem{
			Name: e.Name(),
			Hash: h,
			Kind: kind,
		})
	}
	return out
}


// resolveWalkMovement performs downward raycast against world meshes + base floor (Y=0)
// and horizontal wall probing so walking feels grounded while keeping [F] Free-Fly toggleable.
func resolveWalkMovement(cam *rl.Camera3D, worldModel rl.Model, dt float32, velY *float32, grounded *bool) {
	const (
		moveSpeed   = 6.5
		sprintMult  = 1.75
		mouseSens   = 0.0025
		eyeHeight   = 1.65
		gravity     = 22.0
		jumpSpeed   = 7.8
		wallRadius  = 0.45
		stepHeight  = 0.55
	)

	// Mouse look
	delta := rl.GetMouseDelta()
	forward := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))

	yaw := -delta.X * mouseSens
	pitch := -delta.Y * mouseSens

	// Rotate forward horizontally (yaw)
	cosY, sinY := float32(math.Cos(float64(yaw))), float32(math.Sin(float64(yaw)))
	fYaw := rl.NewVector3(
		forward.X*cosY+forward.Z*sinY,
		forward.Y,
		-forward.X*sinY+forward.Z*cosY,
	)

	// Clamp pitch to avoid gimbal flip
	newY := fYaw.Y + pitch
	if newY > 0.95 {
		newY = 0.95
	} else if newY < -0.95 {
		newY = -0.95
	}
	lookDir := rl.Vector3Normalize(rl.NewVector3(fYaw.X, newY, fYaw.Z))

	// Horizontal movement on XZ plane
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

	if rl.Vector3Length(wish) > 0.001 {
		wish = rl.Vector3Scale(rl.Vector3Normalize(wish), speed*dt)
		moveDir := rl.Vector3Normalize(wish)

		// Horizontal raycast wall check at waist height
		waistPos := rl.NewVector3(cam.Position.X, cam.Position.Y-eyeHeight+stepHeight, cam.Position.Z)
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
			cam.Position.X += wish.X
			cam.Position.Z += wish.Z
		}
	}

	// Jump with [Space]
	if *grounded && rl.IsKeyPressed(rl.KeySpace) {
		*velY = jumpSpeed
		*grounded = false
	}

	// Apply gravity
	*velY -= gravity * dt
	cam.Position.Y += (*velY) * dt

	// Downward ground raycast from hip height
	footY := cam.Position.Y - eyeHeight
	rayOrigin := rl.NewVector3(cam.Position.X, footY+stepHeight, cam.Position.Z)
	downRay := rl.NewRay(rayOrigin, rl.NewVector3(0, -1, 0))

	groundY := float32(-9999.0)
	// Base grid plane at Y=0 within [-32, 32]
	if cam.Position.X >= -32.0 && cam.Position.X <= 32.0 && cam.Position.Z >= -32.0 && cam.Position.Z <= 32.0 {
		groundY = 0.0
	}

	for i := int32(0); i < worldModel.MeshCount; i++ {
		hit := rl.GetRayCollisionMesh(downRay, worldModel.GetMeshes()[i], worldModel.Transform)
		if hit.Hit && hit.Point.Y > groundY && hit.Distance <= stepHeight+0.35 {
			groundY = hit.Point.Y
		}
	}

	if groundY > -9000.0 && footY <= groundY && *velY <= 0 {
		cam.Position.Y = groundY + eyeHeight
		*velY = 0
		*grounded = true
	} else {
		*grounded = false
	}

	// Respawn if fallen into void below Y = -25
	if cam.Position.Y < -25.0 {
		cam.Position = rl.NewVector3(0, eyeHeight+0.5, 10)
		*velY = 0
	}

	cam.Target = rl.Vector3Add(cam.Position, lookDir)
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
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(avatarsDir, 0755)
	_ = os.MkdirAll(itemsDir, 0755)

	kp, err := identity.LoadOrCreate(baseDir)
	if err != nil {
		log.Fatalf("identity error: %v", err)
	}
	nick := *nickFlag
	if nick == "" {
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

	avatarInv := scanInventoryFolder(avatarsDir, "avatar", cacheDir, maxAvMB*1024*1024, localAssets)
	propInv := scanInventoryFolder(itemsDir, "prop", cacheDir, maxItemMB*1024*1024, localAssets)

	if *propFile != "" {
		if b, err := os.ReadFile(*propFile); err == nil {
			if err := glbguard.ValidateGLB(b, maxItemMB*1024*1024); err == nil {
				h := identity.FileSHA256(b)
				localAssets[h] = b
				_ = os.WriteFile(filepath.Join(cacheDir, h+".glb"), b, 0644)
				propInv = append([]InventoryItem{{Name: filepath.Base(*propFile), Hash: h, Kind: "prop"}}, propInv...)
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
			localAvHash = identity.FileSHA256(b)
			localAvBytes = b
			localAssets[localAvHash] = b
			_ = os.WriteFile(filepath.Join(cacheDir, localAvHash+".glb"), b, 0644)
		}
	}

	propIdx := 0

	netState := &NetState{
		peers:        make(map[string]*RemotePeer),
		props:        make(map[string]*WorldProp),
		requested:    make(map[string]bool),
		assemblies:   make(map[string]*AssetAssembler),
		nick:         nick,
		localAvHash:  localAvHash,
		localAvBytes: localAvBytes,
		localAssets:  localAssets,
		cacheDir:     cacheDir,
		maxAssetB:    maxAvMB * 1024 * 1024,
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

	worldModel := rl.LoadModel(activeWorld.EnvCacheGLB)
	defer func() {
		rl.UnloadModel(worldModel)
		if activeWorld.Sandbox != nil {
			activeWorld.Sandbox.Close()
		}
	}()

	loadedModels := make(map[string]rl.Model)
	for _, pe := range propInv {
		loadedModels[pe.Hash] = rl.LoadModel(filepath.Join(cacheDir, pe.Hash+".glb"))
	}
	defer func() {
		for _, m := range loadedModels {
			rl.UnloadModel(m)
		}
	}()

	spawn := rl.NewVector3(0, 1.6, 10)
	if len(activeWorld.Manifest.World.Spawn) == 3 {
		spawn = rl.NewVector3(activeWorld.Manifest.World.Spawn[0], activeWorld.Manifest.World.Spawn[1], activeWorld.Manifest.World.Spawn[2])
	}

	cam := rl.Camera3D{
		Position:   spawn,
		Target:     rl.NewVector3(0, 1, 0),
		Up:         rl.NewVector3(0, 1, 0),
		Fovy:       70.0,
		Projection: rl.CameraPerspective,
	}

	hopToWorld := func(targetURL string) {
		targetURL = normalizeWorldURL(targetURL)
		netState.addChat("[PORTAL]: Loading " + targetURL + " ...")
		nextW, err := loadWorldFromURL(targetURL, cacheDir)
		if err != nil {
			netState.addChat("[PORTAL ERR]: " + err.Error())
			return
		}
		rl.UnloadModel(worldModel)
		if activeWorld.Sandbox != nil {
			activeWorld.Sandbox.Close()
		}
		activeWorld = nextW
		worldModel = rl.LoadModel(activeWorld.EnvCacheGLB)
		rl.SetWindowTitle("VektorWeb - " + activeWorld.Manifest.World.Title)

		newSpawn := rl.NewVector3(0, 1.6, 10)
		if len(activeWorld.Manifest.World.Spawn) == 3 {
			newSpawn = rl.NewVector3(activeWorld.Manifest.World.Spawn[0], activeWorld.Manifest.World.Spawn[1], activeWorld.Manifest.World.Spawn[2])
		}
		cam.Position = newSpawn
		cam.Target = rl.NewVector3(newSpawn.X, newSpawn.Y, newSpawn.Z-1.0)
		startRelayForWorld(activeWorld)
		netState.addChat("[PORTAL]: Arrived at " + activeWorld.Manifest.World.Title)
	}

	chatActive := false
	dirOpen := false
	helpOpen := false
	invOpen := false
	invTab := "prop"
	invCursor := 0

	walkMode := true
	velY := float32(0.0)
	grounded := false
	ghostActive := false
	snapToGrid := false
	ghostYawOffset := float32(0)
	ghostYOffset := float32(0)
	ghostScale := float32(1.0)
	var pendingDrop *PendingDrop

	chatInput := ""
	luaHUD := ""
	lastSync := time.Now()
	var ownedPropIDs []string

	rl.SetExitKey(0)
	cursorLocked := true
	rl.DisableCursor()
	for !rl.WindowShouldClose() {
		if !rl.IsWindowFocused() {
			rl.SetTargetFPS(15)
			if cursorLocked {
				rl.EnableCursor()
				cursorLocked = false
			}
		} else {
			rl.SetTargetFPS(60)
			if !chatActive && !invOpen && !dirOpen && pendingDrop == nil {
				if rl.IsKeyPressed(rl.KeyEscape) && cursorLocked {
					rl.EnableCursor()
					cursorLocked = false
				} else if rl.IsMouseButtonPressed(rl.MouseButtonLeft) && !cursorLocked {
					rl.DisableCursor()
					cursorLocked = true
				}
			}
		}
		dt := float64(rl.GetFrameTime())

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
				pendingDrop = &PendingDrop{
					FileName: filepath.Base(fpath),
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

				netState.mu.Lock()
				netState.localAvHash = pendingDrop.Hash
				netState.localAvBytes = pendingDrop.Bytes
				netState.localAssets[pendingDrop.Hash] = pendingDrop.Bytes
				netState.mu.Unlock()
				localAvHash = pendingDrop.Hash

				avatarInv = scanInventoryFolder(avatarsDir, "avatar", cacheDir, maxAvMB*1024*1024, localAssets)
				netState.addChat("[AVATAR]: Equipped " + pendingDrop.FileName + "!")
				pendingDrop = nil
			} else if rl.IsKeyPressed(rl.KeyTwo) {
				destP := filepath.Join(itemsDir, pendingDrop.FileName)
				_ = os.WriteFile(destP, pendingDrop.Bytes, 0644)
				cachedP := filepath.Join(cacheDir, pendingDrop.Hash+".glb")
				_ = os.WriteFile(cachedP, pendingDrop.Bytes, 0644)

				netState.mu.Lock()
				netState.localAssets[pendingDrop.Hash] = pendingDrop.Bytes
				netState.mu.Unlock()

				if _, exists := loadedModels[pendingDrop.Hash]; !exists {
					loadedModels[pendingDrop.Hash] = rl.LoadModel(cachedP)
				}
				propInv = scanInventoryFolder(itemsDir, "prop", cacheDir, maxItemMB*1024*1024, localAssets)
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
		netState.localPos = cam.Position
		netState.mu.Unlock()

		for _, h := range toLoad {
			if _, exists := loadedModels[h]; !exists {
				p := filepath.Join(cacheDir, h+".glb")
				loadedModels[h] = rl.LoadModel(p)
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
				rl.UpdateAudioStream(voiceStream, samples)
			}
		}

		if activeWorld.Sandbox != nil {
			actions, hud := activeWorld.Sandbox.Tick(dt, cam.Position.X, cam.Position.Y, cam.Position.Z)
			luaHUD = hud
			for _, act := range actions {
				switch act.Type {
				case "NOTIFY":
					netState.addChat("[WORLD]: " + act.Text)
				case "TELEPORT":
					offset := rl.Vector3Subtract(cam.Target, cam.Position)
					cam.Position = rl.NewVector3(act.Pos[0], act.Pos[1], act.Pos[2])
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

		if pendingDrop == nil && rl.IsKeyPressed(rl.KeyF1) {
			helpOpen = !helpOpen
		}

		if pendingDrop == nil && !chatActive && rl.IsKeyPressed(rl.KeyI) {
			invOpen = !invOpen
			dirOpen = false
			if invOpen {
				avatarInv = scanInventoryFolder(avatarsDir, "avatar", cacheDir, maxAvMB*1024*1024, localAssets)
				propInv = scanInventoryFolder(itemsDir, "prop", cacheDir, maxItemMB*1024*1024, localAssets)
				invCursor = 0
				rl.EnableCursor()
			} else {
				rl.DisableCursor()
			}
		}

		if pendingDrop == nil && !chatActive && rl.IsKeyPressed(rl.KeyTab) {
			dirOpen = !dirOpen
			invOpen = false
			if dirOpen {
				rl.EnableCursor()
			} else {
				rl.DisableCursor()
			}
		}

		forward := rl.Vector3Normalize(rl.Vector3Subtract(cam.Target, cam.Position))
		ghostPos := rl.Vector3Add(cam.Position, rl.Vector3Scale(forward, 3.0))
		ghostPos.Y = ghostYOffset
		ghostYaw := float32(math.Atan2(float64(forward.X), float64(forward.Z))*(180.0/math.Pi)) + ghostYawOffset

		if snapToGrid {
			ghostPos.X = float32(math.Round(float64(ghostPos.X)))
			ghostPos.Y = float32(math.Round(float64(ghostPos.Y)*2.0) / 2.0)
			ghostPos.Z = float32(math.Round(float64(ghostPos.Z)))
			ghostYaw = float32(math.Round(float64(ghostYaw)/45.0) * 45.0)
		}

		var activeProp InventoryItem
		if len(propInv) > 0 {
			activeProp = propInv[propIdx%len(propInv)]
		}

		if pendingDrop != nil {
		} else if invOpen {
			if rl.IsKeyPressed(rl.KeyOne) {
				invTab = "prop"
				invCursor = 0
			} else if rl.IsKeyPressed(rl.KeyTwo) {
				invTab = "avatar"
				invCursor = 0
			}

			activeList := propInv
			if invTab == "avatar" {
				activeList = avatarInv
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
			if rl.IsKeyPressed(rl.KeyEnter) && len(activeList) > 0 {
				chosen := activeList[invCursor%len(activeList)]
				if invTab == "avatar" {
					netState.mu.Lock()
					netState.localAvHash = chosen.Hash
					netState.localAvBytes = localAssets[chosen.Hash]
					netState.mu.Unlock()
					localAvHash = chosen.Hash
					netState.addChat("[AVATAR]: Equipped " + chosen.Name)
				} else {
					propIdx = invCursor % len(propInv)
					if _, ok := loadedModels[chosen.Hash]; !ok {
						loadedModels[chosen.Hash] = rl.LoadModel(filepath.Join(cacheDir, chosen.Hash+".glb"))
					}
					ghostActive = true
					netState.addChat("[INVENTORY]: Selected " + chosen.Name + " (Press [P] to place)")
				}
				invOpen = false
				rl.DisableCursor()
			} else if rl.IsKeyPressed(rl.KeyEscape) {
				invOpen = false
				rl.DisableCursor()
			}
		} else if dirOpen {
			for i := 0; i < len(directory.Worlds) && i < 9; i++ {
				if rl.IsKeyPressed(int32(rl.KeyOne + i)) {
					dirOpen = false
					rl.DisableCursor()
					hopToWorld(directory.Worlds[i].URL)
					break
				}
			}
		} else if !chatActive {
			if rl.IsKeyPressed(rl.KeyT) {
				chatActive = true
				rl.EnableCursor()
			} else {
				if rl.IsKeyPressed(rl.KeyF) {
					walkMode = !walkMode
					velY = 0
					if !cursorLocked {
					// Window unfocused or cursor unlocked with [Esc]
				} else if walkMode {
						netState.addChat("[MOVE]: Walk + Ground Collision ON")
					} else {
						netState.addChat("[MOVE]: Free-Fly Camera ON")
					}
				}
				if walkMode {
					resolveWalkMovement(&cam, worldModel, float32(dt), &velY, &grounded)
				} else {
					rl.UpdateCamera(&cam, rl.CameraFree)
				}

				if rl.IsKeyPressed(rl.KeyG) && activeProp.Hash != "" {
					ghostActive = !ghostActive
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
						ghostScale += 0.25
					}
					if rl.IsKeyPressed(rl.KeyMinus) && ghostScale > 0.25 {
						ghostScale -= 0.25
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

			for key := rl.GetCharPressed(); key > 0; key = rl.GetCharPressed() {
				if key >= 32 && key <= 125 && len(chatInput) < 200 {
					chatInput += string(rune(key))
				}
			}
			if rl.IsKeyPressed(rl.KeyBackspace) && len(chatInput) > 0 {
				chatInput = chatInput[:len(chatInput)-1]
			}
			if rl.IsKeyPressed(rl.KeyEnter) {
				if chatInput != "" {
					handleChatCommand(chatInput, kp, netState, hopToWorld)
					chatInput = ""
				}
				chatActive = false
				rl.DisableCursor()
			} else if rl.IsKeyPressed(rl.KeyEscape) {
				chatInput = ""
				chatActive = false
				rl.DisableCursor()
			}
		}

		if time.Since(lastSync) >= 33*time.Millisecond {
			lastSync = time.Now()
			dx := float64(cam.Target.X - cam.Position.X)
			dz := float64(cam.Target.Z - cam.Position.Z)
			yaw := float32(math.Atan2(dx, dz) * (180.0 / math.Pi))
			netState.mu.RLock()
			curNick := netState.nick
			netState.mu.RUnlock()

			_ = netState.sendJSON(WireMsg{
				Type:       "STATE",
				Nick:       curNick,
				Pos:        []float32{cam.Position.X, cam.Position.Y, cam.Position.Z},
				Yaw:        yaw,
				AvatarHash: localAvHash,
			})
		}

		rl.BeginDrawing()
		rl.ClearBackground(rl.NewColor(12, 14, 20, 255))

		rl.BeginMode3D(cam)
		rl.DrawGrid(32, 1.0)
		rl.DrawModel(worldModel, rl.Vector3Zero(), 1.0, rl.White)

		if ghostActive && activeProp.Hash != "" {
			if mod, ok := loadedModels[activeProp.Hash]; ok {
				rl.DrawModelEx(mod, ghostPos, rl.NewVector3(0, 1, 0), ghostYaw, rl.NewVector3(ghostScale, ghostScale, ghostScale), rl.Fade(rl.Lime, 0.55))
			}
			rl.DrawCircle3D(ghostPos, 0.5, rl.NewVector3(1, 0, 0), 90, rl.Lime)
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
			if time.Since(peer.LastSeen) > 10*time.Second {
				continue
			}
			if mod, ok := loadedModels[peer.AvatarHash]; ok {
				rl.DrawModelEx(mod, peer.Pos, rl.NewVector3(0, 1, 0), peer.Yaw, rl.NewVector3(1, 1, 1), rl.White)
			} else {
				rl.DrawCapsule(
					rl.NewVector3(peer.Pos.X, peer.Pos.Y-1.2, peer.Pos.Z),
					rl.NewVector3(peer.Pos.X, peer.Pos.Y+0.2, peer.Pos.Z),
					0.35, 8, 8, rl.SkyBlue,
				)
			}
			if time.Since(peer.Speaking) < 250*time.Millisecond {
				rl.DrawCircle3D(
					rl.NewVector3(peer.Pos.X, peer.Pos.Y-1.15, peer.Pos.Z),
					0.65, rl.NewVector3(1, 0, 0), 90, rl.Lime,
				)
			}
		}
		netState.mu.RUnlock()
		rl.EndMode3D()

		netState.mu.RLock()
		peerCount := len(netState.peers)
		propCount := len(netState.props)
		curNick := netState.nick
		isMuted := netState.muted
		lines := append([]string(nil), netState.chatLog...)
		netState.mu.RUnlock()

		rl.DrawText(fmt.Sprintf("World: %s | %s (%s...) | Peers: %d | Props: %d", activeWorld.Manifest.World.Title, curNick, kp.PubHex()[:8], peerCount, propCount), 16, 16, 18, rl.RayWhite)
		rl.DrawFPS(16, 42)
		if luaHUD != "" {
			rl.DrawText(luaHUD, 16, 68, 18, rl.Gold)
		}
		if ghostActive && activeProp.Name != "" {
			snapTxt := "OFF"
			if snapToGrid {
				snapTxt = "ON"
			}
			rl.DrawText(fmt.Sprintf("[BUILD: %s] Snap[B]:%s | Yaw:%.0f | Y:%.2f | Scale:%.2fx", activeProp.Name, snapTxt, ghostYawOffset, ghostYOffset, ghostScale), 16, 94, 16, rl.Lime)
		}
		if rl.IsKeyDown(rl.KeyV) && !chatActive {
			rl.DrawText("[MIC LIVE - SPATIAL TX]", 1010, 16, 18, rl.Lime)
		} else if isMuted {
			rl.DrawText("[AUDIO MUTED - M]", 1045, 16, 18, rl.Red)
		}

		drawMiniRadar(1190, 95, 65, cam, netState)

		y := int32(480)
		for _, line := range lines {
			rl.DrawText(line, 16, y, 18, rl.LightGray)
			y += 22
		}
		if chatActive {
			rl.DrawRectangle(14, 678, 760, 28, rl.NewColor(25, 30, 42, 220))
			rl.DrawText("> "+chatInput+"_", 20, 682, 18, rl.Green)
		} else {
			rl.DrawText("[I] Inv | [F] Walk/Fly | [Space] Jump | [T] Chat | [V] Voice | [G] Ghost | [P] Place | [F1] Help", 16, 684, 16, rl.Gray)
		}

		if invOpen {
			rl.DrawRectangle(310, 140, 660, 420, rl.NewColor(14, 18, 26, 245))
			rl.DrawRectangleLines(310, 140, 660, 420, rl.SkyBlue)
			rl.DrawText("LOCAL INVENTORY (~/.vektorweb-client/)", 335, 160, 20, rl.SkyBlue)

			propCol := rl.Gray
			avCol := rl.Gray
			activeList := propInv
			if invTab == "prop" {
				propCol = rl.Lime
			} else {
				avCol = rl.Lime
				activeList = avatarInv
			}
			rl.DrawText(fmt.Sprintf("[1] Props (%d)", len(propInv)), 335, 192, 18, propCol)
			rl.DrawText(fmt.Sprintf("[2] Avatars (%d)", len(avatarInv)), 505, 192, 18, avCol)

			if len(activeList) == 0 {
				rl.DrawText("No .glb files found. Drag & drop a .glb onto the window!", 335, 240, 17, rl.LightGray)
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
					rl.DrawText(fmt.Sprintf("%s%s (%s...)%s", prefix, item.Name, item.Hash[:8], equipped), 335, int32(232+i*28), 17, col)
				}
			}
			rl.DrawText("[Up/Down] Select  |  [Enter] Equip/Arm  |  [I / Esc] Close", 335, 525, 16, rl.Gray)
		}

		if pendingDrop != nil {
			rl.DrawRectangle(340, 230, 600, 210, rl.NewColor(14, 20, 30, 245))
			rl.DrawRectangleLines(340, 230, 600, 210, rl.Lime)
			rl.DrawText("IMPORTED .GLB (VALIDATED BY GLBGUARD)", 365, 250, 19, rl.Lime)
			rl.DrawText("File: "+pendingDrop.FileName, 365, 282, 18, rl.RayWhite)
			rl.DrawText("[1] Equip as My Live Avatar", 365, 325, 18, rl.SkyBlue)
			rl.DrawText("[2] Add to Prop Inventory & Place", 365, 355, 18, rl.Gold)
			rl.DrawText("[Esc] Cancel", 365, 395, 16, rl.Gray)
		}

		if dirOpen {
			rl.DrawRectangle(340, 160, 600, 360, rl.NewColor(16, 20, 30, 240))
			rl.DrawRectangleLines(340, 160, 600, 360, rl.SkyBlue)
			rl.DrawText("WORLD DIRECTORY (~/.vektorweb-client/worlds.toml)", 365, 180, 20, rl.SkyBlue)
			for i, w := range directory.Worlds {
				if i >= 9 {
					break
				}
				rl.DrawText(fmt.Sprintf("[%d] %s", i+1, w.Name), 365, int32(220+i*30), 18, rl.RayWhite)
				rl.DrawText(w.URL, 395, int32(238+i*30), 14, rl.Gray)
			}
			rl.DrawText("Press [1-9] to hop portal or [Tab] to close", 365, 485, 16, rl.Gold)
		}

		if helpOpen {
			rl.DrawRectangle(300, 120, 680, 490, rl.NewColor(14, 18, 26, 245))
			rl.DrawRectangleLines(300, 120, 680, 490, rl.Gold)
			rl.DrawText("GRID CONTROLS & SLASH COMMANDS", 330, 140, 20, rl.Gold)
			helpLines := []string{
				"[I]              : Open Local Inventory (Props & Avatars)",
				"Drag & Drop .glb : Equip avatar or import prop into inventory",
				"WASD + Mouse     : Walk / Look ([Shift] Sprint, [Space] Jump, [F] Walk/Fly)",
				"[T]              : Open chat (/nick, /say, /portal vektorweb://...)",
				"[V] (Hold)       : Spatial proximity voice (35m radius)",
				"[M]              : Mute / unmute incoming voice",
				"[G] / [B] / [N]  : Toggle 3D Ghost / Snap-to-Grid / Next prop",
				"[Q]/[E] [PgUp/Dn]: Rotate & nudge prop height ([+/-] scale)",
				"[P] / [X]        : Spawn prop in world / Undo last prop",
				"[Tab]            : Open World Discovery portal directory",
				"[F1]             : Close this help overlay",
			}
			for i, hl := range helpLines {
				rl.DrawText(hl, 330, int32(180+i*30), 17, rl.RayWhite)
			}
		}

		rl.EndDrawing()
	}
}

func drawMiniRadar(cx, cy, radius int32, cam rl.Camera3D, ns *NetState) {
	rl.DrawCircle(cx, cy, float32(radius), rl.NewColor(16, 20, 28, 200))
	rl.DrawCircleLines(cx, cy, float32(radius), rl.DarkGray)
	rl.DrawCircle(cx, cy, 3, rl.Green)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	for _, peer := range ns.peers {
		if time.Since(peer.LastSeen) > 10*time.Second {
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
		rl.DrawCircle(cx+int32(dx), cy+int32(dz), 3.5, rl.SkyBlue)
	}
}

func handleChatCommand(input string, kp *identity.Keypair, ns *NetState, hopFn func(string)) {
	if strings.HasPrefix(input, "/nick ") {
		newNick := strings.TrimSpace(strings.TrimPrefix(input, "/nick "))
		if newNick != "" {
			if len(newNick) > 24 {
				newNick = newNick[:24]
			}
			ns.mu.Lock()
			ns.nick = newNick
			ns.mu.Unlock()
			ns.addChat("[SYS]: Nickname set to " + newNick)
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
			ns.addChat(fmt.Sprintf("[LOCAL %s]: %s", n, txt))
		}
		return
	}

	_ = ns.sendJSON(WireMsg{Type: "CHAT", Channel: "global", Text: input})
	ns.mu.RLock()
	n := ns.nick
	ns.mu.RUnlock()
	ns.addChat(fmt.Sprintf("[%s]: %s", n, input))
}

func runRelayLoop(ctx context.Context, relayURL string, kp *identity.Keypair, ns *NetState) {
	u, err := url.Parse(relayURL)
	if err != nil {
		log.Printf("invalid relay URL: %v", err)
		return
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, relayURL, nil)
	if err != nil {
		log.Printf("relay offline (%v), running in local explore mode", err)
		return
	}
	defer conn.Close()

	var challenge WireMsg
	if err := conn.ReadJSON(&challenge); err != nil || challenge.Type != "CHALLENGE" {
		return
	}

	ns.mu.RLock()
	curNick := ns.nick
	ns.mu.RUnlock()

	sig := kp.SignChallenge(challenge.Nonce, challenge.Timestamp, u.Hostname())
	reply := WireMsg{
		Type:       "AUTH",
		Nonce:      challenge.Nonce,
		Timestamp:  challenge.Timestamp,
		PubKey:     kp.PubHex(),
		Nick:       curNick,
		Signature:  sig,
		AvatarHash: ns.localAvHash,
	}
	if err := conn.WriteJSON(reply); err != nil {
		return
	}

	ns.mu.Lock()
	ns.conn = conn
	ns.mu.Unlock()
	log.Printf("Connected & authenticated with relay %s", u.Hostname())

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

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
				if ns.muted {
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
		case "STATE":
			if len(msg.Pos) != 3 {
				continue
			}
			ns.mu.Lock()
			p, ok := ns.peers[msg.PubKey]
			if !ok {
				p = &RemotePeer{PubKey: msg.PubKey}
				ns.peers[msg.PubKey] = p
			}
			if msg.Nick != "" {
				p.Nick = msg.Nick
			} else if p.Nick == "" && len(msg.PubKey) >= 8 {
				p.Nick = msg.PubKey[:8]
			}
			p.Pos = rl.NewVector3(msg.Pos[0], msg.Pos[1], msg.Pos[2])
			p.Yaw = msg.Yaw
			p.AvatarHash = msg.AvatarHash
			p.LastSeen = time.Now()

			needAsset := ensureAssetQueued(ns, msg.AvatarHash)
			ns.mu.Unlock()

			if needAsset {
				_ = ns.sendJSON(WireMsg{Type: "REQ_ASSET", TargetPub: msg.PubKey, Hash: msg.AvatarHash})
			}

		case "PROP_SPAWN":
			if len(msg.Pos) != 3 || msg.Hash == "" || msg.PropID == "" {
				continue
			}
			ns.mu.Lock()
			ns.props[msg.PropID] = &WorldProp{
				PropID: msg.PropID,
				Owner:  msg.PubKey,
				Hash:   msg.Hash,
				Pos:    rl.NewVector3(msg.Pos[0], msg.Pos[1], msg.Pos[2]),
				Yaw:    msg.Yaw,
				Scale:  msg.Scale,
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
			ns.mu.RLock()
			data, ok := ns.localAssets[msg.Hash]
			ns.mu.RUnlock()
			if ok && len(data) > 0 {
				go streamAssetChunks(ns, msg.PubKey, msg.Hash, data)
			}

		case "ASSET_CHUNK":
			handleIncomingChunk(ns, msg)

		case "CHAT":
			label := msg.Nick
			if label == "" && len(msg.PubKey) >= 8 {
				label = msg.PubKey[:8]
			}
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

func ensureAssetQueued(ns *NetState, hash string) bool {
	if hash == "" || hash == "none" || ns.requested[hash] {
		return false
	}
	cachedPath := filepath.Join(ns.cacheDir, hash+".glb")
	if _, err := os.Stat(cachedPath); err == nil {
		ns.requested[hash] = true
		ns.readyModels = append(ns.readyModels, hash)
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
	if msg.Total <= 0 || msg.Total > 512 || msg.Seq < 0 || msg.Seq >= msg.Total {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		return
	}

	ns.mu.Lock()
	asm, ok := ns.assemblies[msg.Hash]
	if !ok {
		asm = &AssetAssembler{Total: msg.Total, Chunks: make(map[int][]byte)}
		ns.assemblies[msg.Hash] = asm
	}
	asm.Chunks[msg.Seq] = raw
	complete := len(asm.Chunks) == asm.Total
	var full []byte
	if complete {
		for i := 0; i < asm.Total; i++ {
			full = append(full, asm.Chunks[i]...)
		}
		delete(ns.assemblies, msg.Hash)
	}
	ns.mu.Unlock()

	if !complete {
		return
	}

	if identity.FileSHA256(full) != msg.Hash {
		log.Printf("Rejected peer asset %s: SHA-256 mismatch", msg.Hash[:12])
		return
	}
	if err := glbguard.ValidateGLB(full, ns.maxAssetB); err != nil {
		log.Printf("Rejected peer asset %s via glbguard: %v", msg.Hash[:12], err)
		return
	}

	outPath := filepath.Join(ns.cacheDir, msg.Hash+".glb")
	if err := os.WriteFile(outPath, full, 0644); err == nil {
		ns.mu.Lock()
		ns.readyModels = append(ns.readyModels, msg.Hash)
		ns.mu.Unlock()
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

func fetchManifest(rawURL string) (*WorldManifest, string, error) {
	b, err := fetchBytes(rawURL)
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
	parsed.Path = filepath.Dir(parsed.Path) + "/"
	return &m, parsed.String(), nil
}

func fetchBytes(targetURL string) ([]byte, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(targetURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, targetURL)
	}
	return io.ReadAll(resp.Body)
}
