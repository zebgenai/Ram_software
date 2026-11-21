package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"remotefiles/internal/proto"
)

// activityLoop periodically samples the OS's recently-opened-files list and
// reports NEW entries to the server (log-on-change: a path is sent only when its
// "opened" time is newer than the last time we reported it). This is the light
// history feed; the accurate "what's open right now" is a separate on-demand
// command. recentActivity() is implemented per-platform (Windows reads the
// Recent shortcuts folder; other OSes return nothing).
func activityLoop(cfg config) {
	const interval = 60 * time.Second
	client := &http.Client{Timeout: 20 * time.Second}
	lastSent := map[string]time.Time{} // newest reported time per path
	for {
		var fresh []proto.ActivityEntry
		for _, e := range recentActivity() {
			if e.Path == "" {
				continue
			}
			if t, ok := lastSent[e.Path]; ok && !e.At.After(t) {
				continue
			}
			lastSent[e.Path] = e.At
			fresh = append(fresh, e)
		}
		if len(fresh) > 0 {
			sendActivity(client, cfg, fresh)
		}
		if len(lastSent) > 5000 { // keep the dedupe map bounded
			lastSent = map[string]time.Time{}
		}
		time.Sleep(interval)
	}
}

func sendActivity(c *http.Client, cfg config, events []proto.ActivityEntry) {
	body, _ := json.Marshal(proto.ActivityReport{Events: events})
	req, _ := http.NewRequest(http.MethodPost, cfg.Server+"/agent/activity", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(cfg))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
}
