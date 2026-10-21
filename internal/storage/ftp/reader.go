package ftp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"

	client "github.com/jlaffaye/ftp"
)

type objectReader struct {
	session    *session
	response   *client.Response
	readMu     sync.Mutex
	closeMu    sync.Mutex
	closed     atomic.Bool
	closeErr   error
	stopCancel func() bool
}

// Open resumes reads of one immutable object at an absolute byte offset.
// The caller must consume and Close the reader and check both error results.
func (r *Repository) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if err := r.key(key, false, false); err != nil {
		return nil, failure("open", err)
	}
	if offset < 0 {
		return nil, failure("open", fs.ErrInvalid)
	}
	s, err := r.connect(ctx)
	if err != nil {
		return nil, failure("open", err)
	}
	if err = r.owner(s); err == nil {
		_, err = r.object(s, key)
	}
	var response *client.Response
	if err == nil {
		response, err = s.conn.RetrFrom(r.absolute(key), uint64(offset))
	}
	if err != nil {
		err = failure("open", s.result(err))
		s.finish()
		return nil, err
	}
	reader := &objectReader{session: s, response: response}
	reader.closeMu.Lock()
	reader.stopCancel = context.AfterFunc(s.ctx, reader.close)
	reader.closeMu.Unlock()
	return reader, nil
}

func (r *objectReader) Read(p []byte) (int, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if r.closed.Load() {
		return 0, failure("read", errors.Join(fs.ErrClosed, r.session.ctx.Err()))
	}
	n, err := r.response.Read(p)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, failure("read", r.session.result(err))
}

func (r *objectReader) Close() error {
	r.close()
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	return r.closeErr
}

func (r *objectReader) close() {
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed.Swap(true) {
		return
	}
	r.stopCancel()
	// Response.Read only accesses the net.Conn. Closing that connection can
	// safely unblock an in-flight Read without concurrent SDK control calls.
	r.closeErr = failure("close_reader", r.session.result(r.response.Close()))
	r.session.finish()
}
