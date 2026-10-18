package auth

import (
	"container/list"
	"math"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// LoginLimiter applies both buckets on every attempt: per-IP 30/minute (burst
// 30), and per (IP, case-folded username) 5/minute (burst 5). The combined table
// is capped at 10000 buckets with least-recently-used eviction.
type LoginLimiter struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	lru      *list.List
	capacity int
}

type loginBucket struct {
	key     string
	limiter *rate.Limiter
}

func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{entries: make(map[string]*list.Element), lru: list.New(), capacity: 10000}
}

// Allow returns the Retry-After delay in whole seconds, or zero when allowed.
// Only usernames matching the account contract become per-user keys. Every other
// value, including multi-megabyte bodies, shares one fixed key per IP. When the
// IP bucket already denies, the per-user bucket is not created.
func (l *LoginLimiter) Allow(ip, username string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	ipBucket := l.bucket("ip:"+ip, rate.Every(2*time.Second), 30)
	if !ipBucket.AllowN(now, 1) {
		return retryAfter(ipBucket, now)
	}
	userBucket := l.bucket(userLimitKey(ip, username), rate.Every(12*time.Second), 5)
	if !userBucket.AllowN(now, 1) {
		return retryAfter(userBucket, now)
	}
	return 0
}

func userLimitKey(ip, username string) string {
	if len(username) >= 3 && len(username) <= 64 && usernamePattern.MatchString(username) {
		return "user:" + ip + "\x00" + strings.ToLower(username)
	}
	return "user:" + ip + "\x00*"
}

func retryAfter(limiter *rate.Limiter, now time.Time) int {
	delay := (1 - limiter.TokensAt(now)) / float64(limiter.Limit())
	return max(1, int(math.Ceil(delay)))
}

// PasswordLimiter bounds current-password verification on PUT /auth/password.
// Five attempts per minute per user ID, burst 5, with a fixed-size LRU of 10000.
type PasswordLimiter struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	lru      *list.List
	capacity int
}

func NewPasswordLimiter() *PasswordLimiter {
	return &PasswordLimiter{entries: make(map[string]*list.Element), lru: list.New(), capacity: 10000}
}

// Allow returns the Retry-After delay in whole seconds, or zero when allowed.
func (l *PasswordLimiter) Allow(userID string, now time.Time) int {
	if userID == "" {
		userID = "*"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.bucket("pw:" + userID)
	if bucket.AllowN(now, 1) {
		return 0
	}
	return retryAfter(bucket, now)
}

func (l *PasswordLimiter) bucket(key string) *rate.Limiter {
	if element, exists := l.entries[key]; exists {
		l.lru.MoveToFront(element)
		return loginBucketAt(element).limiter
	}
	if len(l.entries) >= l.capacity {
		oldest := l.lru.Back()
		delete(l.entries, loginBucketAt(oldest).key)
		l.lru.Remove(oldest)
	}
	bucket := loginBucket{key: key, limiter: rate.NewLimiter(rate.Every(12*time.Second), 5)}
	l.entries[key] = l.lru.PushFront(bucket)
	return bucket.limiter
}

func (l *LoginLimiter) bucket(key string, limit rate.Limit, burst int) *rate.Limiter {
	if element, exists := l.entries[key]; exists {
		l.lru.MoveToFront(element)
		return loginBucketAt(element).limiter
	}
	if len(l.entries) >= l.capacity {
		oldest := l.lru.Back()
		delete(l.entries, loginBucketAt(oldest).key)
		l.lru.Remove(oldest)
	}
	bucket := loginBucket{key: key, limiter: rate.NewLimiter(limit, burst)}
	l.entries[key] = l.lru.PushFront(bucket)
	return bucket.limiter
}

func loginBucketAt(element *list.Element) loginBucket {
	bucket, ok := element.Value.(loginBucket)
	if !ok {
		panic("invalid login limiter bucket")
	}
	return bucket
}
