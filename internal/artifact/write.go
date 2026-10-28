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

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/streamingaead"
	"github.com/tink-crypto/tink-go/v2/tink"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

type sealer struct {
	env     envelope
	binding binding
	master  tink.AEAD
	stream  tink.StreamingAEAD
}

func Write(ctx context.Context, dir string, payload io.Reader, manifest []byte, opts Options) error {
	if len(manifest) > 4<<20 {
		return errors.New("manifest exceeds 4 MiB")
	}
	if opts.Encrypt {
		if err := ValidateKeyLocation(opts.KeyFile, dir); err != nil {
			return err
		}
	}
	stream, err := NewStream(opts, manifest)
	if err != nil {
		return err
	}
	if err = stream.writePayload(ctx, dir, payload); err != nil {
		return err
	}
	encoded, err := stream.Envelope()
	if err != nil {
		return err
	}
	return saveEnvelope(dir, encoded)
}

func newSealer(opts Options, manifest []byte) (*sealer, error) {
	seal := &sealer{env: envelope{Version: 1}, binding: binding{Format: "intern-artifact-v1", Compression: "none", Cipher: "none", ManifestHash: digest(manifest), KeysetHash: digest(nil)}}
	if opts.Compress {
		seal.binding.Compression = "zstd"
	}
	if opts.Encrypt {
		if err := seal.initializeEncryption(opts.KeyFile); err != nil {
			return nil, err
		}
	}
	aad, err := json.Marshal(seal.binding)
	if err != nil {
		return nil, err
	}
	seal.env.AAD = aad
	return seal, nil
}

func (s *sealer) initializeEncryption(path string) error {
	kek, err := loadKEK(path)
	if err != nil {
		return err
	}
	s.master, err = aead.New(kek)
	if err != nil {
		return err
	}
	h, err := keyset.NewHandle(streamingaead.AES128GCMHKDF1MBKeyTemplate())
	if err != nil {
		return err
	}
	var wrapped bytes.Buffer
	if err = h.Write(keyset.NewBinaryWriter(&wrapped), s.master); err != nil {
		return err
	}
	s.env.Keyset = wrapped.Bytes()
	s.stream, err = streamingaead.New(h)
	if err != nil {
		return err
	}
	s.binding.Cipher = streamingCipher
	s.binding.KeysetHash = digest(s.env.Keyset)
	s.binding.KeyID = fmt.Sprintf("%08x", kek.KeysetInfo().PrimaryKeyId)
	return nil
}

func (s *Stream) writePayload(ctx context.Context, dir string, payload io.Reader) (retErr error) {
	f, err := fsutil.OpenFile(filepath.Join(dir, "payload.bin"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, f)
	if err = s.encode(ctx, f, payload); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Stream) encode(ctx context.Context, w io.Writer, payload io.Reader) (retErr error) {
	// Construct downstream first; deferred closes run upstream first, retaining
	// Write's original flush and file-ownership behavior on source failures.
	for i := len(s.stages) - 1; i >= 0; i-- {
		stage, err := s.stages[i](w)
		if err != nil {
			return err
		}
		defer fsutil.Close(&retErr, stage)
		w = stage
	}
	_, err := io.Copy(w, &contextReader{ctx: ctx, r: payload})
	return errors.Join(err, ctx.Err())
}

func (s *sealer) sealMetadata(meta metadata) error {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if s.master != nil {
		encoded, err = s.master.Encrypt(encoded, s.env.AAD)
		if err != nil {
			return err
		}
	}
	s.env.Metadata = encoded
	return nil
}

func saveEnvelope(dir string, encoded []byte) (retErr error) {
	f, err := fsutil.OpenFile(filepath.Join(dir, "envelope.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, f)
	_, err = f.Write(encoded)
	return errors.Join(err, f.Sync())
}
