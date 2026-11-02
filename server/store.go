package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Device struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	Username     string    `json:"username"`
	AgentVersion string    `json:"agentVersion"`
	SecretHash   string    `json:"secretHash"`
	EnrolledAt   time.Time `json:"enrolledAt"`
	LastSeen     time.Time `json:"lastSeen"`
	LastIP       string    `json:"lastIp"`
}

type Invite struct {
	TokenHash string     `json:"tokenHash"`
	Note      string     `json:"note"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	DeviceID  string     `json:"deviceId,omitempty"`
}

type storeData struct {
	Devices map[string]*Device `json:"devices"`
	Invites map[string]*Invite `json:"invites"` // keyed by token hash
	Users   map[string]*User   `json:"users"`   // keyed by lowercase username
}

// Store is a small JSON-file database. For a handful of devices this is
// simpler and safer to back up than a separate database server.
type Store struct {
	mu   sync.Mutex
	path string
	data storeData
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "store.json")}
	s.data = storeData{Devices: map[string]*Device{}, Invites: map[string]*Invite{}, Users: map[string]*User{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.saveLocked()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	if s.data.Devices == nil {
		s.data.Devices = map[string]*Device{}
	}
	if s.data.Invites == nil {
		s.data.Invites = map[string]*Invite{}
	}
	if s.data.Users == nil {
		s.data.Users = map[string]*User{}
	}
	return s, nil
}

// saveLocked writes atomically (temp file + rename). Caller holds mu.
func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.data.Devices))
	for _, d := range s.data.Devices {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EnrolledAt.Before(out[j].EnrolledAt) })
	return out
}

func (s *Store) Device(id string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.Devices[id]
	if !ok {
		return Device{}, false
	}
	return *d, true
}

func (s *Store) DeleteDevice(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Devices[id]; !ok {
		return false, nil
	}
	delete(s.data.Devices, id)
	return true, s.saveLocked()
}

func (s *Store) RenameDevice(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.Devices[id]
	if !ok {
		return errNotFound
	}
	d.Name = name
	return s.saveLocked()
}

// TouchDevice records activity. It only writes to disk once a minute per
// device so that long-polling doesn't hammer the disk.
func (s *Store) TouchDevice(id, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.Devices[id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	persist := now.Sub(d.LastSeen) > time.Minute || d.LastIP != ip
	d.LastSeen = now
	d.LastIP = ip
	if persist {
		_ = s.saveLocked()
	}
}

func (s *Store) AddInvite(inv Invite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Drop invites that expired more than a week ago.
	for k, v := range s.data.Invites {
		if time.Since(v.ExpiresAt) > 7*24*time.Hour {
			delete(s.data.Invites, k)
		}
	}
	s.data.Invites[inv.TokenHash] = &inv
	return s.saveLocked()
}

func (s *Store) Invites() []Invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Invite, 0, len(s.data.Invites))
	for _, v := range s.data.Invites {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Store) DeleteInvite(tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Invites, tokenHash)
	return s.saveLocked()
}

// ValidInvite reports whether an invite exists, is unused and unexpired.
func (s *Store) ValidInvite(tokenHash string) (Invite, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.data.Invites[tokenHash]
	if !ok || inv.UsedAt != nil || time.Now().After(inv.ExpiresAt) {
		return Invite{}, false
	}
	return *inv, true
}

// Enroll atomically consumes an invite and creates the device.
func (s *Store) Enroll(tokenHash string, d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.data.Invites[tokenHash]
	if !ok || inv.UsedAt != nil || time.Now().After(inv.ExpiresAt) {
		return errInvalidInvite
	}
	now := time.Now().UTC()
	inv.UsedAt = &now
	inv.DeviceID = d.ID
	if d.Name == "" && inv.Note != "" {
		d.Name = inv.Note
	}
	s.data.Devices[d.ID] = &d
	return s.saveLocked()
}

var (
	errNotFound      = errors.New("not found")
	errInvalidInvite = errors.New("invite link is invalid, expired or already used")
)

// ---- audit log (append-only JSON lines) ----

type AuditEntry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	IP     string    `json:"ip"`
	Action string    `json:"action"`
	Device string    `json:"device,omitempty"`
	Path   string    `json:"path,omitempty"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
}

type Audit struct {
	mu   sync.Mutex
	path string
}

func OpenAudit(dir string) *Audit { return &Audit{path: filepath.Join(dir, "audit.log")} }

func (a *Audit) Log(e AuditEntry) {
	e.Time = time.Now().UTC()
	b, _ := json.Marshal(e)
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// Recent returns up to n newest entries (newest first), reading at most
// the last 2 MB of the log.
func (a *Audit) Recent(n int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.Open(a.path)
	if err != nil {
		return []AuditEntry{}
	}
	defer f.Close()
	const window = 2 << 20
	if st, err := f.Stat(); err == nil && st.Size() > window {
		_, _ = f.Seek(-window, io.SeekEnd)
	}
	var all []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	out := make([]AuditEntry, 0, n)
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out
}
