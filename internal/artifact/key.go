package artifact

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	aesgcm "github.com/tink-crypto/tink-go/v2/proto/aes_gcm_go_proto"
	"github.com/tink-crypto/tink-go/v2/tink"
	"google.golang.org/protobuf/proto"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

// GenerateKey creates the independent, private AES-128 KEK. Never copy it into a backup directory.
func GenerateKey(path string) (retErr error) {
	if path == "" {
		return errors.New("key-file is required")
	}
	h, err := keyset.NewHandle(aead.AES128GCMKeyTemplate())
	if err != nil {
		return err
	}
	f, err := fsutil.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		fsutil.Close(&retErr, f)
		if retErr != nil {
			retErr = errors.Join(retErr, os.Remove(path))
		}
	}()
	if err = insecurecleartextkeyset.Write(h, keyset.NewJSONWriter(f)); err != nil {
		return err
	}
	return f.Sync()
}

func openKEK(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("encryption requires a separately stored --key-file")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("cannot read key file")
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Size() > 65536 {
		return nil, errors.New("key file must be a regular private file (chmod 600), not a symlink")
	}
	return fsutil.Open(path)
}

func loadKEK(path string) (h *keyset.Handle, retErr error) {
	f, err := openKEK(path)
	if err != nil {
		return nil, err
	}
	defer fsutil.Close(&retErr, f)
	h, err = insecurecleartextkeyset.Read(keyset.NewJSONReader(io.LimitReader(f, 65537)))
	if err != nil {
		return nil, errors.New("invalid KEK keyset")
	}
	if err = validateKEK(h); err != nil {
		return nil, err
	}
	return h, nil
}

func validateKEK(h *keyset.Handle) error {
	material := insecurecleartextkeyset.KeysetMaterial(h)
	if len(material.Key) == 0 {
		return errors.New("empty KEK keyset")
	}
	for _, key := range material.Key {
		if key.KeyData == nil || key.KeyData.TypeUrl != "type.googleapis.com/google.crypto.tink.AesGcmKey" {
			return errors.New("every KEK must be AES-128-GCM")
		}
		var aesKey aesgcm.AesGcmKey
		if proto.Unmarshal(key.KeyData.Value, &aesKey) != nil || len(aesKey.KeyValue) != 16 {
			return errors.New("every KEK must be AES-128-GCM")
		}
	}
	return nil
}

func masterKey(path string) (tink.AEAD, error) {
	h, err := loadKEK(path)
	if err != nil {
		return nil, err
	}
	return aead.New(h)
}

func KeyID(path string) (string, error) {
	h, err := loadKEK(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x", h.KeysetInfo().PrimaryKeyId), nil
}

// ValidateKeyLocation also works before the backup/state directories have been created.
func ValidateKeyLocation(key string, directories ...string) error {
	if key == "" {
		return nil
	}
	keyAbs, err := canonicalPath(key)
	if err != nil {
		return err
	}
	for _, directory := range directories {
		root, err := canonicalPath(directory)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, keyAbs)
		if err != nil {
			return err
		}
		if rel != ".." && !filepath.IsAbs(rel) && (len(rel) < 3 || rel[:3] != ".."+string(os.PathSeparator)) {
			return errors.New("KEK must be separate from backup and state directories")
		}
	}
	return nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := abs
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
