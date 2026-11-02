# Remote Files

Consent-based remote file management for company PCs (Windows + Mac) over the internet.

**Phase 1:** invite links, user consent, enrollment, online status, drive/folder browsing, revoke, audit log.

**Phase 2 — chunk 1 (done):** file operations — new folder, rename, copy, move, upload, download,
and permanent delete (with a type-DELETE confirm for folders). Uploads back up any existing file
first (`.rf-backups/` next to it) and refuse silently if the file is open on the PC. Transfers stream
through the server (no size cap in code; upload HTTP cap 2 GB), so large files don't sit in memory.

**Phase 2 — chunk 2 (done):** users & roles.
- **Admin** — everything, including permanent delete, device management, invites, audit log, and user management.
- **Editor** — browse, upload, download, copy, move, rename. No delete.
- **Viewer** — browse and download only.
- One built-in root admin from `ADMIN_USER` / `ADMIN_PASSWORD_HASH` (can't be deleted or demoted, so you
  can always get in). Extra users live in `store.json`; passwords are PBKDF2-hashed. Role gates are enforced
  server-side on every route, not just hidden in the UI. Disabling, deleting, or changing a user's role or
  password signs their active sessions out immediately.

**Phase 2 — chunk 3 (built, needs real-PC testing):** autostart + tray.
- On first run the agent copies itself to a stable path (`%APPDATA%\RemoteAgent\` /
  `~/Library/Application Support/RemoteAgent/`) and sets itself to start at every logon
  (Windows: a `.cmd` in the Startup folder; macOS: a LaunchAgent plist). Consent is still asked only once.
- **Windows:** a system-tray icon (built with raw Win32, no external deps) with *Open dashboard*,
  *Disconnect this computer*, *Quit*.
- **macOS:** runs headless via the LaunchAgent; the menu-bar icon needs Cocoa bindings and is added in the
  macOS build step (this build environment can't fetch the needed modules).
- Disconnect (tray or `--disconnect`) removes the autostart entry and the saved connection.
- Not a pre-login Windows Service yet — it starts at user logon, which on an office PC is right after boot.
  A true boot-time service is a later hardening option.

**Phase 2 — chunk 4 (built; needs OnlyOffice on the VPS to test in a browser):** in-browser Excel editing.
- Dashboard shows **Edit in Excel** on spreadsheet files (.xlsx/.xlsm/.xls/.csv/.ods) for Editor+ when the
  feature is configured. It opens a full-screen OnlyOffice editor.
- Flow: editor asks our server for a signed config → the Document Server (DS) pulls the file from our
  server, which fetches it from the agent → on save, DS calls back → our server downloads the edited file
  and writes it to the agent (overwrite **with a backup**, same as uploads).
- Security: unguessable per-session id in the file/callback URLs, plus **JWT** (HS256) shared with the DS;
  a bad/missing token is rejected and nothing is saved.
- Disabled by default: if `ONLYOFFICE_PUBLIC_URL` / `INTERNAL_BASE_URL` are unset, the Edit button is
  hidden and the endpoints return 501 — the rest of the app runs normally.
- Server-side flow (open → serve → signed callback → save-back-with-backup, JWT accept/reject, disabled
  mode) was tested here against a **mocked** Document Server. The real DS rendering in a browser needs the
  VPS deploy below.

**Still to come:** macOS menu-bar icon, code signing / notarization + installers (Phase 4),
per-PC access scoping.

## OnlyOffice (Excel) deploy notes

The `onlyoffice` service and the `office.clevercampus.cloud` nginx block are included. To enable Excel:

1. In `deploy/.env`: set `ONLYOFFICE_PUBLIC_URL=https://office.clevercampus.cloud`,
   `INTERNAL_BASE_URL=http://remote-files-server:8080`, and
   `ONLYOFFICE_JWT_SECRET=$(openssl rand -hex 32)`. Put the **same** secret on the `onlyoffice` service
   (it reads `ONLYOFFICE_JWT_SECRET` via compose).
2. Add a DNS A record `office.clevercampus.cloud` → this VPS (same wildcard cert covers it, like `files`).
   Confirm no school tenant uses `office`.
3. `docker compose up -d` (pulls ~1.5 GB DS image; needs ~2 GB RAM free — add swap if tight). Wait for
   `remote-files-onlyoffice` to become healthy (first boot takes a minute).
4. The nginx dry-run + attach steps are the same as for `files` — both blocks are in the one config file.
5. Verify: open `https://office.clevercampus.cloud/welcome/` (DS welcome page), then in the dashboard
   click **Edit in Excel** on an .xlsx.

⚠️ **One integration point to watch on first real test:** when DS posts the save callback, the download
URL it provides for the edited file must be reachable from the `remote-files-server` container. If saves
fail, check the DS callback URL host in the logs — it should resolve on the shared docker network
(`onlyoffice`), and may need the DS `--network-alias` / a documentserver address setting. This is the one
part that couldn't be tested without the real DS.

## How it works

```
Admin dashboard ──HTTPS──> nginx (Clever Campus) ──> remote-files-server ──long-poll──< Agent on each PC
```

- **Agent** (single Go binary, no install needed) asks the user for permission, enrolls once,
  then long-polls the server over plain HTTPS. No open ports on the PCs, works behind NAT/firewalls.
- **Invite link** → user downloads `RemoteAgent-<one-time-code>.exe`. The code is in the file name,
  so signing (Phase 4) won't be broken by embedding it. Links expire (default 24h) and work once.
- **Server** (Go, zero third-party dependencies) stores devices/invites in `/data/store.json`
  and an append-only `/data/audit.log`. Device secrets and invite codes are stored only as SHA-256 hashes.
- **Admin auth:** one admin account, PBKDF2-SHA256 (600k iterations), 12h sessions, HttpOnly +
  SameSite=Strict cookie + CSRF header, 5 failed logins / 15 min lockout per IP.

## Layout

```
agent/      agent (consent dialog: Win MessageBox / macOS osascript; drives; listing)
server/     server + embedded dashboard (server/web)
internal/   shared wire types
deploy/     docker-compose, nginx server block, Clever Campus override, certbot hook
Dockerfile  builds server + Windows/Mac agents (URL + company name baked in)
```

## Deploy on the VPS (Clever Campus server)

Nothing below touches Clever Campus until step 6, and step 6 is test-first with a rollback.

### 0. Pre-checks (read-only)

```bash
# Which compose files does Clever Campus use? If this lists anything other than
# docker-compose.yml, STOP — the override file won't auto-load; adjust step 6.
docker inspect clever-campus-nginx-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}'
ls -l /root/school-erp-saas/docker-compose.override.yml 2>/dev/null || echo "no override yet (good)"
```
Also confirm no school tenant uses the subdomain `files`.

### 1. Upload code to `/root/remote-files`

### 2. Configure
```bash
cd /root/remote-files/deploy
cp .env.example .env && chmod 600 .env
nano .env        # set PUBLIC_URL, COMPANY_NAME, ADMIN_USER
```

### 3. Build
```bash
docker compose build
```

### 4. Admin password hash
```bash
docker run --rm -i remote-files-server:latest hash-password
# type a password (12+ chars), paste output into .env as ADMIN_PASSWORD_HASH='...'
```

### 5. Start (no ports published — invisible from outside until step 6)
```bash
docker compose up -d
docker compose ps        # should become "healthy"
docker exec clever-campus-nginx-1 wget -qO- http://remote-files-server:8080/healthz   # → ok
```

### 6. Attach to nginx — needs approval, do it at low traffic

**6a. Dry-run test with the exact same nginx image (zero impact on the live site):**
```bash
docker run --rm --network clever-campus_clever_campus_net \
  -v /root/school-erp-saas/deploy/nginx/docker.conf:/etc/nginx/conf.d/default.conf:ro \
  -v /root/remote-files/deploy/nginx/remote-files.conf:/etc/nginx/conf.d/remote-files.conf:ro \
  -v /root/school-erp-saas/deploy/nginx/ssl:/etc/nginx/ssl:ro \
  nginx:1.27-alpine nginx -t
```
Must print `syntax is ok` and `test is successful`. If not, stop here.

**6b. Apply (nginx container recreated → ~2–5 s blip for Clever Campus):**
```bash
cp /root/remote-files/deploy/clever-campus-override.yml /root/school-erp-saas/docker-compose.override.yml
cd /root/school-erp-saas && docker compose up -d nginx
docker exec clever-campus-nginx-1 nginx -t
curl -sI https://clevercampus.cloud | head -1          # Clever Campus still OK
curl -s https://files.clevercampus.cloud/healthz        # → ok
```

**Rollback:**
```bash
rm /root/school-erp-saas/docker-compose.override.yml
cd /root/school-erp-saas && docker compose up -d nginx
```

> From now on, always run Clever Campus's `docker compose` commands from `/root/school-erp-saas`
> (the override is picked up automatically there).

### 7. Use it
Open `https://files.clevercampus.cloud` → sign in → **Add device** → send the link.

## ⚠️ SSL renewal (affects Clever Campus too)

The wildcard cert was issued with `authenticator = manual` (DNS-01). The certbot timer **cannot**
renew it automatically, and the files in `deploy/nginx/ssl/` are copies, not symlinks.
Current cert expires **10 Dec 2026**.

- **Short term:** renew manually before ~1 Dec: `certbot renew --cert-name <name> --manual`
  (add the TXT record it asks for), then run the hook below once by hand.
- **Proper fix:** switch to a DNS plugin for your DNS provider (e.g. Cloudflare →
  `python3-certbot-dns-cloudflare`) so renewals are automatic, and install
  `deploy/certbot-deploy-hook.sh` as `/etc/letsencrypt/renewal-hooks/deploy/clever-campus-copy.sh`
  so renewed certs are copied into nginx and reloaded with zero downtime.
  Check which lineage is the wildcard first: `certbot certificates`.

## Local development

```bash
go run ./server hash-password                    # get a hash
DATA_DIR=./data AGENTS_DIR=./agents PUBLIC_URL=http://localhost:8080 \
  ADMIN_PASSWORD_HASH='...' go run ./server
# agent: copy the binary to RemoteAgent-<code> and run with RF_SERVER=http://localhost:8080
```

## Known Phase 1 limits (planned)

- Read-only (Phase 2: file operations, chunked upload/download, trash-not-delete).
- Agent runs per user session; no autostart/tray yet (Phase 4: service, tray icon, installer).
- Unsigned binaries → SmartScreen / Gatekeeper warnings (Phase 4: code signing + notarization).
- Mac needs Full Disk Access granted manually for full visibility.
- Admin sessions are in memory — a server restart signs admins out.
