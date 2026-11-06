package main

import (
	"os"
	"path/filepath"

	"remotefiles/internal/proto"
)

// knownFolders returns quick-access shortcuts to the logged-in user's common
// folders (only the ones that actually exist), so the dashboard can show them
// without drilling down through C:\Users\<name>\...
func knownFolders() []proto.Drive {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	names := []string{"Desktop", "Downloads", "Documents", "Pictures", "Videos", "Music"}
	var out []proto.Drive
	for _, n := range names {
		p := filepath.Join(home, n)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, proto.Drive{Path: p, Label: n})
		}
	}
	return out
}
