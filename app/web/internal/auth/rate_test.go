package auth

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoginLimiterBothLevelsAndCaseFolding(t *testing.T) {
	limiter := NewLoginLimiter()
	now := time.Now()
	for i := range 5 {
		if retry := limiter.Allow("192.0.2.1", "Admin", now); retry != 0 {
			t.Fatalf("attempt %d: retry=%d", i, retry)
		}
	}
	if retry := limiter.Allow("192.0.2.1", "ADMIN", now); retry != 12 {
		t.Fatalf("pair Retry-After=%d", retry)
	}
	if retry := limiter.Allow("192.0.2.2", "admin", now); retry != 0 {
		t.Fatalf("different IP blocked: %d", retry)
	}
	for i := range 24 {
		if retry := limiter.Allow("192.0.2.1", "name"+strconv.Itoa(i), now); retry != 0 {
			t.Fatalf("IP attempt %d: %d", i, retry)
		}
	}
	if retry := limiter.Allow("192.0.2.1", "different", now); retry != 2 {
		t.Fatalf("IP Retry-After=%d", retry)
	}
	if retry := limiter.Allow("192.0.2.1", "ADMIN", now.Add(12*time.Second)); retry != 0 {
		t.Fatalf("refilled pair blocked: %d", retry)
	}
}

func TestLoginLimiterBoundAndLRUEviction(t *testing.T) {
	limiter := NewLoginLimiter()
	limiter.capacity = 4
	now := time.Now()
	limiter.Allow("one", "user", now)
	limiter.Allow("two", "user", now)
	limiter.Allow("one", "user", now)
	limiter.Allow("three", "user", now)
	if len(limiter.entries) != 4 || limiter.lru.Len() != 4 {
		t.Fatal("bucket limit exceeded")
	}
	if _, exists := limiter.entries["ip:two"]; exists {
		t.Fatal("least recently used IP retained")
	}
	if _, exists := limiter.entries["ip:one"]; !exists {
		t.Fatal("recently used IP evicted")
	}
	for i := range 12000 {
		limiter.Allow(strconv.Itoa(i), "user", now)
	}
	if len(limiter.entries) != 4 || limiter.lru.Len() != 4 {
		t.Fatal("unbounded bucket growth")
	}
}

func TestLoginLimiterMalformedUsernamesStayBounded(t *testing.T) {
	limiter := NewLoginLimiter()
	now := time.Now()
	for i := range 200 {
		limiter.Allow("192.0.2.8", strings.Repeat("x", 4096)+strconv.Itoa(i), now)
	}
	if len(limiter.entries) != 2 {
		t.Fatalf("entries=%d", len(limiter.entries))
	}
	for key := range limiter.entries {
		if len(key) > 80 {
			t.Fatalf("limiter retained an oversized key of %d bytes", len(key))
		}
	}
	limiter.Allow("192.0.2.8", strings.Repeat("y", 1<<20), now)
	if len(limiter.entries) != 2 {
		t.Fatal("IP denial created another user bucket")
	}
	if retry := limiter.Allow("192.0.2.9", "alice", now); retry != 0 {
		t.Fatal(retry)
	}
	if _, exists := limiter.entries["user:192.0.2.9\x00alice"]; !exists {
		t.Fatal("well-formed username was not a per-user key")
	}
}

func TestPasswordLimiterIsPerUser(t *testing.T) {
	limiter := NewPasswordLimiter()
	now := time.Now()
	for range 5 {
		if retry := limiter.Allow("user-1", now); retry != 0 {
			t.Fatal(retry)
		}
	}
	if retry := limiter.Allow("user-1", now); retry < 1 {
		t.Fatal(retry)
	}
	if retry := limiter.Allow("user-2", now); retry != 0 {
		t.Fatal(retry)
	}
}
