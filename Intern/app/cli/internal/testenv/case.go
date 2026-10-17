//go:build integration

package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"

	"github.com/flyingEirc/IotProject/app/cli/internal/qualitygate"
)

type Case struct {
	T                                                     *testing.T
	Ctx                                                   context.Context
	Suite                                                 *Suite
	Network                                               *tc.DockerNetwork
	Runner                                                tc.Container
	Source, Target                                        *Database
	CA                                                    *authority
	Receipt                                               qualitygate.Receipt
	prefix, rootPassword, backupPassword, restorePassword string
	mu                                                    sync.Mutex
}

func (s *Suite) NewCase(t *testing.T, id string) *Case {
	t.Helper()
	if !s.Contract.HasCase(id) {
		t.Fatal("case is absent from required contract")
	}
	select {
	case s.slots <- struct{}{}:
	case <-t.Context().Done():
		t.Fatal("case resource budget timed out")
	}
	t.Cleanup(func() { <-s.slots })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	c := &Case{T: t, Ctx: ctx, Suite: s, prefix: "intern-" + randomSecret()[:16], rootPassword: randomSecret(), backupPassword: randomSecret(), restorePassword: randomSecret()}
	c.Receipt = qualitygate.Receipt{FormatVersion: 1, CaseID: id, SourceSHA: s.Candidate.SourceSHA, Profile: s.Profile.ID, Platform: s.Candidate.Platform, CandidateImageID: s.Image, BinarySHA256: s.Candidate.BinarySHA256, StartedAt: time.Now().UTC(), Stage: "setup", Status: "running"}
	// Registered before resources, so this runs after every resource cleanup.
	t.Cleanup(c.finish)
	var err error
	c.CA, err = newAuthority()
	c.Must(err)
	c.Network, err = network.New(ctx, network.WithLabels(map[string]string{"intern.quality.case": c.prefix}))
	c.ownNetwork()
	c.Must(err)
	c.Source, err = c.NewDatabase("source", s.Profile, true)
	c.Must(err)
	c.Target, err = c.NewDatabase("target", s.Profile, true)
	c.Must(err)
	c.Must(c.startRunner())
	c.Stage("ready")
	return c
}
func randomSecret() string {
	var data [24]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data[:])
}
func (c *Case) Must(err error) {
	c.T.Helper()
	if err != nil {
		c.T.Fatal(c.Redact(err.Error()))
	}
}
func (c *Case) Redact(message string) string {
	for _, secret := range []string{c.rootPassword, c.backupPassword, c.restorePassword} {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	return message
}
func (c *Case) Stage(stage string) { c.mu.Lock(); defer c.mu.Unlock(); c.Receipt.Stage = stage }
func (c *Case) Assert(id string, condition bool) {
	c.T.Helper()
	if !condition {
		c.T.Fatalf("oracle assertion failed: %s", id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Receipt.Assertions = append(c.Receipt.Assertions, id)
}
func (c *Case) Verified()    { c.Stage("verified") }
func (c *Case) addResource() { c.mu.Lock(); defer c.mu.Unlock(); c.Receipt.ResourcesCreated++ }
func (c *Case) removed(err error, role string) {
	if err != nil {
		c.T.Errorf("cleanup failed (%s): %s", role, c.Redact(err.Error()))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Receipt.ResourcesRemoved++
}
func (c *Case) ownNetwork() {
	if c.Network == nil {
		return
	}
	c.addResource()
	c.T.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c.removed(c.Network.Remove(ctx), "network")
	})
}
func (c *Case) ownContainer(ctr tc.Container, role string, volumes ...string) {
	if ctr == nil {
		return
	}
	c.addResource()
	c.T.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c.removed(tc.TerminateContainer(ctr, tc.StopContext(ctx), tc.StopTimeout(5*time.Second), tc.RemoveVolumes(volumes...)), role)
	})
}
func (c *Case) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Receipt.FinishedAt = time.Now().UTC()
	c.Receipt.Status = "passed"
	if c.T.Failed() {
		c.Receipt.Status = "failed"
	}
	if c.T.Skipped() {
		c.Receipt.Status = "skipped"
	}
	c.Receipt.CleanupComplete = c.Receipt.ResourcesCreated == c.Receipt.ResourcesRemoved
	if err := c.Suite.writeJSON(filepath.Join("receipts", c.Receipt.CaseID+".json"), c.Receipt); err != nil {
		c.T.Errorf("write case receipt: %v", err)
	}
}
func (c *Case) StopDatabase(db *Database) {
	c.T.Helper()
	c.Must(db.SQL.Close())
	timeout := 5 * time.Second
	c.Must(db.Container.Stop(c.Ctx, &timeout))
}
func (c *Case) RestartDatabase(db *Database) {
	c.T.Helper()
	c.Must(db.Container.Start(c.Ctx))
	c.Must(c.connectDatabase(db))
	c.Must(c.waitDatabase(db))
}
func (c *Case) Await(description string, check func() (bool, error)) {
	c.T.Helper()
	ctx, cancel := context.WithTimeout(c.Ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := check()
		c.Must(err)
		if ready {
			return
		}
		select {
		case <-ctx.Done():
			c.T.Fatal(fmt.Errorf("observation timed out: %s: %w", description, ctx.Err()))
		case <-ticker.C:
		}
	}
}
func (c *Case) SafeError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(c.Redact(err.Error()))
}
