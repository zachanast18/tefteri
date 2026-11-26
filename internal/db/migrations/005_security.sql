-- 1. Household encryption keys
ALTER TABLE households ADD COLUMN dek_wrapped BLOB;
ALTER TABLE households ADD COLUMN kek_version INTEGER NOT NULL DEFAULT 0;

-- 2. Normalize roles: owner | member
CREATE TABLE household_members_new (
    household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (household_id, user_id)
);

INSERT INTO household_members_new (household_id, user_id, role, created_at)
SELECT household_id, user_id, CASE WHEN role = 'admin' THEN 'owner' ELSE 'member' END, created_at
FROM household_members;

DROP TABLE household_members;
ALTER TABLE household_members_new RENAME TO household_members;

-- 3. Scoped, revocable invites
ALTER TABLE household_invites ADD COLUMN role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member'));
ALTER TABLE household_invites ADD COLUMN revoked_at TEXT;

-- 4. User MFA, instance admin, and session hardening
ALTER TABLE users ADD COLUMN is_instance_admin INTEGER NOT NULL DEFAULT 0;
UPDATE users SET is_instance_admin = 1 WHERE id = 1;
ALTER TABLE users ADD COLUMN totp_seed_enc BLOB;
ALTER TABLE users ADD COLUMN totp_kek_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_enabled INTEGER NOT NULL DEFAULT 0;

ALTER TABLE sessions ADD COLUMN last_seen_at TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN mfa_verified INTEGER NOT NULL DEFAULT 1;
ALTER TABLE sessions ADD COLUMN last_auth_at TEXT NOT NULL DEFAULT '';
UPDATE sessions SET last_seen_at = created_at, last_auth_at = created_at WHERE last_seen_at = '';

-- 5. WebAuthn passkeys, challenges, recovery codes, and audit logs
CREATE TABLE webauthn_credentials (
    id BLOB PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label TEXT NOT NULL DEFAULT '',
    credential_json BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_webauthn_creds_user ON webauthn_credentials(user_id);

CREATE TABLE webauthn_challenges (
    id TEXT PRIMARY KEY,
    user_id INTEGER,
    purpose TEXT NOT NULL,
    session_data BLOB NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE recovery_codes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash TEXT NOT NULL,
    used_at TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_recovery_codes_user ON recovery_codes(user_id);

CREATE TABLE audit_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event TEXT NOT NULL,
    user_id INTEGER,
    household_id INTEGER,
    detail TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_logs_time ON audit_logs(created_at DESC, id DESC);

-- 6. Encrypted payload columns on sensitive tables
ALTER TABLE receipt_images ADD COLUMN data_enc BLOB;
ALTER TABLE receipt_images ADD COLUMN enc_version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE receipt_drafts ADD COLUMN merchant_enc BLOB;
ALTER TABLE receipt_drafts ADD COLUMN amount_enc BLOB;
ALTER TABLE receipt_drafts ADD COLUMN ocr_error_enc BLOB;
ALTER TABLE receipt_drafts ADD COLUMN enc_version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE expenses ADD COLUMN merchant_enc BLOB;
ALTER TABLE expenses ADD COLUMN note_enc BLOB;
ALTER TABLE expenses ADD COLUMN amount_enc BLOB;
ALTER TABLE expenses ADD COLUMN enc_version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE expense_splits ADD COLUMN share_enc BLOB;
ALTER TABLE expense_splits ADD COLUMN enc_version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE settlements ADD COLUMN amount_enc BLOB;
ALTER TABLE settlements ADD COLUMN note_enc BLOB;
ALTER TABLE settlements ADD COLUMN enc_version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE subscriptions ADD COLUMN name_enc BLOB;
ALTER TABLE subscriptions ADD COLUMN amount_enc BLOB;
ALTER TABLE subscriptions ADD COLUMN enc_version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE subscription_occurrences ADD COLUMN paid_amount_enc BLOB;
ALTER TABLE subscription_occurrences ADD COLUMN enc_version INTEGER NOT NULL DEFAULT 0;
