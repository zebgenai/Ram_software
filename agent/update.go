package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// sendEvent reports a short status event to the server (best-effort).
func sendEvent(cfg config, event string) {
	body, _ := json.Marshal(map[string]string{"event": event})
	req, _ := http.NewRequest(http.MethodPost, cfg.Server+"/agent/event", bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader(cfg))
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 10 * time.Second}
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
}

// platformKey maps this build's OS/arch to the server's agent-platform key.
func platformKey() string {
	switch {
	case runtime.GOOS == "windows":
		return "windows"
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "mac-arm64"
	case runtime.GOOS == "darwin":
		return "mac-intel"
	}
	return ""
}

// cleanupOldBinary removes the ".old" file left by a previous self-update.
func cleanupOldBinary() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// updateLoop periodically checks the server for a newer agent build and updates.
func updateLoop(cfg config) {
	time.Sleep(30 * time.Second) // let startup settle
	for {
		checkUpdate(cfg)
		time.Sleep(6 * time.Hour)
	}
}

// checkUpdate asks the server for the latest build id and self-updates if it
// differs from this agent's own.
func checkUpdate(cfg config) {
	plat := platformKey()
	if plat == "" {
		return
	}
	c := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, cfg.Server+"/agent/version", nil)
	req.Header.Set("Authorization", authHeader(cfg))
	resp, err := c.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return
	}
	if v.Version == "" || v.Version == version {
		return // already up to date (or server has no build id)
	}
	log.Printf("update available: %s -> %s", version, v.Version)
	if err := selfUpdate(cfg, plat); err != nil {
		log.Printf("self-update failed: %v", err)
	}
}

// selfUpdate downloads the current agent binary and replaces this executable,
// then starts the new one and exits.
func selfUpdate(cfg config, plat string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: 5 * time.Minute}
	req, _ := http.NewRequest(http.MethodGet, cfg.Server+"/agent/update/"+plat, nil)
	req.Header.Set("Authorization", authHeader(cfg))
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}

	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil || n < 1<<20 { // sanity: a real agent binary is several MB
		os.Remove(tmp)
		return fmt.Errorf("incomplete download (%d bytes): %v", n, err)
	}

	// Replace the running executable: rename self -> .old, move .new -> self.
	// Windows allows renaming a running exe; the old file is removed next start.
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(old, exe) // roll back
		return err
	}

	// Release the single-instance lock so the new process can take it, then
	// start the new binary and exit.
	if instanceLock != nil {
		_ = instanceLock.Close()
	}
	if err := exec.Command(exe).Start(); err != nil {
		return err
	}
	log.Printf("updated to new build; restarting")
	os.Exit(0)
	return nil
}
