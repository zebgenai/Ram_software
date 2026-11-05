package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"remotefiles/internal/proto"
)

// transferClient has no overall timeout: a big file may take a while. The
// server closes the connection if the peer side goes away.
var transferClient = &http.Client{}

// fetch (download) reads a file on this PC and pushes its bytes to the server,
// which streams them on to the waiting admin. The server pairs the stream by jobId.
func fetch(cfg config, p proto.FetchParams) (proto.OpResult, error) {
	path, err := cleanAbs(p.Path)
	if err != nil {
		notifyBlobError(cfg, p.JobID, err)
		return proto.OpResult{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		e := friendlyErr(err)
		notifyBlobError(cfg, p.JobID, e)
		return proto.OpResult{}, e
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		e := errors.New("not a file")
		notifyBlobError(cfg, p.JobID, e)
		return proto.OpResult{}, e
	}

	req, _ := http.NewRequest(http.MethodPost, cfg.Server+"/agent/blob/"+p.JobID, f)
	req.Header.Set("Authorization", authHeader(cfg))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-RF-Name", filepath.Base(path))
	req.Header.Set("X-RF-Mtime", strconv.FormatInt(st.ModTime().Unix(), 10))
	req.ContentLength = st.Size()
	resp, err := transferClient.Do(req)
	if err != nil {
		return proto.OpResult{}, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return proto.OpResult{}, fmt.Errorf("server rejected upload (%s)", resp.Status)
	}
	return proto.OpResult{Path: path, Bytes: st.Size()}, nil
}

// notifyBlobError tells the server the download failed so the admin's request
// unblocks with an error instead of hanging.
func notifyBlobError(cfg config, jobID string, e error) {
	req, _ := http.NewRequest(http.MethodPost, cfg.Server+"/agent/blob/"+jobID, nil)
	req.Header.Set("Authorization", authHeader(cfg))
	req.Header.Set("X-RF-Error", e.Error())
	if resp, err := transferClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// errFileLocked signals that the destination exists but can't be written because
// it is open in another program on the PC (e.g. Excel/WPS hold an exclusive
// lock). The server recognises this marker and queues the save for a retry.
var errFileLocked = errors.New("RF_FILE_LOCKED: the file is open on the PC")

// errConflict signals that the file on disk changed since the edit started
// (someone saved different changes on the PC), so overwriting would lose them.
var errConflict = errors.New("RF_CONFLICT: the file changed on the PC since editing started")

// isWriteLocked reports whether an existing file can't be opened for writing
// (because another program holds it open). A missing file is not locked.
func isWriteLocked(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return !os.IsNotExist(err)
	}
	f.Close()
	return false
}

// store (upload) pulls bytes for a job from the server and writes them to disk,
// backing up any existing file first.
func store(cfg config, p proto.StoreParams) (proto.OpResult, error) {
	if err := validName(p.Name); err != nil {
		return proto.OpResult{}, err
	}
	dir, err := cleanAbs(p.Dir)
	if err != nil {
		return proto.OpResult{}, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return proto.OpResult{}, errors.New("destination folder not found")
	}
	dst := filepath.Join(dir, p.Name)
	if _, err := os.Lstat(dst); err == nil && !p.Overwrite {
		return proto.OpResult{}, errors.New("a file with that name already exists")
	}
	// Fail fast (before pulling bytes) if the target is open on the PC, so the
	// server can hold the edit and retry once the user closes the file.
	if p.Overwrite && isWriteLocked(dst) {
		return proto.OpResult{}, errFileLocked
	}
	// If the file changed on disk since the edit started, don't overwrite — the
	// server stashes this version instead, so concurrent PC edits aren't lost.
	if p.Overwrite && p.BaseMtime > 0 {
		if st, err := os.Stat(dst); err == nil && st.ModTime().Unix() != p.BaseMtime {
			return proto.OpResult{}, errConflict
		}
	}

	req, _ := http.NewRequest(http.MethodGet, cfg.Server+"/agent/blob/"+p.JobID, nil)
	req.Header.Set("Authorization", authHeader(cfg))
	resp, err := transferClient.Do(req)
	if err != nil {
		return proto.OpResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return proto.OpResult{}, fmt.Errorf("could not get upload data (%s)", resp.Status)
	}

	// Write to a temp file first; only touch the real file once it's complete.
	tmp := filepath.Join(dir, "."+p.Name+".rf-upload")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return proto.OpResult{}, friendlyErr(err)
	}
	n, err := io.Copy(out, resp.Body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return proto.OpResult{}, fmt.Errorf("transfer interrupted: %w", err)
	}

	backup, err := backupExisting(dst)
	if err != nil {
		os.Remove(tmp)
		if isWriteLocked(dst) { // opened between the pre-check and now
			return proto.OpResult{}, errFileLocked
		}
		return proto.OpResult{}, fmt.Errorf("could not back up the existing file: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		if isWriteLocked(dst) {
			return proto.OpResult{}, errFileLocked
		}
		return proto.OpResult{}, fmt.Errorf("could not save the file (it may be open on the PC): %w", err)
	}
	return proto.OpResult{Path: dst, Backup: backup, Bytes: n}, nil
}
