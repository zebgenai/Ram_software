package main

import (
	"errors"
	"io"
	"sync"
	"time"
)

// Transfers brokers file bytes between an admin HTTP request and an agent.
// Agents only make outbound requests, so for a download the agent POSTs the
// file to the blob endpoint and for an upload the agent GETs it — in both
// cases the server pairs the two sides by a one-time job id.
type Transfers struct {
	mu sync.Mutex
	m  map[string]*xfer
}

type dlMeta struct {
	name  string
	size  int64
	mtime int64 // file modification time on the PC (Unix seconds), 0 if unknown
	err   error
}

type xfer struct {
	device  string
	kind    string // "dl" (download) or "ul" (upload)
	created time.Time

	// download: agent → admin
	pr   *io.PipeReader
	pw   *io.PipeWriter
	meta chan dlMeta

	// upload: admin → agent
	src     io.Reader
	claimed bool // the agent may pull upload bytes only once
}

const transferConnectTimeout = 30 * time.Second

func NewTransfers() *Transfers {
	t := &Transfers{m: map[string]*xfer{}}
	go t.reap()
	return t
}

func (t *Transfers) reap() {
	for range time.Tick(time.Minute) {
		t.mu.Lock()
		for id, x := range t.m {
			if time.Since(x.created) > 30*time.Minute {
				if x.pw != nil {
					x.pw.CloseWithError(errors.New("transfer expired"))
				}
				delete(t.m, id)
			}
		}
		t.mu.Unlock()
	}
}

func (t *Transfers) add(x *xfer) string {
	id := randHex(16)
	x.created = time.Now()
	t.mu.Lock()
	t.m[id] = x
	t.mu.Unlock()
	return id
}

func (t *Transfers) take(id, device string) (*xfer, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	x, ok := t.m[id]
	if !ok || x.device != device {
		return nil, false
	}
	return x, true
}

func (t *Transfers) remove(id string) {
	t.mu.Lock()
	delete(t.m, id)
	t.mu.Unlock()
}

func (t *Transfers) newDownload(device string) (id string, x *xfer) {
	pr, pw := io.Pipe()
	x = &xfer{device: device, kind: "dl", pr: pr, pw: pw, meta: make(chan dlMeta, 1)}
	return t.add(x), x
}

func (t *Transfers) newUpload(device string, src io.Reader) (id string, x *xfer) {
	x = &xfer{device: device, kind: "ul", src: src}
	return t.add(x), x
}
