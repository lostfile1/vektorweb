package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Keypair struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
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
