// Command server is the central relay + admin dashboard for Remote Files.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is the build id, baked at build time (-X main.version=...) and shared
// with the agents built in the same image. Agents compare their own baked
// version to this and self-update when it differs.
var version = "dev"

type Config struct {
	ListenAddr   string
	DataDir      string
	AgentsDir    string
	PublicURL    string
	AdminUser    string
	AdminHash    string
	CompanyName  string
	SecureCookie bool

	// OnlyOffice (Excel editing). All optional — if OfficePublicURL is empty
	// the Excel feature is disabled and the dashboard hides the Edit button.
	OfficePublicURL string // browser-facing DS URL, e.g. https://office.example.com
	OfficeJWTSecret string // shared JWT secret configured on the DS
	InternalBaseURL string // how the DS container reaches THIS server, e.g. http://remote-files-server:8080
}

func (c Config) ExcelEnabled() bool { return c.OfficePublicURL != "" && c.InternalBaseURL != "" }

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		fmt.Fprint(os.Stderr, "Password: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw := strings.TrimRight(line, "\r\n")
		if len(pw) < 12 {
			fmt.Fprintln(os.Stderr, "password must be at least 12 characters")
			os.Exit(1)
		}
		h, err := hashPassword(pw)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(h)
		return
	}

	cfg := Config{
		ListenAddr:  env("LISTEN_ADDR", ":8080"),
		DataDir:     env("DATA_DIR", "/data"),
		AgentsDir:   env("AGENTS_DIR", "/app/agents"),
		PublicURL:   strings.TrimRight(env("PUBLIC_URL", ""), "/"),
		AdminUser:   env("ADMIN_USER", "admin"),
		AdminHash:   env("ADMIN_PASSWORD_HASH", ""),
		CompanyName: env("COMPANY_NAME", "Assac"),

		OfficePublicURL: strings.TrimRight(env("ONLYOFFICE_PUBLIC_URL", ""), "/"),
		OfficeJWTSecret: env("ONLYOFFICE_JWT_SECRET", ""),
		InternalBaseURL: strings.TrimRight(env("INTERNAL_BASE_URL", ""), "/"),
	}
	u, err := url.Parse(cfg.PublicURL)
	if cfg.PublicURL == "" || err != nil || u.Host == "" {
		log.Fatal("PUBLIC_URL must be set, e.g. https://files.example.com")
	}
	cfg.SecureCookie = u.Scheme == "https"
	if !strings.HasPrefix(cfg.AdminHash, "pbkdf2-sha256$") {
		log.Fatal("ADMIN_PASSWORD_HASH must be set (generate with: server hash-password)")
	}

	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	app := &App{
		cfg:       cfg,
		store:     store,
		audit:     OpenAudit(cfg.DataDir),
		hub:       NewHub(),
		sessions:  NewSessions(),
		limiter:   NewLimiter(),
		transfers: NewTransfers(),
		excel:     NewExcelSessions(),
		pending:   NewPendingSaves(cfg.DataDir),
	}
	go app.runPendingRetry()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("listening on %s (public %s)", cfg.ListenAddr, cfg.PublicURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
