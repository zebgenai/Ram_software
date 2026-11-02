//go:build darwin

package main

import (
	"os"
	"path/filepath"
)

const launchLabel = "com.remotefiles.agent"

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist"), nil
}

func ensureInstalled() error {
	dst := stableExe()
	if _, err := copySelfTo(dst); err != nil {
		return err
	}
	plistPath, err := launchAgentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchLabel + `</string>
  <key>ProgramArguments</key><array><string>` + dst + `</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
`
	return os.WriteFile(plistPath, []byte(plist), 0o644)
}

func removeAutostart() error {
	if plistPath, err := launchAgentPath(); err == nil {
		os.Remove(plistPath)
	}
	return nil
}
