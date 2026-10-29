package ftp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	client "github.com/jlaffaye/ftp"
)

// Repository owns all sessions and active temporary objects. Publication is
// serialized locally; FTP has no atomic NOREPLACE. Use one deployment/writer,
// not multiple independent writers sharing this namespace.
type Repository struct {
	cfg         Config
	tlsConfig   *tls.Config
	dialContext func(context.Context, string, string) (net.Conn, error)
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	sinks       map[string]*Sink
	parts       map[string]bool
	active      sync.WaitGroup
	publication sync.Mutex
	closeOnce   sync.Once
	closeErr    error
}

// Open verifies or exclusively creates the exact instance ownership marker.
// Existing unmarked directories are never adopted, even when empty.
func Open(ctx context.Context, cfg Config) (*Repository, error) {
	cfg, transport, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	root, cancel := context.WithCancel(context.Background())
	r := &Repository{
		cfg: cfg, tlsConfig: transport, ctx: root, cancel: cancel,
		dialContext: (&net.Dialer{Timeout: cfg.DialTimeout}).DialContext,
		sinks:       make(map[string]*Sink), parts: make(map[string]bool),
	}
	s, err := r.connect(ctx)
	if err == nil {
		err = r.initialize(s)
		err = s.result(err)
		s.finish()
	}
	if err != nil {
		cancel()
		return nil, failure("open_repository", err)
	}
	return r, nil
}

// LIST arguments may be interpreted as glob patterns. CWD takes the absolute
// literal directory; listing its contents without an argument preserves names.
func listDirectory(c *client.ServerConn, name string) ([]*client.Entry, error) {
	if err := c.ChangeDir(name); err != nil {
		return nil, err
	}
	return c.List("")
}

// A listing can omit an existing object (for example at a server entry cap).
// Literal probes may veto absence, but never invent an entry type or authorize
// reading/deleting an object whose exact name the listing did not establish.
func lookup(c *client.ServerConn, name string) (*client.Entry, error) {
	entries, err := listDirectory(c, path.Dir(name))
	if err != nil {
		return nil, err
	}
	var found *client.Entry
	base := path.Base(name)
	for _, entry := range entries {
		if entry.Name == base {
			if found != nil {
				return nil, fs.ErrInvalid
			}
			found = entry
		}
	}
	if found == nil {
		return nil, probeUnlisted(c, name)
	}
	return found, nil
}

func probeUnlisted(c *client.ServerConn, name string) error {
	_, err := c.FileSize(name)
	if err == nil {
		return fs.ErrExist
	}
	if !unavailableReply(err) {
		return err
	}
	if err = c.ChangeDir(name); err == nil {
		return fs.ErrExist
	}
	if !unavailableReply(err) {
		return err
	}
	return nil
}

func unavailableReply(err error) bool {
	var reply *textproto.Error
	return errors.As(err, &reply) && reply.Code == client.StatusFileUnavailable
}

func directory(c *client.ServerConn, name string) error {
	if name == "/" {
		_, err := listDirectory(c, name)
		return err
	}
	current := "/"
	for part := range strings.SplitSeq(strings.TrimPrefix(name, "/"), "/") {
		current = path.Join(current, part)
		entry, err := lookup(c, current)
		if err != nil {
			return err
		}
		if entry == nil {
			return fs.ErrNotExist
		}
		if entry.Type != client.EntryTypeFolder {
			return ErrOwnership
		}
	}
	return nil
}

func (r *Repository) initialize(s *session) error {
	if err := directory(s.conn, r.cfg.BasePath); err != nil {
		return err
	}
	namespace := r.absolute(r.cfg.InstanceID)
	entry, err := lookup(s.conn, namespace)
	if err != nil {
		return err
	}
	if entry != nil {
		return r.owner(s)
	}
	// A failed MKD is never interpreted as permission to adopt a racing creator.
	if err = s.conn.MakeDir(namespace); err != nil {
		return errors.Join(ErrOwnership, err)
	}
	part := temporaryKey(r.cfg.InstanceID)
	err = r.writeMarker(s, part, namespace)
	if err != nil {
		// Never remove the namespace, marker, or any preexisting object. A
		// disconnected initialization may need operator ownership reconciliation.
		cleanupErr := s.conn.Delete(r.absolute(part))
		if cleanupErr != nil {
			return errors.Join(err, &CleanupError{Path: part, cause: failure("cleanup_marker", cleanupErr)})
		}
		return err
	}
	return r.owner(s)
}

func (r *Repository) writeMarker(s *session, part, namespace string) error {
	if err := s.conn.Stor(r.absolute(part), strings.NewReader(r.cfg.InstanceID)); err != nil {
		return err
	}
	size, err := s.conn.FileSize(r.absolute(part))
	if err != nil {
		return err
	}
	if size != int64(len(r.cfg.InstanceID)) {
		return io.ErrShortWrite
	}
	entry, err := lookup(s.conn, path.Join(namespace, markerName))
	if err != nil {
		return err
	}
	if entry != nil {
		return ErrOwnership
	}
	return s.conn.Rename(r.absolute(part), path.Join(namespace, markerName))
}

func (r *Repository) owner(s *session) error {
	if err := directory(s.conn, r.absolute(r.cfg.InstanceID)); err != nil {
		return errors.Join(ErrOwnership, err)
	}
	marker := path.Join(r.absolute(r.cfg.InstanceID), markerName)
	entry, err := lookup(s.conn, marker)
	if err != nil {
		return err
	}
	if entry == nil || entry.Type != client.EntryTypeFile || entry.Size != uint64(len(r.cfg.InstanceID)) {
		return ErrOwnership
	}
	response, err := s.conn.Retr(marker)
	if err != nil {
		return err
	}
	content, readErr := io.ReadAll(io.LimitReader(response, int64(len(r.cfg.InstanceID)+1)))
	closeErr := response.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if !bytes.Equal(content, []byte(r.cfg.InstanceID)) {
		return ErrOwnership
	}
	return nil
}

func (r *Repository) operation(ctx context.Context, op string, action func(*session) error) error {
	s, err := r.connect(ctx)
	if err != nil {
		return failure(op, err)
	}
	defer s.finish()
	if err = r.owner(s); err == nil {
		err = action(s)
	}
	return failure(op, s.result(err))
}

func (r *Repository) object(s *session, key string) (*client.Entry, error) {
	if err := directory(s.conn, path.Dir(r.absolute(key))); err != nil {
		return nil, err
	}
	entry, err := lookup(s.conn, r.absolute(key))
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fs.ErrNotExist
	}
	if entry.Type != client.EntryTypeFile {
		return nil, fs.ErrInvalid
	}
	return entry, nil
}

// Size reports byte length, not cryptographic authentication.
func (r *Repository) Size(ctx context.Context, key string) (int64, error) {
	if err := r.key(key, false, false); err != nil {
		return 0, failure("size", err)
	}
	var size int64
	err := r.operation(ctx, "size", func(s *session) error {
		if _, err := r.object(s, key); err != nil {
			return err
		}
		var err error
		size, err = s.conn.FileSize(r.absolute(key))
		return err
	})
	return size, err
}

// Delete removes one published regular object, never a directory or part.
func (r *Repository) Delete(ctx context.Context, key string) error {
	if err := r.key(key, false, false); err != nil {
		return failure("delete", err)
	}
	return r.operation(ctx, "delete", func(s *session) error {
		if _, err := r.object(s, key); err != nil {
			return err
		}
		return s.conn.Delete(r.absolute(key))
	})
}

// List returns sorted published regular keys below one component-aligned
// prefix. Empty prefix means the instance root, not the whole FTP account.
func (r *Repository) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix == "" {
		prefix = r.cfg.InstanceID
	}
	if err := r.key(prefix, true, false); err != nil {
		return nil, failure("list", err)
	}
	keys := make([]string, 0)
	err := r.operation(ctx, "list", func(s *session) error {
		if err := directory(s.conn, path.Dir(r.absolute(prefix))); err != nil {
			return err
		}
		entry, err := lookup(s.conn, r.absolute(prefix))
		if err != nil {
			return err
		}
		if entry == nil {
			return fs.ErrNotExist
		}
		if entry.Type == client.EntryTypeFile {
			keys = append(keys, prefix)
			return nil
		}
		if entry.Type != client.EntryTypeFolder {
			return fs.ErrInvalid
		}
		var walkErr error
		keys, walkErr = r.walk(s, prefix)
		return walkErr
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

func (r *Repository) walk(s *session, prefix string) ([]string, error) {
	keys := make([]string, 0)
	pending := []string{prefix}
	for len(pending) != 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := listDirectory(s.conn, r.absolute(dir))
		if err != nil {
			return nil, err
		}
		for _, child := range entries {
			if !component(child.Name) || reserved(child.Name) {
				continue
			}
			key := path.Join(dir, child.Name)
			switch child.Type {
			case client.EntryTypeFile:
				keys = append(keys, key)
			case client.EntryTypeFolder:
				pending = append(pending, key)
			case client.EntryTypeLink:
				// Do not follow links, including directory links.
			}
		}
	}
	return keys, nil
}

// CleanupPart retries a caller-persisted temporary key. It is bounded even if
// the caller forgot to set a deadline, and rejects all currently active parts.
func (r *Repository) CleanupPart(ctx context.Context, key string) error {
	if err := r.key(key, false, true); err != nil {
		return failure("cleanup", err)
	}
	r.mu.Lock()
	if r.parts[key] {
		r.mu.Unlock()
		return failure("cleanup", fs.ErrExist)
	}
	r.parts[key] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.parts, key)
		r.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := r.operation(ctx, "cleanup", func(s *session) error {
		_, err := r.object(s, key)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.conn.Delete(r.absolute(key))
	})
	if err != nil {
		return &CleanupError{Path: key, cause: err}
	}
	return nil
}

// Probe checks actual temporary write, SIZE and delete privileges without
// publishing an object. Cleanup failures carry the same durable handoff key.
func (r *Repository) Probe(ctx context.Context) error {
	part := temporaryKey(r.cfg.InstanceID)
	r.mu.Lock()
	r.parts[part] = true
	r.mu.Unlock()
	err := r.operation(ctx, "probe", func(s *session) error {
		if err := s.conn.Stor(r.absolute(part), strings.NewReader("intern FTP probe")); err != nil {
			return err
		}
		size, err := s.conn.FileSize(r.absolute(part))
		if err == nil && size != int64(len("intern FTP probe")) {
			return io.ErrShortWrite
		}
		return err
	})
	r.mu.Lock()
	delete(r.parts, part)
	r.mu.Unlock()
	return errors.Join(err, r.CleanupPart(context.Background(), part))
}

// Close cancels and drains every owned session. Unremoved started parts are
// returned as CleanupErrors for retry through a reopened repository.
func (r *Repository) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		sinks := make([]*Sink, 0, len(r.sinks))
		for _, sink := range r.sinks {
			sinks = append(sinks, sink)
		}
		r.mu.Unlock()
		r.cancel()
		for _, sink := range sinks {
			r.closeErr = errors.Join(r.closeErr, sink.Abort())
		}
		r.active.Wait()
	})
	return r.closeErr
}
