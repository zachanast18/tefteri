# Teftéri Security & Operator Recovery Runbook

## 1. Encryption Model & Readable Metadata

Teftéri uses **per-household 256-bit AES-GCM Data Encryption Keys (DEKs)** wrapped by a server-managed versioned **Key Encryption Key (KEK)** (`MASTER_KEY_FILE` or `MASTER_KEY`). Every ciphertext binds `tefteri:hh:<household_id>:<table>:<record_id>:<field>` as Authenticated Associated Data (AAD).

### Fields encrypted at rest
- `receipt_images.data_enc` (JPEG bytes)
- `receipt_drafts`: `merchant_enc`, `amount_enc`, `ocr_error_enc`
- `expenses`: `merchant_enc`, `note_enc`, `amount_enc`
- `expense_splits.share_enc`
- `settlements`: `amount_enc`, `note_enc`
- `subscriptions`: `name_enc`, `amount_enc`
- `subscription_occurrences.paid_amount_enc`
- `users.totp_seed_enc` (encrypted under a domain-separated KEK subkey)

### Fields left readable for routing, scheduling, and indexing
- `households.id`, `households.name`, `household_members` (`user_id`, `role`) — membership and authorization routing.
- `expenses.household_id`, `purchase_date`, `kind`, `payment_method`, `category_id`, `source`, `created_by_user_id`, `paid_by_user_id` — range filtering and attribution without decrypting unrelated households.
- `expenses.normalized_merchant` and `merchant_category_rules.normalized_merchant` — keyed HMAC-SHA256 blind index (`Subkey(dek, "index:merchant")`), not plaintext.
- `subscriptions.household_id`, `frequency`, `next_due_date`, `anchor_day`, `anchor_month`, `status` — renewal scheduling.
- `audit_logs` — minimal event identifiers (`event`, `user_id`, `household_id`, `created_at`), excluding receipt content, passwords, OTPs, cookies, and tokens.

## 2. Master Key Generation & Rotation

Generate a 256-bit key:
```bash
echo "v1:$(openssl rand -base64 32)" > /root/tefteri-master.key
chmod 600 /root/tefteri-master.key
```
Keep a separately protected offline recovery copy (e.g., password manager or encrypted USB) **outside** the Proxmox host, Docker volume, and SQLite backups.

### Routine KEK Rotation
1. Add `v2:<new_base64>` alongside `v1:<old_base64>` in `MASTER_KEY` / `MASTER_KEY_FILE`:
   `v1:<old>,v2:<new>`
2. Restart or run:
   ```bash
   docker compose exec app /usr/local/bin/ledger rotate-kek
   ```
3. Remove `v1:<old>,` from `MASTER_KEY` / `MASTER_KEY_FILE` and restart the container.

### Household DEK Rotation (if a household key is suspected exposed)
```bash
docker compose exec app /usr/local/bin/ledger rotate-dek <household_id>
```

## 3. Existing Plaintext Migration

On startup with a valid KEK, `OpenWithKeys` automatically encrypts any `enc_version = 0` rows and zeroes plaintext columns. Before upgrading a production database:
1. Take a backup (`ledger backup`).
2. Rehearse migration on a copy in a staging container.
3. Verify record counts and decrypted receipts/expenses, then switch over.
4. Purge or encrypt old plaintext backups, `-wal`/`-shm` files, and retired volumes according to your retention policy.

## 4. Live Backup & Clean-VM Restore

### Backup (off-host)
```bash
docker compose exec app /usr/local/bin/ledger backup /data/backup.db
docker compose cp app:/data/backup.db ./tefteri-backup-$(date +%F).db
docker compose exec app rm -f /data/backup.db
# Encrypt and copy off the Proxmox host (e.g., restic / borg / age)
```

### Restore to a clean VM
1. Provision the new Docker host with `compose.yaml`, `compose.restore.yaml`, and `.env` (including the **same** `MASTER_KEY` / `MASTER_KEY_FILE`, `WEBAUTHN_RP_ID`, and `WEBAUTHN_RP_ORIGIN`).
2. Restore the database into `tefteri_restore_data`, making `/data` writable by UID `10001` so SQLite can create WAL/SHM journal files:
   ```bash
   docker volume create tefteri_restore_data
   docker run --rm -v tefteri_restore_data:/data -v "$PWD":/backup debian:bookworm-slim \
     sh -c "cp /backup/tefteri-backup-YYYY-MM-DD.db /data/ledger.db && chown -R 10001:10001 /data && chmod 750 /data && chmod 640 /data/ledger.db"
   ```
3. Verify the effective volume mount and start using the restore override:
   ```bash
   docker compose -f compose.yaml -f compose.restore.yaml config | grep -A3 ledger_data
   docker compose -f compose.yaml -f compose.restore.yaml up -d
   ```
4. Sign in on the configured HTTPS domain, open a receipt, reconcile household totals, and verify a new expense survives `docker compose -f compose.yaml -f compose.restore.yaml restart`.

## 5. Incident & Member Revocation

- **Revoke a member:** Household owner clicks **Remove** on `/users`. Active sessions immediately lose access to that household.
- **Lost device:** User signs in with a **single-use recovery code** at `/recovery` (which revokes all existing sessions), then enrolls a new passkey/TOTP on `/security`.
- **Compromised session:** User clicks **Sign out all devices** on `/security`.

## 6. Explicit Deployment Decisions Before Public Launch

1. **DNS & HTTPS Ingress:** Point your domain to the reverse proxy (Caddy/Nginx), configure `WEBAUTHN_RP_ID=<domain>` and `WEBAUTHN_RP_ORIGIN=https://<domain>`, and set `COOKIE_SECURE=true` and `TRUSTED_PROXY=<proxy_ip>`.
2. **Registration Policy:** Keep `REGISTRATION_MODE=invite_only` (or `closed` once all household members are enrolled).
3. **Monitoring:** Alert on repeated `login_failed` / `mfa_failed` / `recovery_failed` in `audit_logs`, OCR saturation (`ErrBusy` / `ErrTimeout`), `/data` disk usage, and failed backups.
