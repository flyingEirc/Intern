package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/streamingaead"
	"github.com/tink-crypto/tink-go/v2/tink"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

func Read(ctx context.Context, dir string, output io.Writer, opts ReadOptions) ([]byte, error) {
	return ReadFiles(ctx, filepath.Join(dir, "envelope.json"), filepath.Join(dir, "payload.bin"), output, opts)
}

// ReadFiles authenticates and decodes a v1 artifact stored under explicit member
// paths. It enforces the same regular-file, size, and authentication checks as Read.
func ReadFiles(ctx context.Context, envelopePath, payloadPath string, output io.Writer, opts ReadOptions) (manifest []byte, retErr error) {
	if opts.MaxBytes < 1 {
		return nil, errors.New("expanded artifact exceeds restore size limit")
	}
	env, b, err := readEnvelope(envelopePath, opts.Encrypt)
	if err != nil {
		return nil, err
	}
	meta, stream, err := openMetadata(env, b, opts.KeyFile)
	if err != nil {
		return nil, err
	}
	f, err := openPayload(ctx, payloadPath, meta)
	if err != nil {
		return nil, err
	}
	defer fsutil.Close(&retErr, f)
	var input io.Reader = f
	if stream != nil {
		decrypted, authErr := authenticate(ctx, f, env.AAD, stream, opts.WorkDirectory)
		if authErr != nil {
			return nil, authErr
		}
		defer func() { fsutil.Close(&retErr, decrypted); retErr = errors.Join(retErr, os.Remove(decrypted.Name())) }()
		input = decrypted
	}
	if err = decode(ctx, output, input, b.Compression, opts.MaxBytes); err != nil {
		return nil, err
	}
	return meta.Manifest, nil
}

func readEnvelope(path string, encrypted bool) (env envelope, b binding, retErr error) {
	f, err := regularFile(path)
	if err != nil {
		return env, b, err
	}
	defer fsutil.Close(&retErr, f)
	data, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil {
		return env, b, err
	}
	if len(data) > 8<<20 {
		return env, b, errors.New("oversized artifact envelope")
	}
	if err = json.Unmarshal(data, &env); err != nil || env.Version != 1 {
		return env, b, errors.New("invalid artifact envelope")
	}
	b, err = readBinding(env.AAD, encrypted)
	return env, b, err
}

func readBinding(aad []byte, encrypted bool) (b binding, err error) {
	if err = json.Unmarshal(aad, &b); err != nil || b.Format != "intern-artifact-v1" {
		return b, errors.New("unsupported artifact format/codec")
	}
	if b.Compression != "none" && b.Compression != "zstd" {
		return b, errors.New("unsupported artifact format/codec")
	}
	if encrypted != (b.Cipher != "none") {
		return b, errors.New("artifact encryption mode differs from the published backup contract")
	}
	return b, nil
}

func openMetadata(env envelope, b binding, keyFile string) (meta metadata, stream tink.StreamingAEAD, err error) {
	encoded, stream, err := decryptMetadata(env, b.Cipher, keyFile)
	if err != nil {
		return meta, nil, err
	}
	if err = json.Unmarshal(encoded, &meta); err != nil {
		return meta, nil, errors.New("artifact metadata binding mismatch")
	}
	if meta.Size < 0 || b.ManifestHash != digest(meta.Manifest) || b.KeysetHash != digest(env.Keyset) {
		return meta, nil, errors.New("artifact metadata binding mismatch")
	}
	return meta, stream, nil
}

func decryptMetadata(env envelope, cipher, keyFile string) ([]byte, tink.StreamingAEAD, error) {
	switch cipher {
	case "none":
		if len(env.Keyset) != 0 {
			return nil, nil, errors.New("plaintext artifact unexpectedly contains a keyset")
		}
		return env.Metadata, nil, nil
	case streamingCipher:
		return unwrapMetadata(env, keyFile)
	default:
		return nil, nil, errors.New("unsupported artifact cipher")
	}
}

func unwrapMetadata(env envelope, keyFile string) ([]byte, tink.StreamingAEAD, error) {
	master, err := masterKey(keyFile)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := master.Decrypt(env.Metadata, env.AAD)
	if err != nil {
		return nil, nil, errors.New("manifest authentication failed: wrong key or modified metadata")
	}
	h, err := keyset.Read(keyset.NewBinaryReader(bytes.NewReader(env.Keyset)), master)
	if err != nil {
		return nil, nil, errors.New("cannot unwrap artifact data key")
	}
	stream, err := streamingaead.New(h)
	return encoded, stream, err
}

func openPayload(ctx context.Context, path string, meta metadata) (result *os.File, retErr error) {
	f, err := regularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			fsutil.Close(&retErr, f)
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(hash, &contextReader{ctx: ctx, r: f})
	if err != nil {
		return nil, err
	}
	if n != meta.Size || hex.EncodeToString(hash.Sum(nil)) != meta.SHA256 {
		return nil, errors.New("artifact ciphertext length/digest mismatch")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return f, nil
}

func authenticate(ctx context.Context, input io.Reader, aad []byte, stream tink.StreamingAEAD, work string) (result *os.File, retErr error) {
	f, err := os.CreateTemp(work, "intern-authenticated-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			fsutil.Close(&retErr, f)
			retErr = errors.Join(retErr, os.Remove(f.Name()))
		}
	}()
	r, err := stream.NewDecryptingReader(input, aad)
	if err != nil {
		return nil, errors.New("artifact decryption failed")
	}
	// Authenticate through EOF before giving compressed bytes to the decompressor.
	if _, err = io.Copy(f, &contextReader{ctx: ctx, r: r}); err != nil {
		return nil, errors.Join(errors.New("artifact stream authentication failed"), ctx.Err())
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return f, nil
}

func decode(ctx context.Context, output io.Writer, input io.Reader, compression string, limit int64) error {
	if compression == "zstd" {
		decoder, err := zstd.NewReader(input, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return err
		}
		defer decoder.Close()
		input = decoder
	}
	n, err := io.Copy(output, io.LimitReader(&contextReader{ctx: ctx, r: input}, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("expanded artifact exceeds %d byte restore limit", limit)
	}
	return ctx.Err()
}

func regularFile(path string) (*os.File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("artifact member must be a regular file, not a symlink")
	}
	return fsutil.Open(path)
}
