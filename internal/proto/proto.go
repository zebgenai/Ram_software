// Package proto holds the wire types shared by the server and the agent.
package proto

import (
	"encoding/json"
	"time"
)

const (
	MethodDrives = "drives"
	MethodList   = "list"
	MethodMkdir  = "mkdir"
	MethodRename = "rename"
	MethodMove   = "move"
	MethodCopy   = "copy"
	MethodDelete = "delete"
	MethodFetch  = "fetch" // download: agent pushes file bytes to the server
	MethodStore  = "store" // upload: agent pulls file bytes from the server
	MethodStat   = "stat"  // report whether a file exists and is open/locked on the PC
)

// Request is a command the server queues for an agent.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is what the agent posts back for a Request.
type Response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type Drive struct {
	Path  string `json:"path"`
	Label string `json:"label"`
}

// DrivesResult is what the "drives" method returns: quick-access shortcuts to the
// logged-in user's common folders (Desktop, Downloads, …) plus the real drives.
type DrivesResult struct {
	Shortcuts []Drive `json:"shortcuts,omitempty"`
	Drives    []Drive `json:"drives"`
}

type ListParams struct {
	Path string `json:"path"`
}

type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

type ListResult struct {
	Path      string  `json:"path"`
	Parent    string  `json:"parent"` // empty at a drive root
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

// ---- file operation params (all paths are absolute on the agent) ----

type MkdirParams struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
}

type RenameParams struct {
	Path    string `json:"path"`
	NewName string `json:"newName"`
}

// TransferParams is used by move and copy: a single item into a destination dir.
type TransferParams struct {
	Src    string `json:"src"`
	DstDir string `json:"dstDir"`
}

// StatParams asks the agent about one file.
type StatParams struct {
	Path string `json:"path"`
}

// StatResult reports whether the file exists and whether it's currently open
// (write-locked) by another program on the PC.
type StatResult struct {
	Exists bool `json:"exists"`
	Open   bool `json:"open"`
}

type DeleteParams struct {
	Path string `json:"path"`
}

// FetchParams (download) tells the agent which file to push and under which job.
type FetchParams struct {
	JobID string `json:"jobId"`
	Path  string `json:"path"`
}

// StoreParams (upload) tells the agent to pull bytes for a job and write them.
type StoreParams struct {
	JobID     string `json:"jobId"`
	Dir       string `json:"dir"`
	Name      string `json:"name"`
	Overwrite bool   `json:"overwrite"`
	// BaseMtime, when non-zero, is the file's modification time (Unix seconds)
	// that the edit was based on. If the file on disk has a different mtime, the
	// agent refuses to overwrite (RF_CONFLICT) so concurrent edits aren't lost.
	BaseMtime int64 `json:"baseMtime,omitempty"`
}

// OpResult is returned by mkdir/rename/move/copy/delete/store.
type OpResult struct {
	Path   string `json:"path"`             // resulting path, where applicable
	Backup string `json:"backup,omitempty"` // backup path created, if any
	Bytes  int64  `json:"bytes,omitempty"`  // bytes written, for store
}

type EnrollRequest struct {
	Token        string `json:"token"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Username     string `json:"username"`
	AgentVersion string `json:"agentVersion"`
	Consent      bool   `json:"consent"`
}

type EnrollResponse struct {
	DeviceID string `json:"deviceId"`
	Secret   string `json:"secret"`
}
