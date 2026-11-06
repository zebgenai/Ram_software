package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"remotefiles/internal/proto"
)

// fetchZip streams a folder to the server as a .zip, built on the fly.
func fetchZip(cfg config, p proto.ZipParams) (proto.OpResult, error) {
	root, err := cleanAbs(p.Dir)
	if err != nil {
		notifyBlobError(cfg, p.JobID, err)
		return proto.OpResult{}, err
	}
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		e := errors.New("not a folder")
		notifyBlobError(cfg, p.JobID, e)
		return proto.OpResult{}, e
	}

	pr, pw := io.Pipe()
	go func() {
		zw := zip.NewWriter(pw)
		werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // skip unreadable entries and directories themselves
			}
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return nil
			}
			f, e := os.Open(path)
			if e != nil {
				return nil // skip files we can't read (e.g. open/locked)
			}
			defer f.Close()
			hdr := &zip.FileHeader{Name: filepath.ToSlash(rel), Method: zip.Deflate}
			if info, ie := d.Info(); ie == nil {
				hdr.Modified = info.ModTime()
			}
			zf, e := zw.CreateHeader(hdr)
			if e != nil {
				return e
			}
			_, e = io.Copy(zf, f)
			return e
		})
		if werr == nil {
			werr = zw.Close()
		} else {
			_ = zw.Close()
		}
		_ = pw.CloseWithError(werr)
	}()

	req, _ := http.NewRequest(http.MethodPost, cfg.Server+"/agent/blob/"+p.JobID, pr)
	req.Header.Set("Authorization", authHeader(cfg))
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("X-RF-Name", filepath.Base(root)+".zip")
	resp, err := transferClient.Do(req)
	if err != nil {
		return proto.OpResult{}, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return proto.OpResult{}, fmt.Errorf("server rejected upload (%s)", resp.Status)
	}
	return proto.OpResult{Path: root}, nil
}

// unzip extracts a .zip already on the PC into the folder that contains it.
// It guards against zip-slip (entries escaping the destination).
func unzip(p proto.UnzipParams) (proto.OpResult, error) {
	zipPath, err := cleanAbs(p.Path)
	if err != nil {
		return proto.OpResult{}, err
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return proto.OpResult{}, fmt.Errorf("could not open zip: %w", err)
	}
	defer zr.Close()

	dest := filepath.Clean(filepath.Dir(zipPath))
	prefix := dest + string(os.PathSeparator)
	count := 0
	for _, zf := range zr.File {
		target := filepath.Join(dest, filepath.FromSlash(zf.Name))
		if target != dest && !strings.HasPrefix(target, prefix) {
			return proto.OpResult{}, fmt.Errorf("unsafe path in zip: %s", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return proto.OpResult{}, err
		}
		rc, err := zf.Open()
		if err != nil {
			return proto.OpResult{}, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			rc.Close()
			return proto.OpResult{}, err
		}
		_, err = io.Copy(out, io.LimitReader(rc, 1<<32)) // cap 4 GB per file
		out.Close()
		rc.Close()
		if err != nil {
			return proto.OpResult{}, err
		}
		count++
	}
	return proto.OpResult{Path: dest, Bytes: int64(count)}, nil
}
