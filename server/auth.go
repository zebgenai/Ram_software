package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func randHex(n int) string   { return hex.EncodeToString(randBytes(n)) }
func randToken(n int) string { return base64.RawURLEncoding.EncodeToString(randBytes(n)) }
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---- password hashing: pbkdf2-sha256$<iter>$<salt>$<hash> ----

const pbkdf2Iter = 600_000

func hashPassword(pw string) (string, error) {
	salt := randBytes(16)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func checkPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 100_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---- admin sessions (in memory; a server restart logs admins out) ----

const (
	sessionCookie = "rf_session"
	sessionTTL    = 12 * time.Hour
)

type Sessions struct {
	mu sync.Mutex
	m  map[string]session // keyed by sha256 of the cookie value
}

type session struct {
	user    string
	role    string
	expires time.Time
}

func NewSessions() *Sessions { return &Sessions{m: map[string]session{}} }

func (s *Sessions) Create(user, role string) string {
	tok := randToken(32)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.expires) {
			delete(s.m, k)
		}
	}
	s.m[sha256Hex(tok)] = session{user: user, role: role, expires: now.Add(sessionTTL)}
	return tok
}

func (s *Sessions) Get(tok string) (user, role string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, found := s.m[sha256Hex(tok)]
	if !found || time.Now().After(v.expires) {
		return "", "", false
	}
	return v.user, v.role, true
}

// RevokeUser drops all sessions belonging to a user (after disable/delete/role change).
func (s *Sessions) RevokeUser(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if v.user == user {
			delete(s.m, k)
		}
	}
}

func (s *Sessions) Delete(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, sha256Hex(tok))
}

// ---- login rate limiting: 5 failures per IP per 15 minutes ----

type Limiter struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func NewLimiter() *Limiter { return &Limiter{m: map[string][]time.Time{}} }

const (
	loginMaxFails = 5
	loginWindow   = 15 * time.Minute
)

func (l *Limiter) Blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip)) >= loginMaxFails
}

func (l *Limiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[ip] = append(l.prune(ip), time.Now())
}

func (l *Limiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, ip)
}

func (l *Limiter) prune(ip string) []time.Time {
	var keep []time.Time
	for _, t := range l.m[ip] {
		if time.Since(t) < loginWindow {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.m, ip)
	} else {
		l.m[ip] = keep
	}
	return keep
}

// ---- device auth: "Authorization: Device <id>.<secret>" ----

var errDeviceAuth = errors.New("device authentication failed")

func parseDeviceAuth(r *http.Request) (id, secret string, err error) {
	h := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(h, "Device ")
	if !ok {
		return "", "", errDeviceAuth
	}
	id, secret, ok = strings.Cut(rest, ".")
	if !ok || id == "" || secret == "" {
		return "", "", errDeviceAuth
	}
	return id, secret, nil
}
