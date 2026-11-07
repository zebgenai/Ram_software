//go:build !windows

package main

// isWorkstationLocked is Windows-only; elsewhere we report unlocked.
func isWorkstationLocked() bool { return false }
