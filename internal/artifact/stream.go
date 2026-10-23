package artifact

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"

	"github.com/flyingEirc/Intern/internal/pipeline"
)

// Stream produces one v1 artifact through single-use pipeline stages.
// Writes and lifecycle methods must be serial. Call Envelope only after a
// successful pipeline run; stage completion alone does not commit the sink.
// The caller must keep the KEK separate from its payload and work directories.
type Stream struct {
	seal     *sealer
	manifest []byte
	stages   []pipeline.Stage
	count    pipeline.CountTap
	hash     pipeline.SHA256Tap
	closed   int
	err      error
}

// NewStream binds an immutable manifest of at most 4 MiB before any payload is
// written. Encryption loads the KEK and creates a fresh wrapped data keyset.
func NewStream(opts Options, manifest []byte) (*Stream, error) {
	if len(manifest) > 4<<20 {
		return nil, errors.New("manifest exceeds 4 MiB")
	}
	seal, err := newSealer(opts, manifest)
	if err != nil {
		return nil, err
	}
	s := &Stream{seal: seal, manifest: bytes.Clone(manifest), stages: make([]pipeline.Stage, 0, 4)}
	if opts.Compress {
		s.addStage(func(next io.Writer) (io.WriteCloser, error) {
			return zstd.NewWriter(next, zstd.WithEncoderConcurrency(1))
		})
	}
	if opts.Encrypt {
		s.addStage(func(next io.Writer) (io.WriteCloser, error) {
			return seal.stream.NewEncryptingWriter(next, seal.env.AAD)
		})
	}
	s.addStage(s.count.Stage)
	s.addStage(s.hash.Stage)
	return s, nil
}

// Stages returns upstream-first compression, encryption, and ciphertext taps.
// Each returned stage can be constructed only once, even across repeated calls
// to Stages. Closing a stage never closes its downstream writer.
func (s *Stream) Stages() []pipeline.Stage { return slices.Clone(s.stages) }

// Envelope returns the v1 envelope only after every stage has closed without
// a construction, write, or close failure. It does not establish sink durability.
func (s *Stream) Envelope() ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.closed != len(s.stages) {
		return nil, errors.New("artifact stream stages are not closed")
	}
	if s.seal.env.Metadata == nil {
		if err := s.seal.sealMetadata(metadata{Manifest: s.manifest, Size: s.Size(), SHA256: s.SHA256()}); err != nil {
			return nil, err
		}
	}
	return json.Marshal(s.seal.env)
}

// Size returns the ciphertext byte count after the stages have closed.
func (s *Stream) Size() int64 { return s.count.Bytes() }

// SHA256 returns the ciphertext digest after the stages have closed.
func (s *Stream) SHA256() string {
	sum := s.hash.Sum()
	return hex.EncodeToString(sum[:])
}

// KeyID returns the KEK identifier, or an empty string for plaintext artifacts.
func (s *Stream) KeyID() string { return s.seal.binding.KeyID }

func (s *Stream) addStage(factory pipeline.Stage) {
	used := false
	s.stages = append(s.stages, func(next io.Writer) (io.WriteCloser, error) {
		if used {
			return nil, errors.New("artifact stream stage already attached")
		}
		used = true
		// Hide Close even when the caller passes a WriteCloser directly. A codec
		// may otherwise take ownership of the downstream writer via assertion.
		writer, err := factory(struct{ io.Writer }{next})
		if err != nil {
			s.failed(err)
			return nil, err
		}
		return &streamStage{stream: s, writer: writer}, nil
	})
}

func (s *Stream) failed(err error) {
	if s.err == nil {
		s.err = err
	}
}

type streamStage struct {
	stream   *Stream
	writer   io.WriteCloser
	closed   bool
	closeErr error
}

func (w *streamStage) Write(p []byte) (int, error) {
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := w.writer.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	w.stream.failed(err)
	return n, err
}

func (w *streamStage) Close() error {
	if !w.closed {
		w.closed = true
		w.closeErr = w.writer.Close()
		w.stream.failed(w.closeErr)
		w.stream.closed++
	}
	return w.closeErr
}

func (w *streamStage) ReadFrom(r io.Reader) (int64, error) {
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	if readerFrom, ok := w.writer.(io.ReaderFrom); ok {
		n, err := readerFrom.ReadFrom(r)
		w.stream.failed(err)
		return n, err
	}
	return io.Copy(struct{ io.Writer }{w}, r)
}
