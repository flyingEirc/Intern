// Package artifact stores bounded streams using zstd and Tink AES-128-GCM.
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

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

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
