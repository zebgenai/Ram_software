package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PendingSaves holds edited spreadsheet bytes that couldn't be written back to a
// PC because the file was open there. The bytes live on the SERVER (under
// DataDir/pending) — never on the user's computer — and a background loop writes
// them to the original file as soon as the PC releases it. Repeated saves while
// the file stays open keep only the newest version (keyed by device+path).
type PendingSaves struct {
	dir string
	mu  sync.Mutex
}

type pendingMeta struct {
	Device    string    `json:"device"`
	Dir       string    `json:"dir"`
	Name      string    `json:"name"`
	Full      string    `json:"full"`
	BaseMtime int64     `json:"baseMtime,omitempty"`
	Created   time.Time `json:"created"`
}

type pendingItem struct {
	meta pendingMeta
	data []byte
}

func NewPendingSaves(dataDir string) *PendingSaves {
	d := filepath.Join(dataDir, "pending")
	_ = os.MkdirAll(d, 0o700)
	return &PendingSaves{dir: d}
}

func pendingKey(device, full string) string {
	sum := sha256.Sum256([]byte(device + "|" + full))
	return hex.EncodeToString(sum[:])
}

func (p *PendingSaves) datPath(key string) string  { return filepath.Join(p.dir, key+".dat") }
func (p *PendingSaves) metaPath(key string) string { return filepath.Join(p.dir, key+".json") }

func pendingWriteAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// put stores or replaces the latest edited bytes for a file.
func (p *PendingSaves) put(m pendingMeta, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := pendingKey(m.Device, m.Full)
	if err := pendingWriteAtomic(p.datPath(key), data); err != nil {
		return err
	}
	mb, _ := json.Marshal(m)
	return pendingWriteAtomic(p.metaPath(key), mb)
}

func (p *PendingSaves) remove(device, full string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := pendingKey(device, full)
	os.Remove(p.datPath(key))
	os.Remove(p.metaPath(key))
}

// putConflict stashes an edit that couldn't be applied because the file changed
// on the PC. It goes to a "conflicts" subfolder (never retried), timestamped so
// multiple conflicting versions are all kept for an admin to review and merge.
// Nothing is written to the user's computer.
func (p *PendingSaves) putConflict(m pendingMeta, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cdir := filepath.Join(p.dir, "conflicts")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	base := pendingKey(m.Device, m.Full) + "-" + stamp
	if err := pendingWriteAtomic(filepath.Join(cdir, base+".dat"), data); err != nil {
		return err
	}
	mb, _ := json.Marshal(m)
	return pendingWriteAtomic(filepath.Join(cdir, base+".json"), mb)
}

// all returns every queued save (metadata + bytes). The conflicts subfolder is
// skipped (its entries are directories to all()'s view and never retried).
func (p *PendingSaves) all() []pendingItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries, _ := os.ReadDir(p.dir)
	var out []pendingItem
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		mb, err := os.ReadFile(filepath.Join(p.dir, e.Name()))
		if err != nil {
			continue
		}
		var m pendingMeta
		if json.Unmarshal(mb, &m) != nil {
			continue
		}
		data, err := os.ReadFile(p.datPath(pendingKey(m.Device, m.Full)))
		if err != nil {
			continue
		}
		out = append(out, pendingItem{meta: m, data: data})
	}
	return out
}
