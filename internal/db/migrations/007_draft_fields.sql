ALTER TABLE receipt_drafts ADD COLUMN kind TEXT NOT NULL DEFAULT 'expense';
ALTER TABLE receipt_drafts ADD COLUMN paid_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE receipt_drafts ADD COLUMN note_enc BLOB;
ALTER TABLE receipt_drafts ADD COLUMN amount_raw_enc BLOB;
ALTER TABLE receipt_drafts ADD COLUMN split_data_enc BLOB;
