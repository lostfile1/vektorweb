package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Keypair struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// AssetLicense is a creator-signed Ed25519 ownership certificate saved alongside
// a .glb asset (<hash>.license.json) to bind an asset hash to a specific buyer/owner
// and control whether the buyer is permitted to transfer/resell it to another peer.
type AssetLicense struct {
	AssetHash    string `json:"asset_hash"`
	CreatorPub   string `json:"creator_pub"`
	OwnerPub     string `json:"owner_pub"`
	Transferable bool   `json:"transferable"`
	IssuedAt     int64  `json:"issued_at"`
	Nonce        string `json:"nonce"`
	Signature    string `json:"signature"`
}

func (l *AssetLicense) CanonicalPayload() string {
	t := "false"
	if l.Transferable {
		t = "true"
	}
	return fmt.Sprintf(
		"VEKTORWEB_LICENSE_V1\n%s\n%s\n%s\n%s\n%d\n%s",
		strings.ToLower(strings.TrimSpace(l.AssetHash)),
		strings.ToLower(strings.TrimSpace(l.CreatorPub)),
		strings.ToLower(strings.TrimSpace(l.OwnerPub)),
		t,
		l.IssuedAt,
		strings.TrimSpace(l.Nonce),
	)
}

func (l *AssetLicense) Verify(expectedHash, expectedOwner string) error {
	if l == nil {
		return errors.New("missing license")
	}
	if len(l.AssetHash) != 64 || len(l.CreatorPub) != 64 || len(l.OwnerPub) != 64 {
		return errors.New("invalid license hash or pubkey length")
	}
	if expectedHash != "" && !strings.EqualFold(l.AssetHash, expectedHash) {
		return errors.New("license asset_hash does not match .glb hash")
	}
	if expectedOwner != "" && !strings.EqualFold(l.OwnerPub, expectedOwner) {
		return errors.New("license owner_pub does not match your pubkey")
	}
	if len(l.Nonce) == 0 || len(l.Nonce) > 64 {
		return errors.New("invalid license nonce")
	}
	pubRaw, err := hex.DecodeString(l.CreatorPub)
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		return errors.New("invalid creator_pub hex")
	}
	sigRaw, err := hex.DecodeString(l.Signature)
	if err != nil || len(sigRaw) != ed25519.SignatureSize {
		return errors.New("invalid signature hex")
	}
	if !ed25519.Verify(ed25519.PublicKey(pubRaw), []byte(l.CanonicalPayload()), sigRaw) {
		return errors.New("invalid Ed25519 signature on AssetLicense")
	}
	return nil
}

// MintAssetLicense creates and signs an AssetLicense binding assetHash to ownerPub.
// If transferable is false, the recipient cannot transfer or re-issue the asset.
func (k *Keypair) MintAssetLicense(assetHash, ownerPub string, transferable bool) (*AssetLicense, error) {
	assetHash = strings.ToLower(strings.TrimSpace(assetHash))
	ownerPub = strings.ToLower(strings.TrimSpace(ownerPub))
	if len(assetHash) != 64 || len(ownerPub) != 64 {
		return nil, errors.New("assetHash and ownerPub must be 64-char hex")
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, err
	}
	lic := &AssetLicense{
		AssetHash:    assetHash,
		CreatorPub:   strings.ToLower(k.PubHex()),
		OwnerPub:     ownerPub,
		Transferable: transferable,
		IssuedAt:     time.Now().Unix(),
		Nonce:        hex.EncodeToString(nonceBytes),
	}
	sig := ed25519.Sign(k.Priv, []byte(lic.CanonicalPayload()))
	lic.Signature = hex.EncodeToString(sig)
	return lic, nil
}

func SaveAssetLicense(path string, lic *AssetLicense) error {
	b, err := json.MarshalIndent(lic, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

func LoadAssetLicense(path string) (*AssetLicense, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lic AssetLicense
	if err := json.Unmarshal(b, &lic); err != nil {
		return nil, err
	}
	return &lic, nil
}

func LoadOrCreate(baseDir string) (*Keypair, error) {
	keyPath := filepath.Join(baseDir, "identity.key")
	if b, err := os.ReadFile(keyPath); err == nil {
		raw, err := hex.DecodeString(string(b))
		if err == nil && len(raw) == ed25519.PrivateKeySize {
			priv := ed25519.PrivateKey(raw)
			pub := priv.Public().(ed25519.PublicKey)
			return &Keypair{Priv: priv, Pub: pub}, nil
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(priv)), 0600); err != nil {
		return nil, err
	}
	return &Keypair{Priv: priv, Pub: pub}, nil
}

func (k *Keypair) PubHex() string {
	return hex.EncodeToString(k.Pub)
}

func (k *Keypair) SignChallenge(nonce string, timestamp int64, domain string) string {
	msg := fmt.Sprintf("grid-auth:%s:%d:%s", nonce, timestamp, domain)
	sig := ed25519.Sign(k.Priv, []byte(msg))
	return hex.EncodeToString(sig)
}

func VerifyChallenge(pubHex, nonce string, timestamp int64, domain, sigHex string) bool {
	now := time.Now().Unix()
	if timestamp < now-60 || timestamp > now+60 {
		return false
	}
	pubRaw, err := hex.DecodeString(pubHex)
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		return false
	}
	sigRaw, err := hex.DecodeString(sigHex)
	if err != nil || len(sigRaw) != ed25519.SignatureSize {
		return false
	}
	msg := fmt.Sprintf("grid-auth:%s:%d:%s", nonce, timestamp, domain)
	return ed25519.Verify(ed25519.PublicKey(pubRaw), []byte(msg), sigRaw)
}

func FileSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
