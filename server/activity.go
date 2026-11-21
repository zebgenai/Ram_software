package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"remotefiles/internal/proto"
)

// Activity is a per-device, append-only log of "a file was opened/edited at this
// time", reported by the agent. One JSON-lines file per device under
// <dataDir>/activity/<deviceID>.log. Entries older than activityRetention are
// dropped (on compaction and when reading).
type Activity struct {
	mu  sync.Mutex
	dir string
}

const (
	activityRetention = 30 * 24 * time.Hour // keep 30 days of history
	activityMaxFile   = 1 << 20             // compact (purge old) when a device log passes ~1 MB
	activityReadBytes = 2 << 20             // read at most the last 2 MB when listing
)

func OpenActivity(dir string) *Activity {
	d := filepath.Join(dir, "activity")
	_ = os.MkdirAll(d, 0o700)
	return &Activity{dir: d}
}

func (a *Activity) pathFor(deviceID string) string {
	// deviceID is server-generated hex, so it is safe as a file name.
	return filepath.Join(a.dir, deviceID+".log")
}

// Add appends events for a device, then compacts the file if it has grown past
// the size threshold (which also enforces the 30-day retention).
func (a *Activity) Add(deviceID string, events []proto.ActivityEntry) {
	if deviceID == "" || len(events) == 0 {
		return
	}
	p := a.pathFor(deviceID)
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	for _, e := range events {
		if e.Path == "" {
			continue
		}
		if e.At.IsZero() {
			e.At = time.Now().UTC()
		}
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		_, _ = f.Write(append(b, '\n'))
	}
	st, _ := f.Stat()
	f.Close()
	if st != nil && st.Size() > activityMaxFile {
		a.compactLocked(p)
	}
}

// Recent returns up to n newest entries for a device (newest first), dropping any
// older than the retention window and collapsing repeats of the same path to the
// most recent time seen.
func (a *Activity) Recent(deviceID string, n int) []proto.ActivityEntry {
	p := a.pathFor(deviceID)
	a.mu.Lock()
	defer a.mu.Unlock()
	all := a.readLocked(p, true)
	// Collapse to the latest time per path.
	latest := map[string]time.Time{}
	for _, e := range all {
		if t, ok := latest[e.Path]; !ok || e.At.After(t) {
			latest[e.Path] = e.At
		}
	}
	out := make([]proto.ActivityEntry, 0, len(latest))
	for path, at := range latest {
		out = append(out, proto.ActivityEntry{At: at, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// readLocked reads entries from a device log. When tailOnly is set it reads at
// most the last activityReadBytes. Entries older than the retention window are
// skipped. Caller holds a.mu.
func (a *Activity) readLocked(p string, tailOnly bool) []proto.ActivityEntry {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	if tailOnly {
		if st, err := f.Stat(); err == nil && st.Size() > activityReadBytes {
			_, _ = f.Seek(-activityReadBytes, io.SeekEnd)
		}
	}
	cutoff := time.Now().Add(-activityRetention)
	var out []proto.ActivityEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e proto.ActivityEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Path == "" {
			continue
		}
		if e.At.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// compactLocked rewrites a device log keeping only entries within the retention
// window. Caller holds a.mu.
func (a *Activity) compactLocked(p string) {
	kept := a.readLocked(p, false)
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	for _, e := range kept {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		_, _ = f.Write(append(b, '\n'))
	}
	f.Close()
	_ = os.Rename(tmp, p)
}

// Forget removes a device's activity log (called when the device is revoked).
func (a *Activity) Forget(deviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = os.Remove(a.pathFor(deviceID))
}
