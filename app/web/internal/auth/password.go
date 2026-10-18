// Package auth provides password hashing and server-side session authentication.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

type PasswordParams struct {
	Memory      uint32
	Time        uint32
	Parallelism uint8
}

func DefaultPasswordParams() PasswordParams {
	return PasswordParams{Memory: 65536, Time: 3, Parallelism: 4}
}

func (p PasswordParams) validate() error {
	if p.Parallelism < 1 || p.Parallelism > 16 || p.Memory < 8*uint32(p.Parallelism) || p.Memory > 262144 || p.Time < 1 || p.Time > 10 {
		return errors.New("argon2id parameters are outside supported bounds")
	}
	return nil
}

// Argon2Concurrency is the process-wide cap on concurrent argon2id hash and
// verify operations for one Passwords value. The server keeps a single value.
const Argon2Concurrency = 2

// UnavailableRetryAfter is the Retry-After delay, in seconds, when that cap is
// already full. Callers fail immediately instead of queueing more 64 MiB hashes.
const UnavailableRetryAfter = 1

// ErrUnavailable is returned when Hash or Verify cannot acquire a hashing slot.
var ErrUnavailable = errors.New("password hasher is at capacity")

type Passwords struct {
	params PasswordParams
	dummy  string
	slots  chan struct{}
}

func NewPasswords(params PasswordParams) (*Passwords, error) {
	if err := params.validate(); err != nil {
		return nil, err
	}
	passwords := &Passwords{params: params, slots: make(chan struct{}, Argon2Concurrency)}
	dummy, err := passwords.Hash(base64.RawStdEncoding.EncodeToString(randomBytes(32)))
	if err != nil {
		return nil, err
	}
	passwords.dummy = dummy
	return passwords, nil
}

func (p *Passwords) acquire() error {
	select {
	case p.slots <- struct{}{}:
		return nil
	default:
		return ErrUnavailable
	}
}

func (p *Passwords) release() { <-p.slots }

func randomBytes(size int) []byte {
	data := make([]byte, size)
	rand.Read(data)
	return data
}

func (p *Passwords) Hash(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	if err := p.acquire(); err != nil {
		return "", err
	}
	defer p.release()
	salt := randomBytes(16)
	hash := argon2.IDKey([]byte(password), salt, p.params.Time, p.params.Memory, p.params.Parallelism, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", p.params.Memory, p.params.Time, p.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

// Verify parses the stored PHC parameters rather than silently using new defaults.
func (p *Passwords) Verify(password, encoded string) (bool, error) {
	params, salt, hash, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	if err = p.acquire(); err != nil {
		return false, err
	}
	defer p.release()
	actual := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Parallelism, 32)
	return subtle.ConstantTimeCompare(actual, hash) == 1, nil
}

func (p *Passwords) VerifyDummy(password string) error {
	// This is generated with the same parameters as real passwords at startup.
	_, err := p.Verify(password, p.dummy)
	return err
}

func parsePHC(encoded string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return PasswordParams{}, nil, nil, errors.New("invalid argon2id encoding")
	}
	params, err := parseParams(parts[3])
	if err != nil {
		return params, nil, nil, err
	}
	salt, saltErr := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	hash, hashErr := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if saltErr != nil || hashErr != nil || len(salt) != 16 || len(hash) != 32 {
		return params, nil, nil, errors.New("invalid argon2id salt or hash")
	}
	return params, salt, hash, nil
}

func parseParams(encoded string) (PasswordParams, error) {
	parts := strings.Split(encoded, ",")
	if len(parts) != 3 {
		return PasswordParams{}, errors.New("invalid argon2id parameters")
	}
	values := [3]uint64{}
	for i, prefix := range []string{"m=", "t=", "p="} {
		value, ok := strings.CutPrefix(parts[i], prefix)
		if !ok {
			return PasswordParams{}, errors.New("invalid argon2id parameters")
		}
		bits := 32
		if i == 2 {
			bits = 8
		}
		parsed, err := strconv.ParseUint(value, 10, bits)
		if err != nil {
			return PasswordParams{}, errors.New("invalid argon2id parameters")
		}
		values[i] = parsed
	}
	params := PasswordParams{Memory: uint32(values[0]), Time: uint32(values[1]), Parallelism: uint8(values[2])}
	return params, params.validate()
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,64}$`)

func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return errors.New("username must be 3–64 ASCII letters, digits, dots, underscores, or hyphens")
	}
	return nil
}

func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < 12 || n > 256 {
		return errors.New("password must contain 12–256 characters")
	}
	return nil
}

func ValidRole(role string) bool { return role == "admin" || role == "operator" || role == "viewer" }
