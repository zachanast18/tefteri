CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS categories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_default INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO categories (name, sort_order, is_default) VALUES
    ('Groceries', 10, 1),
    ('Dining', 20, 1),
    ('Transport', 30, 1),
    ('Fuel', 40, 1),
    ('Utilities', 50, 1),
    ('Health', 60, 1),
    ('Shopping', 70, 1),
    ('Entertainment', 80, 1),
    ('Other', 90, 1),
    ('Uncategorized', 100, 1);

CREATE TABLE IF NOT EXISTS merchant_category_rules (
    normalized_merchant TEXT PRIMARY KEY,
    category_id INTEGER NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS receipt_images (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256 TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    width INTEGER NOT NULL,
    height INTEGER NOT NULL,
    data BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_receipt_images_sha256 ON receipt_images(sha256);

CREATE TABLE IF NOT EXISTS receipt_drafts (
    id TEXT PRIMARY KEY,
    receipt_image_id INTEGER NOT NULL REFERENCES receipt_images(id) ON DELETE CASCADE,
    merchant TEXT NOT NULL DEFAULT '',
    purchase_date TEXT NOT NULL DEFAULT '',
    amount_cents INTEGER NOT NULL DEFAULT 0,
    payment_method TEXT NOT NULL DEFAULT '',
    category_id INTEGER REFERENCES categories(id),
    check_merchant INTEGER NOT NULL DEFAULT 0,
    check_date INTEGER NOT NULL DEFAULT 0,
    check_amount INTEGER NOT NULL DEFAULT 0,
    ocr_status TEXT NOT NULL DEFAULT 'ok',
    ocr_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS expenses (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL DEFAULT 'expense' CHECK (kind IN ('expense', 'refund')),
    amount_cents INTEGER NOT NULL CHECK (amount_cents > 0),
    merchant TEXT NOT NULL,
    normalized_merchant TEXT NOT NULL,
    purchase_date TEXT NOT NULL,
    payment_method TEXT NOT NULL CHECK (payment_method IN ('cash', 'card')),
    category_id INTEGER NOT NULL REFERENCES categories(id),
    note TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'receipt', 'subscription')),
    receipt_image_id INTEGER REFERENCES receipt_images(id) ON DELETE SET NULL,
    submission_token TEXT UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_expenses_date ON expenses(purchase_date DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_expenses_category ON expenses(category_id);
CREATE INDEX IF NOT EXISTS idx_expenses_merchant ON expenses(normalized_merchant);
CREATE INDEX IF NOT EXISTS idx_expenses_receipt ON expenses(receipt_image_id);

CREATE TABLE IF NOT EXISTS subscriptions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    amount_cents INTEGER NOT NULL CHECK (amount_cents > 0),
    frequency TEXT NOT NULL CHECK (frequency IN ('monthly', 'yearly')),
    category_id INTEGER NOT NULL REFERENCES categories(id),
    payment_method TEXT NOT NULL CHECK (payment_method IN ('cash', 'card')),
    first_renewal_date TEXT NOT NULL,
    next_due_date TEXT NOT NULL,
    anchor_day INTEGER NOT NULL CHECK (anchor_day BETWEEN 1 AND 31),
    anchor_month INTEGER NOT NULL CHECK (anchor_month BETWEEN 1 AND 12),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_subscriptions_due ON subscriptions(status, next_due_date);

CREATE TABLE IF NOT EXISTS subscription_occurrences (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    subscription_id INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    scheduled_date TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('paid', 'skipped')),
    paid_amount_cents INTEGER,
    paid_date TEXT,
    expense_id INTEGER REFERENCES expenses(id) ON DELETE SET NULL,
    created_at TEXT NOT NULL,
    UNIQUE(subscription_id, scheduled_date)
);
