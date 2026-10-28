// Package artifact stores bounded streams using zstd and Tink AES-128-GCM.
//
// NewStream binds the manifest before payload streaming and exposes single-use
// pipeline stages. After a successful run, Envelope seals the ciphertext size
// and digest in the unchanged v1 format. The caller owns durable publication;
// closing stages alone does not publish an artifact. Write and Read retain the
// payload.bin/envelope.json layout, while ReadFiles accepts explicit member paths.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
)

const streamingCipher = "aes-128-gcm/tink-aes128gcmhkdf1mb"

type Options struct {
	Compress, Encrypt bool
	KeyFile           string
}
type ReadOptions struct {
	KeyFile, WorkDirectory string
	MaxBytes               int64
	Encrypt                bool
}
type envelope struct {
	Version  int    `json:"version"`
	AAD      []byte `json:"aad"`
	Keyset   []byte `json:"encrypted_keyset,omitempty"`
	Metadata []byte `json:"metadata"`
}
type binding struct {
	Format       string `json:"format"`
	Compression  string `json:"compression"`
	Cipher       string `json:"cipher"`
	KeyID        string `json:"key_id,omitempty"`
	ManifestHash string `json:"manifest_sha256"`
	KeysetHash   string `json:"keyset_sha256"`
}
type metadata struct {
	Manifest []byte `json:"manifest"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
