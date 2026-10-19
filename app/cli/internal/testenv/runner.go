//go:build integration

package testenv

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/flyingEirc/Intern/app/cli/internal/qualitygate"
	"github.com/flyingEirc/Intern/internal/backup"
)

type connectionConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	DB       string `json:"db"`
	TLSMode  string `json:"tls_mode"`
	TLSCA    string `json:"tls_ca"`
}
type backupConfig struct {
	Directory       string `json:"directory"`
	StateDirectory  string `json:"state_directory"`
	Project         string `json:"project"`
	Compress        bool   `json:"compress"`
	Encrypt         bool   `json:"encrypt"`
	KeyFile         string `json:"key_file"`
	MaxRestoreBytes int64  `json:"max_restore_bytes"`
}
type cliConfig struct {
	Connection connectionConfig `json:"connection"`
	Backup     backupConfig     `json:"backup"`
}

func (c *Case) configuration(db *Database, restore bool) cliConfig {
	user, password, name := "intern_backup", c.backupPassword, "app"
	if restore {
		user, password, name = "intern_restore", c.restorePassword, "restored"
	}
	return cliConfig{Connection: connectionConfig{Host: db.Alias, Port: 3306, Username: user, Password: password, DB: name, TLSMode: "VERIFY_IDENTITY", TLSCA: "/run/secrets/ca.pem"}, Backup: backupConfig{Directory: "/work/backups", StateDirectory: "/work/state", Project: c.prefix, Compress: true, Encrypt: true, KeyFile: "/run/secrets/key", MaxRestoreBytes: 128 << 20}}
}

func (c *Case) startRunner() error {
	source, err := json.Marshal(c.configuration(c.Source, false))
	if err != nil {
		return err
	}
	target, err := json.Marshal(c.configuration(c.Target, true))
	if err != nil {
		return err
	}
	volumes := []string{c.prefix + "-backups", c.prefix + "-state", c.prefix + "-secrets"}
	req := tc.ContainerRequest{Image: c.Suite.Image, ImagePlatform: c.Suite.Candidate.Platform, Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, ConfigModifier: rootUser, HostConfigModifier: boundedHost, Networks: []string{c.Network.Name}, NetworkAliases: map[string][]string{c.Network.Name: {"runner"}}, Mounts: tc.ContainerMounts{tc.VolumeMount(volumes[0], "/work/backups"), tc.VolumeMount(volumes[1], "/work/state"), tc.VolumeMount(volumes[2], "/run/secrets")}, Tmpfs: map[string]string{"/limited": "rw,size=1048576,mode=0700,uid=10001,gid=10001"}, Files: []tc.ContainerFile{{Reader: bytes.NewReader(source), ContainerFilePath: "/run/secrets/source.yaml", FileMode: 0o600}, {Reader: bytes.NewReader(target), ContainerFilePath: "/run/secrets/target.yaml", FileMode: 0o600}, {Reader: bytes.NewReader(c.CA.PEM), ContainerFilePath: "/run/secrets/ca.pem", FileMode: 0o600}}, WaitingFor: wait.ForExec([]string{"/usr/bin/test", "-x", "/usr/local/bin/intern"}).WithStartupTimeout(30 * time.Second)}
	c.Runner, err = tc.GenericContainer(c.Ctx, tc.GenericContainerRequest{ContainerRequest: req, Started: true})
	c.ownContainer(c.Runner, "CLI runner", volumes...)
	if err != nil {
		return c.SafeError(err)
	}
	if _, code, runErr := execContainer(c.Ctx, c.Runner, "0", []string{"/bin/sh", "-c", "chown -R 10001:10001 /work/backups /work/state /run/secrets && chmod 700 /work/backups /work/state /run/secrets"}); runErr != nil || code != 0 {
		return errors.New("cannot initialize private runner volumes")
	}
	output, code, err := execContainer(c.Ctx, c.Runner, "10001", []string{"sha256sum", "/usr/local/bin/intern"})
	if err != nil || code != 0 {
		return errors.New("cannot identify scenario executable")
	}
	if !strings.HasPrefix(output, c.Suite.Candidate.BinarySHA256+" ") {
		return errors.New("scenario executable differs from candidate")
	}
	c.OK("keygen", "--config", "/run/secrets/source.yaml")
	return nil
}

func (c *Case) Command(args ...string) ([]byte, int) {
	c.T.Helper()
	argv := append([]string{"timeout", "--signal=TERM", "--kill-after=5s", "90s", "/usr/local/bin/intern"}, args...)
	output, code, err := execContainer(c.Ctx, c.Runner, "10001", argv)
	c.Must(c.SafeError(err))
	if code == 124 || code == 137 {
		c.T.Fatal("CLI operation exceeded its deadline; not an expected business refusal")
	}
	return []byte(output), code
}

func (c *Case) OK(args ...string) []byte {
	c.T.Helper()
	out, code := c.Command(args...)
	if code != 0 {
		c.T.Fatalf("CLI %s failed (exit %d): %s", args[0], code, c.Redact(string(out)))
	}
	return out
}

func (c *Case) Refuse(id string, args ...string) {
	c.T.Helper()
	_, code := c.Command(args...)
	c.Assert(id, code != 0)
}

func (c *Case) Backup(kind string, flags ...string) backup.Result {
	c.T.Helper()
	args := append([]string{"backup", kind, "--config", "/run/secrets/source.yaml"}, flags...)
	var result backup.Result
	c.Must(json.Unmarshal(c.OK(args...), &result))
	c.mu.Lock()
	c.Receipt.Checkpoints = append(c.Receipt.Checkpoints, qualitygate.Checkpoint{BackupID: result.ID, File: result.End.File, Position: result.End.Position})
	c.mu.Unlock()
	return result
}

func (c *Case) Restore(id string, execute bool, flags ...string) backup.RestorePlan {
	c.T.Helper()
	args := []string{"restore", "--config", "/run/secrets/target.yaml", "--backup", id}
	if execute {
		args = append(args, "--execute")
	}
	args = append(args, flags...)
	output, code := c.Command(args...)
	if code != 0 {
		c.T.Fatalf("CLI restore failed (exit %d): %s; target SQL error: %s", code, c.Redact(string(output)), c.LastStatementError(c.Target))
	}
	var plan backup.RestorePlan
	c.Must(json.Unmarshal(output, &plan))
	return plan
}

func (c *Case) List(flags ...string) []backup.Result {
	c.T.Helper()
	args := append([]string{"backup", "list", "--config", "/run/secrets/source.yaml"}, flags...)
	var list []backup.Result
	c.Must(json.Unmarshal(c.OK(args...), &list))
	return list
}

func (c *Case) PointTarget(db *Database) {
	c.T.Helper()
	c.Target = db
	c.TargetConfiguration(db, "/run/secrets/target.yaml")
}

func (c *Case) TargetConfiguration(db *Database, path string) {
	c.T.Helper()
	data, err := json.Marshal(c.configuration(db, true))
	c.Must(err)
	c.Copy(path, data)
}

func (c *Case) PointSource(db *Database) {
	c.T.Helper()
	c.Source = db
	data, err := json.Marshal(c.configuration(db, false))
	c.Must(err)
	c.Copy("/run/secrets/source.yaml", data)
}

func (c *Case) Copy(path string, data []byte) {
	c.T.Helper()
	c.Must(c.Runner.CopyToContainer(c.Ctx, data, path, 0o600))
	c.Root("chown", "10001:10001", path)
}

func (c *Case) Read(path string) []byte {
	c.T.Helper()
	reader, err := c.Runner.CopyFileFromContainer(c.Ctx, path)
	c.Must(err)
	data, readErr := io.ReadAll(io.LimitReader(reader, 16<<20+1))
	c.Must(errors.Join(readErr, reader.Close()))
	if len(data) > 16<<20 {
		c.T.Fatal("fixture file exceeded copy limit")
	}
	return data
}

func (c *Case) Root(args ...string) string {
	c.T.Helper()
	output, code, err := execContainer(c.Ctx, c.Runner, "0", args)
	c.Must(c.SafeError(err))
	if code != 0 {
		c.T.Fatal(fmt.Errorf("fixture control failed: %s", c.Redact(output)))
	}
	return output
}
