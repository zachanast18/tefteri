ALTER TABLE merchant_category_rules ADD COLUMN merchant_enc BLOB;
ALTER TABLE merchant_category_rules ADD COLUMN enc_version INTEGER NOT NULL DEFAULT 0;
