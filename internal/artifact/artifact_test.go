package artifact

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testKey(t *testing.T) string {
	t.Helper()
	key := filepath.Join(t.TempDir(), "key")
	requireNoError(t, GenerateKey(key))
	return key
}

func testPayload(size int, seed byte) []byte {
	data := make([]byte, size)
	rand.NewChaCha8([32]byte{seed}).Read(data)
	return data
}

func TestArtifactRoundTripAndExpandedLimit(t *testing.T) {
	key := testKey(t)
	payload := testPayload(2<<20+37, 42)
	for _, compress := range []bool{false, true} {
		for _, encrypt := range []bool{false, true} {
			t.Run(fmt.Sprintf("compress=%t/encrypt=%t", compress, encrypt), func(t *testing.T) { checkRoundTrip(t, key, payload, compress, encrypt) })
		}
	}
}

func checkRoundTrip(t *testing.T, key string, payload []byte, compress, encrypt bool) {
	t.Helper()
	dir := t.TempDir()
	manifest := []byte(`{"project":"p","id":"a","role":"payload"}`)
	requireNoError(t, Write(context.Background(), dir, bytes.NewReader(payload), manifest, Options{Compress: compress, Encrypt: encrypt, KeyFile: key}))
	var out bytes.Buffer
	opts := ReadOptions{KeyFile: key, WorkDirectory: t.TempDir(), MaxBytes: int64(len(payload)), Encrypt: encrypt}
	got, err := Read(context.Background(), dir, &out, opts)
	requireNoError(t, err)
	if !bytes.Equal(payload, out.Bytes()) || !bytes.Equal(manifest, got) {
		t.Fatal("recovered payload or metadata differs")
	}
	opts.MaxBytes--
	if _, err = Read(context.Background(), dir, io.Discard, opts); err == nil {
		t.Fatal("expanded size limit ignored")
	}
}

func TestEmptyAuthenticatedStream(t *testing.T) {
	key := testKey(t)
	dir := t.TempDir()
	requireNoError(t, Write(context.Background(), dir, bytes.NewReader(nil), []byte("empty"), Options{Compress: true, Encrypt: true, KeyFile: key}))
	var out bytes.Buffer
	_, err := Read(context.Background(), dir, &out, ReadOptions{KeyFile: key, WorkDirectory: t.TempDir(), MaxBytes: 1, Encrypt: true})
	requireNoError(t, err)
	if out.Len() != 0 {
		t.Fatal("non-empty recovered empty stream")
	}
}

func assertRejectedBeforeOutput(t *testing.T, dir, key string) {
	t.Helper()
	var out bytes.Buffer
	if _, err := Read(context.Background(), dir, &out, ReadOptions{KeyFile: key, WorkDirectory: t.TempDir(), MaxBytes: 8 << 20, Encrypt: true}); err == nil {
		t.Fatal("corrupt stream accepted")
	}
	if out.Len() != 0 {
		t.Fatal("unauthenticated plaintext reached output")
	}
}

func TestAuthenticationBeforeOutput(t *testing.T) {
	key := testKey(t)
	dir := t.TempDir()
	requireNoError(t, Write(context.Background(), dir, bytes.NewReader(testPayload(3<<20+37, 9)), []byte("immutable identity"), Options{Encrypt: true, KeyFile: key}))
	assertRejectedBeforeOutput(t, dir, testKey(t))
	originalEnv, err := os.ReadFile(filepath.Join(dir, "envelope.json"))
	requireNoError(t, err)
	ciphertext, err := os.ReadFile(filepath.Join(dir, "payload.bin"))
	requireNoError(t, err)
	for _, kind := range []string{"bit-flip", "missing-final-byte", "segment-boundary", "appended-byte"} {
		t.Run(kind, func(t *testing.T) {
			altered := tamperCiphertext(ciphertext, kind)
			encoded := resignOuterDigest(t, key, originalEnv, altered)
			requireNoError(t, os.WriteFile(filepath.Join(dir, "envelope.json"), encoded, 0o600))
			requireNoError(t, os.WriteFile(filepath.Join(dir, "payload.bin"), altered, 0o600))
			assertRejectedBeforeOutput(t, dir, key)
		})
	}
}

func tamperCiphertext(ciphertext []byte, kind string) []byte {
	altered := bytes.Clone(ciphertext)
	switch kind {
	case "bit-flip":
		altered[len(altered)/2] ^= 1
	case "missing-final-byte":
		altered = altered[:len(altered)-1]
	case "segment-boundary":
		altered = altered[:1<<20]
	case "appended-byte":
		altered = append(altered, 0)
	}
	return altered
}

func resignOuterDigest(t *testing.T, key string, original, altered []byte) []byte {
	t.Helper()
	// Re-authenticate the outer digest to exercise Tink's full-stream authentication.
	master, err := masterKey(key)
	requireNoError(t, err)
	var env envelope
	requireNoError(t, json.Unmarshal(original, &env))
	plain, err := master.Decrypt(env.Metadata, env.AAD)
	requireNoError(t, err)
	var meta metadata
	requireNoError(t, json.Unmarshal(plain, &meta))
	meta.Size = int64(len(altered))
	meta.SHA256 = digest(altered)
	plain, err = json.Marshal(meta)
	requireNoError(t, err)
	env.Metadata, err = master.Encrypt(plain, env.AAD)
	requireNoError(t, err)
	encoded, err := json.Marshal(env)
	requireNoError(t, err)
	return encoded
}
