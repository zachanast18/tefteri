ALTER TABLE expenses ADD COLUMN paid_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL;
UPDATE expenses SET paid_by_user_id = created_by_user_id WHERE paid_by_user_id IS NULL;

CREATE TABLE expense_splits (
    expense_id INTEGER NOT NULL REFERENCES expenses(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    share_cents INTEGER NOT NULL CHECK (share_cents >= 0),
    PRIMARY KEY (expense_id, user_id)
);

CREATE TABLE settlements (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    from_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    to_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    amount_cents INTEGER NOT NULL CHECK (amount_cents > 0),
    settled_date TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    created_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_settlements_hh_date ON settlements(household_id, settled_date DESC, id DESC);

ALTER TABLE receipt_drafts ADD COLUMN needs_review INTEGER NOT NULL DEFAULT 0;
ALTER TABLE receipt_drafts ADD COLUMN review_reason TEXT NOT NULL DEFAULT '';

CREATE TABLE category_budgets (
    household_id INTEGER NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    category_id INTEGER NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    monthly_limit_cents INTEGER NOT NULL CHECK (monthly_limit_cents > 0),
    PRIMARY KEY (household_id, category_id)
);
