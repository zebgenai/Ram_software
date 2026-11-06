package main

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"remotefiles/internal/proto"
)

// searchDir walks Dir and returns files/folders whose name contains Query
// (case-insensitive), bounded by Max results and a time limit.
func searchDir(p proto.SearchParams) (proto.SearchResult, error) {
	root, err := cleanAbs(p.Dir)
	if err != nil {
		return proto.SearchResult{}, err
	}
	q := strings.ToLower(strings.TrimSpace(p.Query))
	if q == "" {
		return proto.SearchResult{}, errors.New("search text required")
	}
	max := p.Max
	if max <= 0 || max > 1000 {
		max = 500
	}
	deadline := time.Now().Add(20 * time.Second)
	var out []proto.Entry
	truncated := false

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries/folders
		}
		if len(out) >= max || time.Now().After(deadline) {
			truncated = true
			return filepath.SkipAll
		}
		if strings.Contains(strings.ToLower(d.Name()), q) {
			e := proto.Entry{Name: d.Name(), Path: path, Dir: d.IsDir()}
			if info, ierr := d.Info(); ierr == nil {
				e.Size = info.Size()
				e.ModTime = info.ModTime()
			}
			out = append(out, e)
		}
		return nil
	})
	return proto.SearchResult{Entries: out, Truncated: truncated}, nil
}
