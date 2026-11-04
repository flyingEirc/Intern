// Command s2tap is the FTPS fixture's server-side audit tap: a plain TCP proxy
// in front of one pure-ftpd listener that records whether credential material
// ever reached the server.
//
// It never stores what it forwards beyond a bounded buffer, and it never writes
// a byte it counted. Each closed session appends one JSON line holding the
// occurrence count of every needle in each direction, the total byte counts, a
// digest of each direction and the session's start and end. Counts are computed
// over the reassembled stream, so a needle that straddled two TCP segments is
// still found; the tap cannot be passed by splitting a command across writes.
//
// Streams are bounded on purpose: an FTP control session is a few kilobytes, and
// a tap that buffered an unbounded body would turn a fixture into a denial of
// service. A session that exceeded the cap is recorded with truncated=true, which
// the acceptance tests refuse to read as a zero count.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/flyingEirc/Intern/internal/storage/ftp/s2fixture"
)

// maxSessionBytes bounds the retained stream of one direction of one session.
const maxSessionBytes = 1 << 20

// needleNames are fixed, so a refusal cannot be arranged around a pattern the
// operator chose. "user" and "pass" are the two commands that carry a
// credential, "auth_tls" is the upgrade that must precede them, "greeting" is the
// server's plaintext 220 line, and "secret" and "marker" are the generated
// account password and the cleartext probe pattern, supplied by the fixture's
// environment and never by an argument, a file it logs, or the record itself.

var sequence atomic.Int64

type configuration struct {
	name    string
	listen  string
	dial    string
	audit   string
	needles map[string][]byte
}

// record is the shared audit shape, so a tap and the worker that reads it can
// never disagree about what a session said.
type record = s2fixture.Audit

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "s2tap:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := load()
	if err != nil {
		return err
	}
	census, err := cfg.openCensus()
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := census.Close(); closeErr != nil {
			fmt.Fprintln(os.Stderr, "s2tap: audit file close failed:", closeErr)
		}
	}()
	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	// The fixture entrypoint waits for this line before it reports readiness, so
	// the bound address is knowable without a port probe. The line names no needle.
	fmt.Printf("s2tap listening name=%s listen=%s dial=%s audit=%s\n",
		cfg.name, listener.Addr().String(), cfg.dial, cfg.audit)
	return cfg.accept(listener, census)
}

// openCensus prepares the one append-only file this listener's sessions land in.
// The directory is 0755 and the record 0644 because the census holds counts and
// digests only - never a byte it searched - and the unprivileged worker that
// reads it back lives in a different container, with a different uid, on a shared
// volume. A file it could not read would make every refusal claim unfalsifiable.
func (cfg configuration) openCensus() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.audit), 0o755); err != nil { //nolint:gosec //G301 public census directory
		return nil, err
	}
	return os.OpenFile(cfg.audit, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec //G302 cross-uid census read
}

// accept audits one session per connection until the listener is closed. Records
// are serialized by one mutex: an interleaved pair of JSON lines would make the
// census unreadable, and a lost line is a missing session.
func (cfg configuration) accept(listener net.Listener, census *os.File) error {
	var writing sync.Mutex
	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}
		go func() {
			entry := cfg.serve(client)
			writing.Lock()
			defer writing.Unlock()
			payload, marshalErr := json.Marshal(entry)
			if marshalErr != nil {
				fmt.Fprintln(os.Stderr, "s2tap: audit record refused name="+cfg.name+":", marshalErr)
				return
			}
			// A census line that never landed is a missing session, and a worker
			// that read a gap as "the client sent nothing" would be told the
			// loudest possible lie this tool could tell. Say so on the log.
			if _, writeErr := census.Write(append(payload, '\n')); writeErr != nil {
				fmt.Fprintln(os.Stderr, "s2tap: audit write refused name="+cfg.name+":", writeErr)
			}
		}()
	}
}

func load() (configuration, error) {
	var cfg configuration
	cfg.name = os.Getenv("S2_TAP_NAME")
	cfg.listen = os.Getenv("S2_TAP_LISTEN")
	cfg.dial = os.Getenv("S2_TAP_DIAL")
	cfg.audit = os.Getenv("S2_TAP_AUDIT")
	if cfg.name == "" || cfg.listen == "" || cfg.dial == "" || cfg.audit == "" {
		return cfg, errors.New("S2_TAP_NAME, S2_TAP_LISTEN, S2_TAP_DIAL and S2_TAP_AUDIT are required")
	}
	cfg.needles = map[string][]byte{
		"user":     []byte("USER "),
		"pass":     []byte("PASS "),
		"auth_tls": []byte("AUTH TLS"),
		"greeting": []byte("220 "),
	}
	if secret := os.Getenv("S2_TAP_SECRET"); secret != "" {
		cfg.needles["secret"] = []byte(secret)
	}
	if marker := os.Getenv("S2_TAP_MARKER"); marker != "" {
		cfg.needles["marker"] = []byte(marker)
	}
	return cfg, nil
}

// serve proxies one session and returns its audit record. It blocks until both
// directions have ended, so the record's end timestamp is the moment the server
// saw the connection close, and a test that waits for a record is waiting for a
// finished session rather than a partial one.
func (cfg configuration) serve(client net.Conn) record {
	entry := record{
		Seq:     sequence.Add(1),
		Name:    cfg.name,
		Client:  client.RemoteAddr().String(),
		Started: time.Now().UnixNano(),
	}
	upstream, err := net.Dial("tcp", cfg.dial)
	if err != nil {
		entry.Ended = time.Now().UnixNano()
		entry.Error = "upstream dial failed"
		_ = client.Close() //nolint:errcheck // the dial failure is the reported outcome
		return entry
	}
	inbound := newStream(cfg.needles)
	outbound := newStream(cfg.needles)
	var directions sync.WaitGroup
	directions.Add(2)
	// Each direction owns exactly one error variable: the census of a session is
	// assembled after both goroutines end, never written to concurrently.
	var inboundErr, outboundErr error
	go func() {
		defer directions.Done()
		// A copy failure here is the session itself: a refused FTPS handshake, or
		// a client that hung up mid-command. The record still counts what reached
		// the server, which is exactly what an acceptance case measures.
		_, inboundErr = io.Copy(&tee{stream: inbound, next: upstream}, client)
		halfClose(upstream)
		_ = client.Close() //nolint:errcheck // the session is already ending
	}()
	go func() {
		defer directions.Done()
		_, outboundErr = io.Copy(&tee{stream: outbound, next: client}, upstream)
		halfClose(client)
		_ = upstream.Close() //nolint:errcheck // the session is already ending
	}()
	directions.Wait()
	if note := copyFailure(inboundErr) + copyFailure(outboundErr); note != "" {
		entry.Error = note
	}
	entry.Ended = time.Now().UnixNano()
	entry.ToServer = inbound.counts()
	entry.FromServer = outbound.counts()
	entry.ToServerBytes = inbound.total
	entry.FromServerBytes = outbound.total
	entry.ToServerSHA = inbound.digest()
	entry.FromServerSHA = outbound.digest()
	entry.Truncated = inbound.truncated || outbound.truncated
	return entry
}

// tee counts and hashes every byte it forwards, keeping at most the bounded
// prefix of the stream for the final census.
type tee struct {
	stream *stream
	next   io.Writer
}

func (t *tee) Write(p []byte) (int, error) {
	t.stream.feed(p)
	return t.next.Write(p)
}

type stream struct {
	needles   map[string][]byte
	buffer    []byte
	total     int64
	hasher    hash.Hash
	truncated bool
}

func newStream(needles map[string][]byte) *stream {
	return &stream{needles: needles, hasher: sha256.New()}
}

func (s *stream) feed(p []byte) {
	s.total += int64(len(p))
	_, _ = s.hasher.Write(p)
	room := maxSessionBytes - len(s.buffer)
	if room <= 0 {
		s.truncated = true
		return
	}
	if len(p) > room {
		s.buffer = append(s.buffer, p[:room]...)
		s.truncated = true
		return
	}
	s.buffer = append(s.buffer, p...)
}

// counts reports the occurrence of every needle in this direction, including a
// zero for a needle the fixture never configured.
func (s *stream) counts() map[string]int {
	result := make(map[string]int, len(s.needles))
	for name, needle := range s.needles {
		result[name] = bytes.Count(s.buffer, needle)
	}
	return result
}

func (s *stream) digest() string {
	return hex.EncodeToString(s.hasher.Sum(nil))
}

// copyFailure names a copy error that is not one end simply hanging up, which is
// how a normal FTPS session finishes. The result is a fixed word, never the
// peer's text: a census line must not become a place credentials can appear.
func copyFailure(err error) string {
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed),
		errors.Is(err, io.ErrClosedPipe), errors.Is(err, syscall.ECONNRESET):
		return ""
	default:
		return "copy failed"
	}
}

// halfClose ends one direction so io.Copy returns, and falls back to a full close
// for a connection that cannot half-close, or that failed while trying.
func halfClose(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		_ = conn.Close() //nolint:errcheck // the session is already ending
		return
	}
	if err := tcp.CloseWrite(); err != nil {
		_ = conn.Close() //nolint:errcheck // the failed half-close is the diagnostic
	}
}
