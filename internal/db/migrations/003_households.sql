CREATE TABLE households (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    created_at TEXT NOT NULL
);

INSERT INTO households (id, name, created_at)
VALUES (1, 'Our Household', strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));

CREATE TABLE household_members (
    household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (household_id, user_id)
);

INSERT OR IGNORE INTO household_members (household_id, user_id, role, created_at)
SELECT 1, id, CASE WHEN id = 1 THEN 'admin' ELSE 'member' END, created_at FROM users;

CREATE TABLE household_invites (
    code TEXT PRIMARY KEY,
    household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    created_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
    expires_at TEXT NOT NULL,
    used_at TEXT,
    created_at TEXT NOT NULL
);

ALTER TABLE sessions ADD COLUMN active_household_id INTEGER REFERENCES households(id) ON DELETE SET NULL;
UPDATE sessions SET active_household_id = 1 WHERE active_household_id IS NULL;

-- Rebuild categories to be per-household: UNIQUE(household_id, name)
CREATE TABLE categories_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_default INTEGER NOT NULL DEFAULT 0,
    UNIQUE(household_id, name)
);

INSERT INTO categories_new (id, household_id, name, sort_order, is_default)
SELECT id, 1, name, sort_order, is_default FROM categories;

DROP TABLE categories;
ALTER TABLE categories_new RENAME TO categories;

-- Rebuild merchant_category_rules to be per-household
CREATE TABLE merchant_category_rules_new (
    household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    normalized_merchant TEXT NOT NULL,
    category_id INTEGER NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (household_id, normalized_merchant)
);

INSERT INTO merchant_category_rules_new (household_id, normalized_merchant, category_id, updated_at)
SELECT 1, normalized_merchant, category_id, updated_at FROM merchant_category_rules;

DROP TABLE merchant_category_rules;
ALTER TABLE merchant_category_rules_new RENAME TO merchant_category_rules;

-- Add household_id to receipt_images, receipt_drafts, expenses, subscriptions
ALTER TABLE receipt_images ADD COLUMN household_id INTEGER NOT NULL DEFAULT 1 REFERENCES households(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_receipt_images_hh_sha256 ON receipt_images(household_id, sha256);

ALTER TABLE receipt_drafts ADD COLUMN household_id INTEGER NOT NULL DEFAULT 1 REFERENCES households(id) ON DELETE CASCADE;

ALTER TABLE expenses ADD COLUMN household_id INTEGER NOT NULL DEFAULT 1 REFERENCES households(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_expenses_hh_date ON expenses(household_id, purchase_date DESC, id DESC);

ALTER TABLE subscriptions ADD COLUMN household_id INTEGER NOT NULL DEFAULT 1 REFERENCES households(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_subscriptions_hh_due ON subscriptions(household_id, status, next_due_date);
