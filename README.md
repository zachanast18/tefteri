# Teftéri

Self-hosted household spending ledger (`tefteri`) built with Go, SQLite, embedded HTML/CSS/JS, and local Tesseract OCR (`ell+eng`). Designed for phone-first receipt capture and Docker Compose deployment on a Proxmox Debian VM.

## Defaults

- **Currency:** `EUR` (`€`)
- **Timezone:** `Europe/Athens`
- **OCR languages:** `ell+eng` (Greek + English, bundled in the image; no runtime downloads)
- **Bind address:** `127.0.0.1:8080` (loopback by default)

## Quick Start (Docker Compose)

1. Copy the example environment file and adjust if needed:
   ```bash
   cp .env.example .env
   ```

2. Build the image and create your single household account interactively (password is read without echoing):
   ```bash
   docker compose build
   docker compose run --rm app setup-account
   ```

3. Start the service in the background:
   ```bash
   docker compose up -d --build
   ```

4. Check health and open the app:
   ```bash
   docker compose ps
   ```
   Open `http://127.0.0.1:8080` (or your HTTPS reverse proxy URL on your phone).

## Configuration

Edit `.env` before starting:

| Variable | Default | Description |
| --- | --- | --- |
| `BIND_ADDR` | `127.0.0.1` | Host IP to bind. Use `127.0.0.1` when a reverse proxy runs on the same VM, or the VM's private LAN/WireGuard IP if the proxy runs on another host. |
| `APP_PORT` | `8080` | Host port mapped to container port `8080`. |
| `APP_CURRENCY` | `EUR` | ISO currency code displayed in forms and CSV exports. |
| `APP_CURRENCY_SYMBOL` | `€` | Currency symbol used in formatted totals. |
| `APP_TIMEZONE` | `Europe/Athens` | IANA timezone for calendar dates and subscription schedules. |
| `OCR_LANGUAGES` | `ell+eng` | Tesseract language packs passed to `-l`. |
| `OCR_TIMEOUT_SECONDS` | `25` | Subprocess timeout for a single receipt scan. |
| `COOKIE_SECURE` | `auto` | `auto` sets the `Secure` cookie flag when `X-Forwarded-Proto: https` or TLS is present. |

## Phone Access via HTTPS Reverse Proxy

Phone browsers require HTTPS for smooth camera/file access and secure cookies. Keep `BIND_ADDR=127.0.0.1` if Caddy or Nginx runs on the same VM, or set `BIND_ADDR=<VM_PRIVATE_IP>` if the proxy runs on another host (for example, a dedicated reverse-proxy VM or OPNsense).

Example Caddyfile:
```caddy
ledger.home.arpa {
    reverse_proxy 127.0.0.1:8080
}
```

Ensure your proxy forwards `X-Forwarded-Proto: https` so `COOKIE_SECURE=auto` issues `Secure` session cookies.

## Operations

- **View logs:**
  ```bash
  docker compose logs -f app
  ```
- **Stop:**
  ```bash
  docker compose down
  ```
- **Update:**
  ```bash
  git pull
  docker compose up -d --build
  ```

## Live Backup and Restore

SQLite live backups use `VACUUM INTO` so both transactions and stored receipt images are captured consistently while the app is running.

### Export a backup outside the Docker volume

```bash
docker compose exec app /usr/local/bin/ledger backup /data/backup.db
docker compose cp app:/data/backup.db ./household-backup-$(date +%F).db
docker compose exec app rm -f /data/backup.db
```

### Restore into a separate volume

```bash
docker volume create ledger_data_restore
docker run --rm -v ledger_data_restore:/data -v "$PWD":/backup debian:bookworm-slim \
  sh -c "cp /backup/household-backup-YYYY-MM-DD.db /data/ledger.db && chown 10001:10001 /data/ledger.db"
```

Point a test or restored container at `ledger_data_restore:/data` and verify that both expenses and saved receipt images open after signing in.

## Subscription Behaviour

- Renewals are anchored to the original day of the month (and month/day for yearly renewals), clamping to the last day of shorter months (e.g., Jan 31 -> Feb 28/29 -> Mar 31; Feb 29 -> Feb 28 in non-leap years -> Feb 29 in leap years).
- Confirming a renewal creates the linked expense and records `(subscription_id, scheduled_date)` in one transaction.
- If a linked expense is later deleted from the ledger, the occurrence record remains so the renewal does not silently reappear as due.
