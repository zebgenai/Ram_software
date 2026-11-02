//go:build !windows && !darwin

package main

// Linux is used only for development; no autostart integration.
func ensureInstalled() error { return nil }
func removeAutostart() error { return nil }
