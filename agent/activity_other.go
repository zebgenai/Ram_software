//go:build !windows

package main

import "remotefiles/internal/proto"

// recentActivity has no implementation off Windows (the managed PCs are Windows).
func recentActivity() []proto.ActivityEntry { return nil }
