// Package credential seals control-plane secrets with the deployment KEK.
//
// Every ciphertext is bound to the exact table, row and field it belongs to, so
// a value copied to another row or another column fails authentication. The KEK
// is the same independently stored keyset used by backup artifacts; this package
// only adds associated-data binding and refuses a KEK placed inside the
// directories the caller names.
package credential

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tink-crypto/tink-go/v2/tink"

	"github.com/flyingEirc/Intern/internal/artifact"
)

// Sealer encrypts and authenticates single secret values.
type Sealer struct {
	aead tink.AEAD
}

// Open loads the KEK at kekPath and rejects a location inside any forbiddenDir.
// The KEK location rules are artifact's: a regular 0600 file, not a symlink, and
// outside every named directory.
func Open(kekPath string, forbiddenDirs ...string) (*Sealer, error) {
	if kekPath == "" {
		return nil, errors.New("credential encryption requires a configured KEK file")
	}
	if err := artifact.ValidateKeyLocation(kekPath, forbiddenDirs...); err != nil {
		return nil, err
	}
	aead, err := artifact.AEAD(kekPath)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal returns a ciphertext bound to table:id:field.
func (s *Sealer) Seal(table, id, field string, plaintext []byte) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, errors.New("credential sealer is not configured")
	}
	context, err := associatedData(table, id, field)
	if err != nil {
		return nil, err
	}
	if len(plaintext) == 0 {
		return nil, errors.New("secret must not be empty")
	}
	return s.aead.Encrypt(plaintext, context)
}

// Unseal returns the plaintext bound to table:id:field. A different KEK, or a
// ciphertext stored under another row or field, fails authentication.
func (s *Sealer) Unseal(table, id, field string, ciphertext []byte) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, errors.New("credential sealer is not configured")
	}
	context, err := associatedData(table, id, field)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) == 0 {
		return nil, errors.New("sealed secret is empty")
	}
	plaintext, err := s.aead.Decrypt(ciphertext, context)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt %s.%s secret", table, field)
	}
	return plaintext, nil
}

// associatedData is the frozen AAD grammar `table:id:field`. Row and field
// values are validated so that no two distinct bindings produce one string.
func associatedData(table, id, field string) ([]byte, error) {
	for _, part := range []struct{ name, value string }{
		{"table", table}, {"id", id}, {"field", field},
	} {
		if part.value == "" || strings.ContainsRune(part.value, ':') {
			return nil, fmt.Errorf("invalid credential binding: %s part is empty or contains a colon", part.name)
		}
	}
	return []byte(table + ":" + id + ":" + field), nil
}
