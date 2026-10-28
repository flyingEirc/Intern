package artifact

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/flyingEirc/Intern/internal/pipeline"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

type streamSource struct{ payload []byte }

func (s streamSource) Run(ctx context.Context, w io.Writer) error {
	_, err := io.Copy(w, &contextReader{ctx: ctx, r: bytes.NewReader(s.payload)})
	return errors.Join(err, ctx.Err())
}

func publishStream(t *testing.T, payload, manifest []byte, opts Options, payloadName, envelopeName string) (string, *Stream) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	repo, err := local.Open(root)
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, repo.Close()) })
	staging, err := repo.Begin("artifact")
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, staging.Abort()) })
	sink, err := staging.Create(payloadName)
	requireNoError(t, err)
	stream, err := NewStream(opts, manifest)
	requireNoError(t, err)
	if _, err = stream.Envelope(); err == nil {
		t.Fatal("envelope available before stage completion")
	}
	_, err = pipeline.Run(context.Background(), streamSource{payload}, stream.Stages(), sink)
	requireNoError(t, err)
	encoded, err := stream.Envelope()
	requireNoError(t, err)
	again, err := stream.Envelope()
	requireNoError(t, err)
	if !bytes.Equal(encoded, again) {
		t.Fatal("completed envelope changed")
	}
	sink, err = staging.Create(envelopeName)
	requireNoError(t, err)
	_, err = pipeline.Run(context.Background(), streamSource{encoded}, nil, sink)
	requireNoError(t, err)
	requireNoError(t, staging.Publish(context.Background()))
	return filepath.Join(root, "artifact"), stream
}

func TestStreamV1Compatibility(t *testing.T) {
	key := testKey(t)
	for _, compress := range []bool{false, true} {
		for _, encrypt := range []bool{false, true} {
			for _, size := range []int{0, 2<<20 + 37} {
				t.Run(fmt.Sprintf("compress=%t/encrypt=%t/size=%d", compress, encrypt, size), func(t *testing.T) {
					checkStreamCompatibility(t, testPayload(size, 71), Options{Compress: compress, Encrypt: encrypt, KeyFile: key})
				})
			}
		}
	}
}

func checkStreamCompatibility(t *testing.T, payload []byte, opts Options) {
	t.Helper()
	manifest := []byte(`{"source_id":"source","backup_id":"backup"}`)
	dir, stream := publishStream(t, payload, manifest, opts, "payload.bin", "envelope.json")
	readOpts := ReadOptions{KeyFile: opts.KeyFile, WorkDirectory: t.TempDir(), MaxBytes: int64(len(payload)) + 1, Encrypt: opts.Encrypt}
	var out bytes.Buffer
	got, err := Read(context.Background(), dir, &out, readOpts)
	requireNoError(t, err)
	if !bytes.Equal(got, manifest) || !bytes.Equal(out.Bytes(), payload) {
		t.Fatal("Stream artifact is not compatible with Read")
	}
	ciphertext, err := os.ReadFile(filepath.Join(dir, "payload.bin"))
	requireNoError(t, err)
	if stream.Size() != int64(len(ciphertext)) || stream.SHA256() != digest(ciphertext) {
		t.Fatal("stream statistics do not describe ciphertext")
	}
	checkStreamEnvelope(t, dir, stream, payload, manifest, opts)
	envelopePath, payloadPath := filepath.Join(dir, "metadata.envelope.json"), filepath.Join(dir, "metadata.bin")
	requireNoError(t, os.Rename(filepath.Join(dir, "envelope.json"), envelopePath))
	requireNoError(t, os.Rename(filepath.Join(dir, "payload.bin"), payloadPath))
	out.Reset()
	got, err = ReadFiles(context.Background(), envelopePath, payloadPath, &out, readOpts)
	requireNoError(t, err)
	if !bytes.Equal(got, manifest) || !bytes.Equal(out.Bytes(), payload) {
		t.Fatal("renamed artifact members did not round-trip")
	}
}

func checkStreamEnvelope(t *testing.T, dir string, stream *Stream, payload, manifest []byte, opts Options) {
	t.Helper()
	legacyDir := t.TempDir()
	requireNoError(t, Write(context.Background(), legacyDir, bytes.NewReader(payload), manifest, opts))
	env, b, err := readEnvelope(filepath.Join(dir, "envelope.json"), opts.Encrypt)
	requireNoError(t, err)
	legacy, legacyBinding, err := readEnvelope(filepath.Join(legacyDir, "envelope.json"), opts.Encrypt)
	requireNoError(t, err)
	checkV1Binding(t, b, env, manifest, opts)
	if b.KeyID != stream.KeyID() || (opts.Encrypt && bytes.Equal(env.Keyset, legacy.Keyset)) {
		t.Fatal("stream key identity or fresh data keyset differs from contract")
	}
	// Fresh data keys change the encrypted-keyset digest, not the v1 schema.
	legacyBinding.KeysetHash = b.KeysetHash
	if b != legacyBinding {
		t.Fatal("Stream and Write AAD structures differ")
	}
	meta, _, err := openMetadata(env, b, opts.KeyFile)
	requireNoError(t, err)
	if !bytes.Equal(meta.Manifest, manifest) || meta.Size != stream.Size() || meta.SHA256 != stream.SHA256() {
		t.Fatal("sealed metadata differs from stream statistics")
	}
	if !opts.Encrypt {
		encoded, err := json.Marshal(env)
		requireNoError(t, err)
		original, err := json.Marshal(legacy)
		requireNoError(t, err)
		if !bytes.Equal(encoded, original) {
			t.Fatal("deterministic v1 envelope bytes differ from Write")
		}
	}
}

func checkV1Binding(t *testing.T, got binding, env envelope, manifest []byte, opts Options) {
	t.Helper()
	want := binding{Format: "intern-artifact-v1", Compression: "none", Cipher: "none", ManifestHash: digest(manifest), KeysetHash: digest(env.Keyset)}
	if opts.Compress {
		want.Compression = "zstd"
	}
	if opts.Encrypt {
		kek, err := loadKEK(opts.KeyFile)
		requireNoError(t, err)
		want.Cipher = streamingCipher
		want.KeyID = fmt.Sprintf("%08x", kek.KeysetInfo().PrimaryKeyId)
	} else if len(env.Keyset) != 0 {
		t.Fatal("plaintext envelope contains a keyset")
	}
	if got != want {
		t.Fatalf("v1 AAD changed: got=%+v want=%+v", got, want)
	}
}

func TestStreamAuthenticationBeforeOutput(t *testing.T) {
	key := testKey(t)
	dir, _ := publishStream(t, testPayload(3<<20+37, 9), []byte("stream identity"), Options{Compress: true, Encrypt: true, KeyFile: key}, "metadata.bin", "metadata.envelope.json")
	envelopePath, payloadPath := filepath.Join(dir, "metadata.envelope.json"), filepath.Join(dir, "metadata.bin")
	original, err := os.ReadFile(envelopePath)
	requireNoError(t, err)
	ciphertext, err := os.ReadFile(payloadPath)
	requireNoError(t, err)
	for _, kind := range []string{"bit-flip", "missing-final-byte", "segment-boundary", "appended-byte"} {
		t.Run(kind, func(t *testing.T) {
			altered := tamperCiphertext(ciphertext, kind)
			// Re-sign the outer digest so this reaches Tink authentication, not
			// only the length/hash rejection before decryption.
			requireNoError(t, os.WriteFile(envelopePath, resignOuterDigest(t, key, original, altered), 0o600))
			requireNoError(t, os.WriteFile(payloadPath, altered, 0o600))
			var out bytes.Buffer
			_, err := ReadFiles(context.Background(), envelopePath, payloadPath, &out, ReadOptions{KeyFile: key, WorkDirectory: t.TempDir(), MaxBytes: 4 << 20, Encrypt: true})
			if err == nil || out.Len() != 0 {
				t.Fatalf("tampering released plaintext: err=%v bytes=%d", err, out.Len())
			}
		})
	}
}

func TestStreamStageOwnershipAndSingleUse(t *testing.T) {
	stream, err := NewStream(Options{Compress: true, Encrypt: true, KeyFile: testKey(t)}, []byte("identity"))
	requireNoError(t, err)
	for i, stage := range stream.Stages() {
		file, err := os.CreateTemp(t.TempDir(), "downstream")
		requireNoError(t, err)
		t.Cleanup(func() { file.Close() })
		writer, err := stage(file)
		requireNoError(t, err)
		_, err = writer.Write([]byte("payload"))
		requireNoError(t, err)
		requireNoError(t, writer.Close())
		requireNoError(t, writer.Close())
		if _, err = file.Write([]byte("still open")); err != nil {
			t.Fatalf("stage %d closed its downstream: %v", i, err)
		}
		if _, err = writer.Write([]byte("closed")); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("stage %d accepted a write after close: %v", i, err)
		}
		if _, err = stream.Stages()[i](io.Discard); err == nil {
			t.Fatalf("stage %d was reused", i)
		}
		if i < len(stream.Stages())-1 {
			if _, err = stream.Envelope(); err == nil {
				t.Fatal("envelope available before all stages closed")
			}
		}
	}
}

func TestStreamRejectsWriteAndFlushFailures(t *testing.T) {
	key := testKey(t)
	for _, opts := range []Options{{}, {Compress: true}, {Encrypt: true, KeyFile: key}, {Compress: true, Encrypt: true, KeyFile: key}} {
		t.Run(fmt.Sprintf("compress=%t/encrypt=%t", opts.Compress, opts.Encrypt), func(t *testing.T) {
			stream, err := NewStream(opts, []byte("identity"))
			requireNoError(t, err)
			file, err := os.CreateTemp(t.TempDir(), "closed")
			requireNoError(t, err)
			requireNoError(t, file.Close())
			if err = stream.encode(context.Background(), file, bytes.NewReader([]byte("buffered until close"))); err == nil {
				t.Fatal("closed downstream file accepted")
			}
			if _, err = stream.Envelope(); err == nil {
				t.Fatal("failed stream produced an envelope")
			}
		})
	}
}

func TestStreamManifestBoundAndSnapshot(t *testing.T) {
	if _, err := NewStream(Options{}, make([]byte, 4<<20+1)); err == nil {
		t.Fatal("oversized manifest accepted")
	}
	manifest := bytes.Repeat([]byte("x"), 4<<20)
	stream, err := NewStream(Options{}, manifest)
	requireNoError(t, err)
	manifest[0] = 'y'
	requireNoError(t, stream.encode(context.Background(), io.Discard, bytes.NewReader(nil)))
	encoded, err := stream.Envelope()
	requireNoError(t, err)
	var env envelope
	requireNoError(t, json.Unmarshal(encoded, &env))
	b, err := readBinding(env.AAD, false)
	requireNoError(t, err)
	meta, _, err := openMetadata(env, b, "")
	requireNoError(t, err)
	if len(meta.Manifest) != 4<<20 || meta.Manifest[0] != 'x' {
		t.Fatal("manifest was not snapshotted before streaming")
	}
}

func TestStreamPreservesV1CompressedBytes(t *testing.T) {
	for _, size := range []int{0, 1, 128 << 10, 2<<20 + 37} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			payload := testPayload(size, 31)
			var want bytes.Buffer
			encoder, err := zstd.NewWriter(&want, zstd.WithEncoderConcurrency(1))
			requireNoError(t, err)
			// This is the original v1 compression path, including its optimized
			// ReaderFrom selection, independent of Stream and Write.
			_, err = io.Copy(encoder, &contextReader{ctx: context.Background(), r: bytes.NewReader(payload)})
			requireNoError(t, err)
			requireNoError(t, encoder.Close())
			dir, _ := publishStream(t, payload, []byte("v1"), Options{Compress: true}, "payload.bin", "envelope.json")
			got, err := os.ReadFile(filepath.Join(dir, "payload.bin"))
			requireNoError(t, err)
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatal("stream changed original v1 compressed bytes")
			}
		})
	}
}
