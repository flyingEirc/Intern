package credential

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/internal/artifact"
)

func testKey(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := artifact.GenerateKey(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSealUnsealRoundTripBindsRowAndField(t *testing.T) {
	dir := t.TempDir()
	sealer, err := Open(testKey(t, dir, "kek.json"))
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("{{SECRET_ftp_account}}")
	sealed, err := sealer.Seal("sources", "row-a", "secret_ciphertext", secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("ciphertext contains the plaintext")
	}
	opened, err := sealer.Unseal("sources", "row-a", "secret_ciphertext", sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("round trip = %q", opened)
	}
	other, err := sealer.Seal("sources", "row-a", "secret_ciphertext", secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(other, sealed) {
		t.Fatal("two seals produced identical ciphertext")
	}
}

func TestUnsealRejectsMovedCiphertext(t *testing.T) {
	dir := t.TempDir()
	sealer, err := Open(testKey(t, dir, "kek.json"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal("repositories", "row-a", "secret_ciphertext", []byte("ftp-password"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ table, id, field string }{
		{"repositories", "row-b", "secret_ciphertext"},
		{"sources", "row-a", "secret_ciphertext"},
		{"repositories", "row-a", "other_ciphertext"},
	} {
		if _, err = sealer.Unseal(tc.table, tc.id, tc.field, sealed); err == nil {
			t.Fatalf("moved ciphertext accepted for %s:%s:%s", tc.table, tc.id, tc.field)
		}
	}
	if _, err = sealer.Unseal("repositories", "row-a", "secret_ciphertext", append([]byte{0}, sealed...)); err == nil {
		t.Fatal("corrupted ciphertext accepted")
	}
}

func TestUnsealRejectsAnotherKEK(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(testKey(t, dir, "first.json"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(testKey(t, dir, "second.json"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := first.Seal("sources", "row", "secret_ciphertext", []byte("source-password"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = second.Unseal("sources", "row", "secret_ciphertext", sealed); err == nil {
		t.Fatal("a different KEK decrypted the secret")
	}
}

func TestOpenRejectsForbiddenLocationAndUnsafeKey(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "metadata")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	key := testKey(t, inside, "kek.json")
	if _, err := Open(key, inside); err == nil {
		t.Fatal("KEK inside a forbidden directory was accepted")
	}
	if _, err := Open(key, filepath.Join(dir, "elsewhere")); err != nil {
		t.Fatalf("KEK outside forbidden directories rejected: %v", err)
	}
	if _, err := Open(""); err == nil {
		t.Fatal("empty KEK path accepted")
	}
	loose := testKey(t, dir, "loose.json")
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(loose); err == nil {
		t.Fatal("world-readable KEK accepted")
	}
	symlink := filepath.Join(dir, "link.json")
	if err := os.Symlink(filepath.Join(dir, "loose.json"), symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(symlink); err == nil {
		t.Fatal("symlinked KEK accepted")
	}
}

func TestSealRejectsUnboundAndEmptyInput(t *testing.T) {
	dir := t.TempDir()
	sealer, err := Open(testKey(t, dir, "kek.json"))
	if err != nil {
		t.Fatal(err)
	}
	var nilSealer *Sealer
	if _, err = nilSealer.Seal("t", "i", "f", []byte("x")); err == nil {
		t.Fatal("nil sealer sealed a value")
	}
	for _, tc := range []struct{ table, id, field string }{
		{"", "i", "f"}, {"t", "", "f"}, {"t", "i", ""}, {"t:i", "i", "f"}, {"t", "i:f", "f"},
	} {
		if _, err = sealer.Seal(tc.table, tc.id, tc.field, []byte("x")); err == nil {
			t.Fatalf("invalid binding %q:%q:%q accepted", tc.table, tc.id, tc.field)
		}
	}
	if _, err = sealer.Seal("t", "i", "f", nil); err == nil {
		t.Fatal("empty secret sealed")
	}
	if _, err = sealer.Unseal("t", "i", "f", nil); err == nil {
		t.Fatal("empty ciphertext unsealed")
	}
}

func TestSealedValueDoesNotLeakPlaintextThroughErrors(t *testing.T) {
	dir := t.TempDir()
	sealer, err := Open(testKey(t, dir, "kek.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = sealer.Unseal("sources", "row", "secret_ciphertext", []byte("not-a-ciphertext"))
	if err == nil {
		t.Fatal("garbage ciphertext accepted")
	}
	if strings.Contains(err.Error(), "not-a-ciphertext") {
		t.Fatalf("error leaked ciphertext: %v", err)
	}
}
