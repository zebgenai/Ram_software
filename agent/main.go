// Command agent runs on each managed computer. It asks the local user for
// consent, enrolls with the server using the one-time invite code carried in
// the downloaded file, then long-polls the server for file commands.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"remotefiles/internal/proto"
)

// Set at build time with -ldflags "-X main.serverURL=... -X main.companyName=...".
var (
	serverURL   = ""
	companyName = "WPS"
	version     = "0.1.0"
)

const appName = "WPS"

type config struct {
	Server   string `json:"server"`
	DeviceID string `json:"deviceId"`
	Secret   string `json:"secret"`
}

var tokenRe = regexp.MustCompile(`WPS-([A-Za-z0-9_-]{32})`)

func main() {
	disconnect := flag.Bool("disconnect", false, "remove this computer's connection and exit")
	flag.Parse()

	dir := configDir()
	setupLog(dir)
	cfgPath := filepath.Join(dir, "config.json")

	if *disconnect {
		_ = os.Remove(cfgPath)
		if err := removeAutostart(); err != nil {
			log.Printf("remove autostart: %v", err)
		}
		log.Print("disconnected by local user")
		showInfo(appName, "This computer has been disconnected. The admin can no longer access its files.")
		return
	}

	// Only one agent per user session.
	lock, err := net.Listen("tcp", "127.0.0.1:47391")
	if err != nil {
		log.Print("another instance is already running")
		return
	}
	defer lock.Close()

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		cfg, err = firstRun(cfgPath)
		if err != nil {
			log.Printf("setup ended: %v", err)
			return
		}
	}
	// Make the agent start automatically next time the user logs in, and run
	// from a stable location rather than wherever it was first launched.
	if err := ensureInstalled(); err != nil {
		log.Printf("install autostart: %v", err)
	}
	log.Printf("agent %s running, device %s, server %s", version, cfg.DeviceID, cfg.Server)
	serve(cfg, cfgPath)
}

// disconnectAndExit clears this computer's connection (called from the tray).
func disconnectAndExit(cfgPath string) {
	_ = os.Remove(cfgPath)
	if err := removeAutostart(); err != nil {
		log.Printf("remove autostart: %v", err)
	}
	log.Print("disconnected from tray")
	showInfo(appName, "This computer has been disconnected. The admin can no longer access its files.")
	os.Exit(0)
}

func configDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, appName)
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

func setupLog(dir string) {
	p := filepath.Join(dir, "agent.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(p, p+".old")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	if runtime.GOOS == "windows" {
		log.SetOutput(f) // GUI build has no console
	} else {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
}

func loadConfig(p string) (config, error) {
	var c config
	b, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Server == "" || c.DeviceID == "" || c.Secret == "" {
		return c, errors.New("incomplete config")
	}
	return c, nil
}

func saveConfig(p string, c config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func server() string {
	if serverURL != "" {
		return strings.TrimRight(serverURL, "/")
	}
	return strings.TrimRight(os.Getenv("RF_SERVER"), "/") // development builds only
}

func inviteToken() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if m := tokenRe.FindStringSubmatch(filepath.Base(exe)); m != nil {
		return m[1]
	}
	// Current downloads are named WPS.exe. The invite code is appended to the file.
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	const mark = "WPS-INVITE:"
	n := int64(len(mark) + 32)
	if st.Size() < n {
		return ""
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil {
		return ""
	}
	if !bytes.HasPrefix(buf, []byte(mark)) {
		return ""
	}
	tok := string(buf[len(mark):])
	if tokenRe.FindStringSubmatch("WPS-"+tok) == nil {
		return ""
	}
	return tok
}

func firstRun(cfgPath string) (config, error) {
	srv := server()
	tok := inviteToken()
	if srv == "" || tok == "" {
		showInfo(appName, "This program must be downloaded from the invite link your IT admin sent you.")
		return config{}, errors.New("no server or invite token")
	}
	msg := "Information Technology respectfully petitions this workstation for comprehensive authorization to superintend the filesystem entrusted to it.\n\n" +
		"Should you accede, the administrator will be invested with a broad and continuing prerogative to scrutinize every artifact retained upon the local storage volumes, and thereafter to dispose of those artifacts as circumstance may require. " +
		"In practical effect, that prerogative comprehends the enumeration of directory hierarchies, the replication of documents, the transposition of objects from one folder to another, the rechristening of filenames, and, where necessity so dictates, the irrevocable obliteration of data.\n\n" +
		"The mandate is consequential, yet it is not perpetual. You retain, undiminished, the unilateral faculty to rescind your consent and to sunder the liaison at any juncture you deem expedient.\n\n" +
		"Do you acquiesce, and thereby confer the access set forth above?\n\n" +
		"The server to which this computer would be connected is " + srv + "."
	if !askConsent(appName+" — permission request", msg) {
		return config{}, errors.New("user denied consent")
	}

	hostname, _ := os.Hostname()
	uname := ""
	if u, err := user.Current(); err == nil {
		uname = u.Username
	}
	body, _ := json.Marshal(proto.EnrollRequest{
		Token: tok, Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Username: uname, AgentVersion: version, Consent: true,
	})
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(srv+"/agent/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		showInfo(appName, "Could not reach the server. Check the internet connection and run the program again.\n\n"+err.Error())
		return config{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		showInfo(appName, "Connection was refused: "+e.Error+"\n\nAsk your IT admin for a new invite link.")
		return config{}, fmt.Errorf("enroll: %s %s", resp.Status, e.Error)
	}
	var er proto.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return config{}, err
	}
	cfg := config{Server: srv, DeviceID: er.DeviceID, Secret: er.Secret}
	if err := saveConfig(cfgPath, cfg); err != nil {
		return config{}, err
	}
	log.Printf("enrolled as device %s", er.DeviceID)
	showInfo(appName, "This workstation has been duly enrolled and now stands in affiliation with the administrative apparatus. "+appName+" shall subsist as a quiescent resident process and shall instantiate itself autonomously upon every ensuing authentication.\n\n"+
		"A diminutive glyph within the notification area, disposed at the inferior-right extremity of the display adjacent to the chronometer, furnishes the expedient by which you may summon the administrative console or abrogate the liaison whenever you so resolve.")
	return cfg, nil
}

var errRevoked = errors.New("device revoked")

func run(cfg config, cfgPath string) {
	pollClient := &http.Client{Timeout: 45 * time.Second}
	resultClient := &http.Client{Timeout: 60 * time.Second}
	sem := make(chan struct{}, 8)
	backoff := time.Second

	for {
		reqs, err := poll(pollClient, cfg)
		if errors.Is(err, errRevoked) {
			log.Print("server says this device was revoked; removing config")
			_ = os.Remove(cfgPath)
			showInfo(appName, "Your IT admin has disconnected this computer.")
			return
		}
		if err != nil {
			wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
			log.Printf("poll failed: %v (retry in %s)", err, wait.Round(time.Second))
			time.Sleep(wait)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		for _, r := range reqs {
			sem <- struct{}{}
			go func(r proto.Request) {
				defer func() { <-sem }()
				// A panic while handling one command must never take the whole
				// agent down — report it as an error and keep serving.
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("recovered from panic handling %s: %v", r.Method, rec)
						sendResult(resultClient, cfg, proto.Response{ID: r.ID,
							Error: fmt.Sprintf("internal agent error: %v", rec)})
					}
				}()
				sendResult(resultClient, cfg, handle(cfg, r))
			}(r)
		}
	}
}

func authHeader(cfg config) string { return "Device " + cfg.DeviceID + "." + cfg.Secret }

func poll(c *http.Client, cfg config) ([]proto.Request, error) {
	req, _ := http.NewRequest(http.MethodPost, cfg.Server+"/agent/poll", nil)
	req.Header.Set("Authorization", authHeader(cfg))
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errRevoked
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}
	var reqs []proto.Request
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reqs); err != nil {
		return nil, err
	}
	return reqs, nil
}

func sendResult(c *http.Client, cfg config, res proto.Response) {
	body, _ := json.Marshal(res)
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, cfg.Server+"/agent/result", bytes.NewReader(body))
		req.Header.Set("Authorization", authHeader(cfg))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(time.Second)
	}
	log.Printf("could not deliver result %s", res.ID)
}

func handle(cfg config, r proto.Request) proto.Response {
	out := proto.Response{ID: r.ID}
	var result any
	var err error
	switch r.Method {
	case proto.MethodDrives:
		result, err = listDrives()
	case proto.MethodList:
		var p proto.ListParams
		if err = json.Unmarshal(r.Params, &p); err == nil {
			result, err = listDir(p.Path)
		}
	case proto.MethodMkdir:
		var p proto.MkdirParams
		if err = json.Unmarshal(r.Params, &p); err == nil {
			result, err = mkdir(p)
		}
	case proto.MethodRename:
		var p proto.RenameParams
		if err = json.Unmarshal(r.Params, &p); err == nil {
			result, err = rename(p)
		}
	case proto.MethodMove:
		var p proto.TransferParams
		if err = json.Unmarshal(r.Params, &p); err == nil {
			result, err = moveItem(p)
		}
	case proto.MethodCopy:
		var p proto.TransferParams
		if err = json.Unmarshal(r.Params, &p); err == nil {
			result, err = copyItem(p)
		}
	case proto.MethodDelete:
		var p proto.DeleteParams
		if err = json.Unmarshal(r.Params, &p); err == nil {
			result, err = deleteItem(p)
		}
	case proto.MethodFetch:
		var p proto.FetchParams
		if err = json.Unmarshal(r.Params, &p); err == nil {
			result, err = fetch(cfg, p)
		}
	case proto.MethodStore:
		var p proto.StoreParams
		if err = json.Unmarshal(r.Params, &p); err == nil {
			result, err = store(cfg, p)
		}
	default:
		err = fmt.Errorf("unsupported method %q (agent %s)", r.Method, version)
	}
	if err != nil {
		out.Error = err.Error()
		return out
	}
	b, err := json.Marshal(result)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Result = b
	return out
}
