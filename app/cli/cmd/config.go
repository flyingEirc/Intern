package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/flyingEirc/IotProject/internal/backup"
	"github.com/flyingEirc/IotProject/internal/fsutil"
	"github.com/flyingEirc/IotProject/internal/schedule"
)

type backupConfig struct {
	Directory       string `mapstructure:"directory"`
	StateDirectory  string `mapstructure:"state_directory"`
	Project         string `mapstructure:"project"`
	Compress        bool   `mapstructure:"compress"`
	Encrypt         bool   `mapstructure:"encrypt"`
	KeyFile         string `mapstructure:"key_file"`
	MaxRestoreBytes int64  `mapstructure:"max_restore_bytes"`
}
type configuration struct {
	Connection backup.Connection `mapstructure:"connection"`
	Backup     backupConfig      `mapstructure:"backup"`
	Schedule   schedule.Config   `mapstructure:"schedule"`
}

func (c configuration) options() backup.Options {
	return backup.Options{Connection: c.Connection, Directory: c.Backup.Directory, StateDirectory: c.Backup.StateDirectory, Project: c.Backup.Project, Compress: c.Backup.Compress, Encrypt: c.Backup.Encrypt, KeyFile: c.Backup.KeyFile, MaxRestoreBytes: c.Backup.MaxRestoreBytes}
}
func (l *configLoader) load() (configuration, error) {
	var c configuration
	if l.initErr != nil {
		return c, l.initErr
	}
	path, err := l.flags.GetString("config")
	if err != nil {
		return c, err
	}
	if !l.flags.Changed("config") {
		path = os.Getenv("INTERN_CONFIG")
	}
	if path != "" {
		if err = l.readConfig(path); err != nil {
			return c, err
		}
	}
	if err = l.v.UnmarshalExact(&c); err != nil {
		return c, errors.New("invalid configuration key or value type; see example.yaml")
	}
	if err = l.selectPasswordSource(&c.Connection); err != nil {
		return c, err
	}
	if c.Connection.Password == "__INTERN_PROMPT__" {
		c.Connection.Password, err = promptPassword(l.stderr)
		if err != nil {
			return c, err
		}
	}
	return c, nil
}
func (l *configLoader) readConfig(path string) (retErr error) {
	f, err := fsutil.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open YAML configuration %q", path)
	}
	defer fsutil.Close(&retErr, f)
	l.v.SetConfigType("yaml")
	if err = l.v.ReadConfig(f); err != nil {
		return fmt.Errorf("cannot read YAML configuration %q (unreadable or invalid syntax)", path)
	}
	if !l.v.InConfig("connection.password") {
		return nil
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("YAML containing connection.password must be a regular private file (chmod 600); password_env is recommended")
	}
	return nil
}
func (l *configLoader) selectPasswordSource(c *backup.Connection) error {
	_, envPassword := os.LookupEnv("INTERN_CONNECTION_PASSWORD")
	_, envReference := os.LookupEnv("INTERN_CONNECTION_PASSWORD_ENV")
	switch {
	case l.flags.Changed("password"):
		c.PasswordEnv = ""
	case l.flags.Changed("password-env"):
		c.Password = ""
	case envPassword && envReference:
		return errors.New("set only one of INTERN_CONNECTION_PASSWORD and INTERN_CONNECTION_PASSWORD_ENV")
	case envPassword:
		c.PasswordEnv = ""
	case envReference:
		c.Password = ""
	case c.Password != "" && c.PasswordEnv != "":
		return errors.New("YAML must select either password or password_env, not both")
	}
	return nil
}
func promptPassword(stderr io.Writer) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("password prompt requires a terminal; use password_env for automation")
	}
	if _, err := fmt.Fprint(stderr, "MySQL password: "); err != nil {
		return "", err
	}
	b, readErr := term.ReadPassword(int(os.Stdin.Fd()))
	defer clear(b)
	_, lineErr := fmt.Fprintln(stderr)
	if readErr != nil {
		return "", errors.Join(errors.New("password input failed"), lineErr)
	}
	if lineErr != nil {
		return "", lineErr
	}
	return string(b), nil
}
