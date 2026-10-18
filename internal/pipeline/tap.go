package pipeline

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"sync/atomic"
)

// CountTap counts bytes accepted by its downstream writer without copying them.
// Its zero value is ready for one Stage construction.
type CountTap struct {
	next   io.Writer
	bytes  atomic.Int64
	closed atomic.Bool
}

// Stage attaches this tap to next without taking ownership of next.
func (t *CountTap) Stage(next io.Writer) (io.WriteCloser, error) {
	if t.next != nil {
		return nil, errors.New("count tap already attached")
	}
	t.next = next
	return t, nil
}

// Write forwards p and counts only bytes accepted downstream. Calls are serial.
func (t *CountTap) Write(p []byte) (int, error) {
	if t.closed.Load() || t.next == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := t.next.Write(p)
	t.bytes.Add(int64(n))
	return n, err
}

// Close finishes the tap without closing its downstream writer.
func (t *CountTap) Close() error {
	t.closed.Store(true)
	return nil
}

// Bytes returns the number of accepted bytes and may be read while the single writer runs.
func (t *CountTap) Bytes() int64 { return t.bytes.Load() }

// SHA256Tap hashes bytes accepted by its downstream writer.
// Its zero value is ready for one Stage construction. Write is serial;
// hash.Hash corrupts the digest if calls overlap.
type SHA256Tap struct {
	next   io.Writer
	hash   hash.Hash
	sum    [sha256.Size]byte
	closed bool
}

// Stage attaches this tap to next without taking ownership of next.
func (t *SHA256Tap) Stage(next io.Writer) (io.WriteCloser, error) {
	if t.next != nil {
		return nil, errors.New("SHA-256 tap already attached")
	}
	t.next = next
	t.hash = sha256.New()
	return t, nil
}

// Write forwards p and hashes only bytes accepted downstream. Calls are serial.
func (t *SHA256Tap) Write(p []byte) (int, error) {
	if t.closed || t.next == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := t.next.Write(p)
	_, hashErr := t.hash.Write(p[:n])
	return n, errors.Join(err, hashErr)
}

// Close finalizes the digest without closing the downstream writer.
func (t *SHA256Tap) Close() error {
	if !t.closed && t.hash != nil {
		t.hash.Sum(t.sum[:0])
	}
	t.closed = true
	return nil
}

// Sum returns the SHA-256 digest finalized by Close.
func (t *SHA256Tap) Sum() [sha256.Size]byte { return t.sum }
