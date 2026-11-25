package glbguard

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

type gltfHeader struct {
	Asset struct {
		Version string `json:"version"`
	} `json:"asset"`
	Buffers []struct {
		URI        string `json:"uri"`
		ByteLength int    `json:"byteLength"`
	} `json:"buffers"`
	Images []struct {
		URI string `json:"uri"`
	} `json:"images"`
	Meshes []any `json:"meshes"`
	Nodes  []any `json:"nodes"`
}

// ValidateGLB checks binary glTF 2.0 magic, chunk bounds, size limits, and blocks external URIs.
func ValidateGLB(data []byte, maxBytes int) error {
	if len(data) < 20 {
		return errors.New("file too small to be a valid GLB")
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return fmt.Errorf("GLB exceeds max size (%d > %d bytes)", len(data), maxBytes)
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	if magic != 0x46546C67 { // "glTF"
		return errors.New("invalid GLB magic header")
	}
	version := binary.LittleEndian.Uint32(data[4:8])
	if version != 2 {
		return fmt.Errorf("unsupported glTF version %d (must be 2)", version)
	}
	totalLen := binary.LittleEndian.Uint32(data[8:12])
	if int(totalLen) != len(data) {
		return fmt.Errorf("GLB length mismatch (header=%d, actual=%d)", totalLen, len(data))
	}

	jsonLen := binary.LittleEndian.Uint32(data[12:16])
	jsonType := binary.LittleEndian.Uint32(data[16:20])
	if jsonType != 0x4E4F534A { // "JSON"
		return errors.New("first GLB chunk is not JSON")
	}
	if int(20+jsonLen) > len(data) {
		return errors.New("JSON chunk overflows file size")
	}

	var doc gltfHeader
	if err := json.Unmarshal(data[20:20+jsonLen], &doc); err != nil {
		return fmt.Errorf("corrupt GLB JSON chunk: %w", err)
	}
	if len(doc.Nodes) > 512 || len(doc.Meshes) > 256 {
		return errors.New("GLB exceeds node/mesh complexity cap")
	}
	for _, b := range doc.Buffers {
		if b.URI != "" {
			return errors.New("external buffer URIs are forbidden in GLB")
		}
	}
	for _, img := range doc.Images {
		if img.URI != "" {
			return errors.New("external image URIs are forbidden in GLB")
		}
	}
	return nil
}
