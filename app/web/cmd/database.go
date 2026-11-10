package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

// databaseURL reads the PostgreSQL connection string from exactly one source.
// A direct value and a file are mutually exclusive. The string is never logged;
// file errors name only the file path.
func (c configuration) databaseURL() (string, error) {
	if c.DatabaseURL != "" && c.DatabaseURLFile != "" {
		return "", errors.New("database_url and database_url_file are mutually exclusive")
	}
	if c.DatabaseURL != "" {
		return strings.TrimSpace(c.DatabaseURL), nil
	}
	if c.DatabaseURLFile == "" {
		return "", errors.New("database_url or database_url_file is required")
	}
	value, err := readSecretFile(c.DatabaseURLFile)
	if err != nil {
		return "", fmt.Errorf("cannot read database_url_file: %w", err)
	}
	return value, nil
}

func readSecretFile(path string) (value string, retErr error) {
	f, err := openDatabaseSecret(path)
	if err != nil {
		return "", err
	}
	defer fsutil.Close(&retErr, f)
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("must be a non-symlink private regular file (chmod 600)")
	}
	if err = setConfigBlocking(f); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", errors.New("must contain at most 4096 bytes")
	}
	value = strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("must contain one non-empty line")
	}
	return value, nil
}
