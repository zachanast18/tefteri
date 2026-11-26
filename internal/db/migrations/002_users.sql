CREATE TABLE users_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);

INSERT INTO users_new (id, username, display_name, password_hash, created_at)
SELECT id, username, username, password_hash, created_at FROM users;

CREATE TABLE sessions_new (
    token_hash TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users_new(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

INSERT INTO sessions_new (token_hash, user_id, expires_at, created_at)
SELECT token_hash, user_id, expires_at, created_at FROM sessions;

DROP TABLE sessions;
DROP TABLE users;

ALTER TABLE users_new RENAME TO users;
ALTER TABLE sessions_new RENAME TO sessions;
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

ALTER TABLE expenses ADD COLUMN created_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_expenses_user ON expenses(created_by_user_id);

ALTER TABLE subscription_occurrences ADD COLUMN confirmed_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL;
