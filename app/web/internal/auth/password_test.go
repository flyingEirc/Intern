package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testPasswords(t *testing.T) *Passwords {
	t.Helper()
	passwords, err := NewPasswords(PasswordParams{Memory: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	return passwords
}

func TestArgon2RejectsImmediatelyWhenSaturated(t *testing.T) {
	passwords := saturatedPasswords(t)
	start := time.Now()
	_, verifyErr := passwords.Verify("test-credential-123", passwords.dummy)
	_, hashErr := passwords.Hash("test-credential-123")
	requireUnavailable(t, verifyErr)
	requireUnavailable(t, hashErr)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("saturated hasher blocked for %s", time.Since(start))
	}
	<-passwords.slots
	valid, err := passwords.Verify("not-the-dummy-password", passwords.dummy)
	if err != nil || valid {
		t.Fatalf("verification after a free slot = %v, %v", valid, err)
	}
}

func saturatedPasswords(t *testing.T) *Passwords {
	t.Helper()
	passwords := testPasswords(t)
	for range Argon2Concurrency {
		if err := passwords.acquire(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for range cap(passwords.slots) {
			select {
			case <-passwords.slots:
			default:
				return
			}
		}
	})
	return passwords
}

func requireUnavailable(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestPasswordPHCParametersAndVerification(t *testing.T) {
	passwords := testPasswords(t)
	password := "test-credential-123"
	encoded, err := passwords.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatal("missing PHC parameters")
	}
	// A differently configured verifier must honor the parameters in the hash.
	other, err := NewPasswords(PasswordParams{Memory: 128, Time: 2, Parallelism: 2})
	if err != nil {
		t.Fatal(err)
	}
	valid, err := other.Verify(password, encoded)
	if err != nil || !valid {
		t.Fatalf("verification = %v, %v", valid, err)
	}
	valid, err = passwords.Verify("wrong-credential-123", encoded)
	if err != nil || valid {
		t.Fatalf("wrong password = %v, %v", valid, err)
	}
	second, err := passwords.Hash(password)
	if err != nil || second == encoded {
		t.Fatal("password salt was reused")
	}
}

func TestPasswordDefaultsAndDummyHash(t *testing.T) {
	passwords := testPasswords(t)
	if err := passwords.VerifyDummy("test-credential-123"); err != nil {
		t.Fatal(err)
	}
	if DefaultPasswordParams() != (PasswordParams{Memory: 65536, Time: 3, Parallelism: 4}) {
		t.Fatal("production parameters changed")
	}
}

func TestMalformedPHCIsRejectedBeforeHashing(t *testing.T) {
	passwords := testPasswords(t)
	valid, err := passwords.Hash("test-credential-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range []string{
		"", "$argon2i$v=19$m=64,t=1,p=1$x$x", strings.Replace(valid, "v=19", "v=18", 1),
		strings.Replace(valid, "m=64", "m=0", 1), strings.Replace(valid, "m=64", "m=262145", 1),
		strings.Replace(valid, "m=64", "m=4294967296", 1), strings.Replace(valid, "t=1", "t=0", 1),
		strings.Replace(valid, "t=1", "t=11", 1), strings.Replace(valid, "p=1", "p=0", 1),
		strings.Replace(valid, "p=1", "p=17", 1), strings.Replace(valid, "p=1", "p=256", 1),
		strings.Replace(valid, "m=64,t=1,p=1", "t=1,m=64,p=1", 1), valid + "$extra", valid[:len(valid)-2],
	} {
		if _, err = passwords.Verify("test-credential-123", encoded); err == nil {
			t.Fatal("malformed PHC accepted")
		}
	}
	if _, err = NewPasswords(PasswordParams{}); err == nil {
		t.Fatal("invalid hashing parameters accepted")
	}
}

func TestUsernameAndRunePasswordPolicy(t *testing.T) {
	for _, username := range []string{"abc", "ADMIN_123.test-1", strings.Repeat("a", 64)} {
		if err := ValidateUsername(username); err != nil {
			t.Fatal(err)
		}
	}
	for _, username := range []string{"ab", strings.Repeat("a", 65), "with space", "用户名字", "a/b"} {
		if err := ValidateUsername(username); err == nil {
			t.Fatal("invalid username accepted")
		}
	}
	for _, size := range []int{12, 256} {
		if err := ValidatePassword(strings.Repeat("密", size)); err != nil {
			t.Fatal(err)
		}
	}
	for _, size := range []int{0, 11, 257} {
		if err := ValidatePassword(strings.Repeat("密", size)); err == nil {
			t.Fatal("invalid password length accepted")
		}
	}
}

func TestSessionConfigBounds(t *testing.T) {
	defaults := DefaultSessionConfig()
	if err := defaults.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, config := range []SessionConfig{{}, {IdleTimeout: -1, Lifetime: 1}, {IdleTimeout: 2, Lifetime: 1}, {IdleTimeout: 1, Lifetime: -1}} {
		if err := config.Validate(); err == nil {
			t.Fatal("invalid session configuration accepted")
		}
	}
}
