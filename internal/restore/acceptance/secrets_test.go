//go:build integration

package acceptance

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/flyingEirc/Intern/internal/restore"
)

// secretWatch is the P4-A7 census. Every string the worker is about to turn into
// evidence — an emitted line, one progress payload, one cause of a failure chain,
// one copy of the rehearsal server log — is audited against the full set of
// fixture secrets while it is still raw. Redaction on the way out is not proof:
// a leak has to be caught before anything is printed.
type secretWatch struct {
	mu      sync.Mutex
	scanned map[string]int
	leaks   []string
}

var watch = &secretWatch{scanned: map[string]int{}}

// observe audits one raw string under a fixed label and remembers a violation
// without ever storing the secret or the surrounding text.
func (s *secretWatch) observe(label, raw string) {
	index := secretIndex(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanned[label]++
	if index >= 0 {
		s.leaks = append(s.leaks, fmt.Sprintf("%s leaked census entry %d", label, index))
	}
}

func (s *secretWatch) count(label string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanned[label]
}

func (s *secretWatch) violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.leaks)
}

// censusPathLabels are the fixed surfaces this suite audits. Every one needs a
// non-zero count in a green run: an empty scan proves nothing about the surface
// it was meant to cover.
var censusPathLabels = []string{"log", "progress", "chain", "rehearsal"}

// census is every secret this fixture generated: the two passwords, the exact
// source DSN in both its plain and its URL form, the FTP credential pair, and the
// KEK key files as a whole and as their decoded key material.
func census() []string {
	var values []string
	add := func(value string) {
		if value == "" || slices.Contains(values, value) {
			return
		}
		values = append(values, value)
	}
	backupPassword := os.Getenv("INTERN_P4_PASSWORD")
	ftpPassword := os.Getenv("INTERN_P4_FTP_PASSWORD")
	add(backupPassword)
	add(ftpPassword)
	// The source connection string exactly as the driver would print it: a DSN is
	// a different byte string from the bare password it embeds.
	config := driver.NewConfig()
	config.Net, config.Addr, config.User, config.Passwd = "unix", socketPath, sourceUser, backupPassword
	add(config.FormatDSN())
	if address := os.Getenv("INTERN_P4_FTP_ADDRESS"); address != "" && ftpPassword != "" {
		add("normal:" + ftpPassword + "@" + address)
		remote := url.URL{Scheme: "ftp", User: url.UserPassword("normal", ftpPassword), Host: address}
		add(remote.String())
	}
	if root := os.Getenv("INTERN_P4_ROOT"); root != "" {
		for _, name := range []string{"intern.key", "wrong.key"} {
			addKeyFile(add, filepath.Join(root, "keys", name))
		}
	}
	return values
}

// addKeyFile contributes one KEK file: its whole text, the base64 key material of
// every key in the keyset and the decoded key bytes themselves.
func addKeyFile(add func(string), path string) {
	// #nosec G304 -- the fixture key path is derived from the harness root.
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	add(string(raw))
	var keyset struct {
		Key []struct {
			KeyData struct {
				Value string `json:"value"`
			} `json:"keyData"`
		} `json:"key"`
	}
	if err := json.Unmarshal(raw, &keyset); err != nil {
		return
	}
	for _, key := range keyset.Key {
		add(key.KeyData.Value)
		if decoded, decodeErr := base64.StdEncoding.DecodeString(key.KeyData.Value); decodeErr == nil {
			add(string(decoded))
		}
	}
}

// secretIndex reports the position of the first census entry inside text, or -1
// when the text is free of every fixture secret.
func secretIndex(text string) int {
	for index, secret := range census() {
		if strings.Contains(text, secret) {
			return index
		}
	}
	return -1
}

// renderProgress turns one progress callback into one comparable string, so the
// audit covers every value the engine reports, not only the phase name.
func renderProgress(phase restore.Phase, data map[string]any) string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, "phase="+string(phase))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, data[key]))
	}
	return strings.Join(parts, " ")
}

// rehearsalWatch copies the rehearsal server's log while the restore runs. The
// log lives inside the private work directory, which is deleted as soon as the
// restore succeeds, so this is the only moment P4-A7 can inspect it. The returned
// function stops the watcher.
func (w *worker) rehearsalWatch(restoreID string) func() {
	w.t.Helper()
	path := filepath.Join(w.paths.work, "restore-"+restoreID, "mysqld.err")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				observeRehearsalLog(path)
				return
			case <-ticker.C:
				observeRehearsalLog(path)
			}
		}
	}()
	return func() { close(stop); <-done }
}

func observeRehearsalLog(path string) {
	// #nosec G304 -- the path is this restore's own private rehearsal log.
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	watch.observe("rehearsal", string(raw))
}
