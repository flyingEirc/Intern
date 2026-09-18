package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

func TestPublishedEncryptionModeCannotBeDowngraded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifact")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Write(context.Background(), dir, bytes.NewBufferString("replacement"), []byte("known manifest"), Options{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := Read(context.Background(), dir, &out, ReadOptions{WorkDirectory: t.TempDir(), MaxBytes: 100, Encrypt: true}); err == nil {
		t.Fatal("plaintext substitute for encrypted publication accepted")
	}
	if out.Len() != 0 {
		t.Fatal("unverified output escaped")
	}
}

func TestRetainedKEKDecryptsOldBackupAfterPrimaryRotation(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := GenerateKey(key); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "artifact")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Write(context.Background(), dir, bytes.NewBufferString("before rotation"), []byte("manifest"), Options{Encrypt: true, KeyFile: key}); err != nil {
		t.Fatal(err)
	}
	oldID, err := KeyID(key)
	if err != nil {
		t.Fatal(err)
	}
	rotateKEK(t, key)
	newID, err := KeyID(key)
	if err != nil {
		t.Fatal(err)
	}
	if newID == oldID {
		t.Fatal("primary key did not rotate")
	}
	var out bytes.Buffer
	if _, err = Read(context.Background(), dir, &out, ReadOptions{KeyFile: key, WorkDirectory: t.TempDir(), MaxBytes: 100, Encrypt: true}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "before rotation" {
		t.Fatal("old data was not recovered with retained KEK")
	}
}

func rotateKEK(t *testing.T, path string) {
	t.Helper()
	h, err := loadKEK(path)
	requireNoError(t, err)
	manager := keyset.NewManagerFromHandle(h)
	id, err := manager.Add(aead.AES128GCMKeyTemplate())
	requireNoError(t, err)
	requireNoError(t, manager.SetPrimary(id))
	rotated, err := manager.Handle()
	requireNoError(t, err)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	requireNoError(t, err)
	defer f.Close()
	requireNoError(t, insecurecleartextkeyset.Write(rotated, keyset.NewJSONWriter(f)))
}
