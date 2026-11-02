package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"remotefiles/internal/proto"
)

// Hub relays commands from the dashboard to agents. Agents long-poll for
// work over plain HTTPS, so no extra ports or WebSocket support are needed
// and it works through office proxies and firewalls.
type Hub struct {
	mu       sync.Mutex
	queues   map[string]chan proto.Request
	pending  map[string]pendingCall
	lastPoll map[string]time.Time
	polling  map[string]int
}

type pendingCall struct {
	device string
	ch     chan proto.Response
}

const (
	pollHold     = 25 * time.Second // how long the server holds an agent poll open
	onlineWindow = 45 * time.Second // a device is "online" if it polled this recently
	callTimeout  = 30 * time.Second
)

var (
	errOffline = errors.New("device is offline")
	errBusy    = errors.New("device has too many pending requests, try again")
	errTimeout = errors.New("device did not respond in time")
)

func NewHub() *Hub {
	return &Hub{
		queues:   map[string]chan proto.Request{},
		pending:  map[string]pendingCall{},
		lastPoll: map[string]time.Time{},
		polling:  map[string]int{},
	}
}

func (h *Hub) queueLocked(dev string) chan proto.Request {
	q, ok := h.queues[dev]
	if !ok {
		q = make(chan proto.Request, 64)
		h.queues[dev] = q
	}
	return q
}

func (h *Hub) Online(dev string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.polling[dev] > 0 || time.Since(h.lastPoll[dev]) < onlineWindow
}

// Forget drops all state for a revoked device.
func (h *Hub) Forget(dev string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.queues, dev)
	delete(h.lastPoll, dev)
	for id, p := range h.pending {
		if p.device == dev {
			close(p.ch)
			delete(h.pending, id)
		}
	}
}

// Call sends a command to a device and waits for its answer.
func (h *Hub) Call(ctx context.Context, dev, method string, params any) (json.RawMessage, error) {
	if !h.Online(dev) {
		return nil, errOffline
	}
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	req := proto.Request{ID: randHex(16), Method: method, Params: raw}
	ch := make(chan proto.Response, 1)

	h.mu.Lock()
	h.pending[req.ID] = pendingCall{device: dev, ch: ch}
	q := h.queueLocked(dev)
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
	}()

	select {
	case q <- req:
	default:
		return nil, errBusy
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errOffline
		}
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, errTimeout
	}
}

// Send enqueues a command for a device without waiting for a response. Used by
// file transfers, where the result arrives out of band on the blob endpoint.
func (h *Hub) Send(dev string, req proto.Request) error {
	if !h.Online(dev) {
		return errOffline
	}
	h.mu.Lock()
	q := h.queueLocked(dev)
	h.mu.Unlock()
	select {
	case q <- req:
		return nil
	default:
		return errBusy
	}
}

// Poll blocks until work is available for the device or pollHold elapses.
func (h *Hub) Poll(ctx context.Context, dev string) []proto.Request {
	h.mu.Lock()
	q := h.queueLocked(dev)
	h.polling[dev]++
	h.lastPoll[dev] = time.Now()
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.polling[dev]--
		h.lastPoll[dev] = time.Now()
		h.mu.Unlock()
	}()

	t := time.NewTimer(pollHold)
	defer t.Stop()
	var out []proto.Request
	select {
	case r := <-q:
		out = append(out, r)
	case <-t.C:
		return []proto.Request{}
	case <-ctx.Done():
		return []proto.Request{}
	}
	for len(out) < 16 {
		select {
		case r := <-q:
			out = append(out, r)
		default:
			return out
		}
	}
	return out
}

// Deliver routes an agent's answer to the waiting caller. A device may only
// answer requests that were sent to it.
func (h *Hub) Deliver(dev string, resp proto.Response) {
	// Send while holding the lock so Forget can't close the channel under us.
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pending[resp.ID]
	if !ok || p.device != dev {
		return
	}
	select {
	case p.ch <- resp:
	default:
	}
}
