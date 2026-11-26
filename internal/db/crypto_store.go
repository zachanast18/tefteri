package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"spending-project/internal/categorize"
	"spending-project/internal/crypto"
)

func (s *Store) ensureDEKTx(ctx context.Context, tx *sql.Tx, householdID int64) ([]byte, error) {
	if s.Keys == nil {
		return nil, crypto.ErrMissingMasterKey
	}
	var wrapped []byte
	var ver int
	err := tx.QueryRowContext(ctx, `SELECT dek_wrapped, kek_version FROM households WHERE id = ?`, householdID).Scan(&wrapped, &ver)
	if err != nil {
		return nil, err
	}
	if len(wrapped) > 0 && ver > 0 {
		return s.Keys.UnwrapKey(wrapped, ver, householdID)
	}
	dek, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	w, kv, err := s.Keys.WrapKey(dek, householdID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE households SET dek_wrapped = ?, kek_version = ? WHERE id = ?`, w, kv, householdID); err != nil {
		return nil, err
	}
	return dek, nil
}

func (s *Store) EnsureHouseholdDEK(ctx context.Context, householdID int64) ([]byte, error) {
	if s.Keys == nil {
		return nil, crypto.ErrMissingMasterKey
	}
	var wrapped []byte
	var ver int
	err := s.DB.QueryRowContext(ctx, `SELECT dek_wrapped, kek_version FROM households WHERE id = ?`, householdID).Scan(&wrapped, &ver)
	if err != nil {
		return nil, err
	}
	if len(wrapped) > 0 && ver > 0 {
		return s.Keys.UnwrapKey(wrapped, ver, householdID)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	dek, err := s.ensureDEKTx(ctx, tx, householdID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return dek, nil
}

// RotateKEK rewraps all household DEKs and user TOTP seeds under the current KEK version.
func (s *Store) RotateKEK(ctx context.Context) error {
	if s.Keys == nil {
		return crypto.ErrMissingMasterKey
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id, dek_wrapped, kek_version FROM households WHERE dek_wrapped IS NOT NULL AND kek_version > 0`)
	if err != nil {
		return err
	}
	type hhRow struct {
		id  int64
		w   []byte
		ver int
	}
	var hhs []hhRow
	for rows.Next() {
		var r hhRow
		if err := rows.Scan(&r.id, &r.w, &r.ver); err != nil {
			rows.Close()
			return err
		}
		hhs = append(hhs, r)
	}
	rows.Close()

	for _, h := range hhs {
		if h.ver == s.Keys.CurrentVersion {
			continue
		}
		dek, err := s.Keys.UnwrapKey(h.w, h.ver, h.id)
		if err != nil {
			return fmt.Errorf("unwrap household %d: %w", h.id, err)
		}
		nw, nv, err := s.Keys.WrapKey(dek, h.id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE households SET dek_wrapped = ?, kek_version = ? WHERE id = ?`, nw, nv, h.id); err != nil {
			return err
		}
	}

	uRows, err := tx.QueryContext(ctx, `SELECT id, totp_seed_enc, totp_kek_version FROM users WHERE totp_seed_enc IS NOT NULL AND totp_kek_version > 0`)
	if err != nil {
		return err
	}
	type uRow struct {
		id  int64
		ct  []byte
		ver int
	}
	var users []uRow
	for uRows.Next() {
		var r uRow
		if err := uRows.Scan(&r.id, &r.ct, &r.ver); err != nil {
			uRows.Close()
			return err
		}
		users = append(users, r)
	}
	uRows.Close()

	for _, u := range users {
		if u.ver == s.Keys.CurrentVersion {
			continue
		}
		seed, err := s.Keys.DecryptTOTPSeed(u.id, u.ver, u.ct)
		if err != nil {
			return fmt.Errorf("decrypt totp user %d: %w", u.id, err)
		}
		nct, nv, err := s.Keys.EncryptTOTPSeed(u.id, seed)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_seed_enc = ?, totp_kek_version = ? WHERE id = ?`, nct, nv, u.id); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	_ = s.LogAudit(ctx, "kek_rotated", nil, nil, fmt.Sprintf("version=%d", s.Keys.CurrentVersion))
	return nil
}

// RotateHouseholdDEK replaces a household's data key and re-encrypts all its records in one transaction.
func (s *Store) RotateHouseholdDEK(ctx context.Context, householdID int64) error {
	if s.Keys == nil {
		return crypto.ErrMissingMasterKey
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	oldDEK, err := s.ensureDEKTx(ctx, tx, householdID)
	if err != nil {
		return err
	}
	newDEK, err := crypto.GenerateKey()
	if err != nil {
		return err
	}

	// 1. receipt_images
	if err := reencryptReceiptsTx(ctx, tx, householdID, oldDEK, newDEK); err != nil {
		return err
	}
	// 2. receipt_drafts
	if err := reencryptDraftsTx(ctx, tx, householdID, oldDEK, newDEK); err != nil {
		return err
	}
	// 3. expenses + expense_splits
	if err := reencryptExpensesTx(ctx, tx, householdID, oldDEK, newDEK); err != nil {
		return err
	}
	// 4. settlements
	if err := reencryptSettlementsTx(ctx, tx, householdID, oldDEK, newDEK); err != nil {
		return err
	}
	// 5. subscriptions + occurrences
	if err := reencryptSubscriptionsTx(ctx, tx, householdID, oldDEK, newDEK); err != nil {
		return err
	}

	nw, nv, err := s.Keys.WrapKey(newDEK, householdID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE households SET dek_wrapped = ?, kek_version = ? WHERE id = ?`, nw, nv, householdID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = s.LogAudit(ctx, "dek_rotated", nil, &householdID, "")
	return nil
}

// MigratePlaintextToEncrypted encrypts any existing enc_version=0 rows across all households.
func (s *Store) MigratePlaintextToEncrypted(ctx context.Context) error {
	if s.Keys == nil {
		return crypto.ErrMissingMasterKey
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM households ORDER BY id ASC`)
	if err != nil {
		return err
	}
	var hhIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		hhIDs = append(hhIDs, id)
	}
	rows.Close()

	for _, hhID := range hhIDs {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		dek, err := s.ensureDEKTx(ctx, tx, hhID)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := migrateHouseholdPlaintextTx(ctx, tx, hhID, dek); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func migrateHouseholdPlaintextTx(ctx context.Context, tx *sql.Tx, hhID int64, dek []byte) error {
	// receipt_images
	rRows, err := tx.QueryContext(ctx, `SELECT id, data FROM receipt_images WHERE household_id = ? AND enc_version = 0`, hhID)
	if err != nil {
		return err
	}
	type idBytes struct {
		id int64
		b  []byte
	}
	var recs []idBytes
	for rRows.Next() {
		var x idBytes
		if err := rRows.Scan(&x.id, &x.b); err != nil {
			rRows.Close()
			return err
		}
		recs = append(recs, x)
	}
	rRows.Close()
	for _, x := range recs {
		ct, err := crypto.Encrypt(dek, crypto.AAD(hhID, "receipt_images", x.id, "data"), x.b)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE receipt_images SET data_enc = ?, data = x'', enc_version = 1 WHERE id = ?`, ct, x.id); err != nil {
			return err
		}
	}

	// receipt_drafts
	dRows, err := tx.QueryContext(ctx, `SELECT id, merchant, amount_cents, ocr_error FROM receipt_drafts WHERE household_id = ? AND enc_version = 0`, hhID)
	if err != nil {
		return err
	}
	type dRow struct {
		id, m, errStr string
		amt           int64
	}
	var drafts []dRow
	for dRows.Next() {
		var d dRow
		if err := dRows.Scan(&d.id, &d.m, &d.amt, &d.errStr); err != nil {
			dRows.Close()
			return err
		}
		drafts = append(drafts, d)
	}
	dRows.Close()
	for _, d := range drafts {
		mEnc, _ := crypto.Encrypt(dek, crypto.AAD(hhID, "receipt_drafts", d.id, "merchant"), []byte(d.m))
		aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(hhID, "receipt_drafts", d.id, "amount"), d.amt)
		eEnc, _ := crypto.Encrypt(dek, crypto.AAD(hhID, "receipt_drafts", d.id, "ocr_error"), []byte(d.errStr))
		if _, err := tx.ExecContext(ctx, `
			UPDATE receipt_drafts SET merchant_enc = ?, amount_enc = ?, ocr_error_enc = ?, merchant = '', amount_cents = 0, ocr_error = '', enc_version = 1 WHERE id = ?
		`, mEnc, aEnc, eEnc, d.id); err != nil {
			return err
		}
	}

	// expenses
	eRows, err := tx.QueryContext(ctx, `SELECT id, merchant, normalized_merchant, note, amount_cents FROM expenses WHERE household_id = ? AND enc_version = 0`, hhID)
	if err != nil {
		return err
	}
	type eRow struct {
		id           int64
		m, norm, n   string
		amt          int64
	}
	var exps []eRow
	for eRows.Next() {
		var e eRow
		if err := eRows.Scan(&e.id, &e.m, &e.norm, &e.n, &e.amt); err != nil {
			eRows.Close()
			return err
		}
		exps = append(exps, e)
	}
	eRows.Close()
	for _, e := range exps {
		mEnc, _ := crypto.Encrypt(dek, crypto.AAD(hhID, "expenses", e.id, "merchant"), []byte(e.m))
		nEnc, _ := crypto.Encrypt(dek, crypto.AAD(hhID, "expenses", e.id, "note"), []byte(e.n))
		aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(hhID, "expenses", e.id, "amount"), e.amt)
		norm := e.norm
		if norm == "" {
			norm = categorize.NormalizeMerchant(e.m)
		}
		hmacIdx := crypto.BlindIndex(dek, "merchant", norm)
		if _, err := tx.ExecContext(ctx, `
			UPDATE expenses SET merchant_enc = ?, note_enc = ?, amount_enc = ?, merchant = '', note = '', amount_cents = 1, normalized_merchant = ?, enc_version = 1 WHERE id = ?
		`, mEnc, nEnc, aEnc, hmacIdx, e.id); err != nil {
			return err
		}
	}

	// expense_splits
	spRows, err := tx.QueryContext(ctx, `
		SELECT sp.expense_id, sp.user_id, sp.share_cents
		FROM expense_splits sp JOIN expenses e ON e.id = sp.expense_id
		WHERE e.household_id = ? AND sp.enc_version = 0
	`, hhID)
	if err != nil {
		return err
	}
	type spRow struct {
		eid, uid, share int64
	}
	var splits []spRow
	for spRows.Next() {
		var sp spRow
		if err := spRows.Scan(&sp.eid, &sp.uid, &sp.share); err != nil {
			spRows.Close()
			return err
		}
		splits = append(splits, sp)
	}
	spRows.Close()
	for _, sp := range splits {
		sEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(hhID, "expense_splits", fmt.Sprintf("%d:%d", sp.eid, sp.uid), "share"), sp.share)
		if _, err := tx.ExecContext(ctx, `UPDATE expense_splits SET share_enc = ?, share_cents = 0, enc_version = 1 WHERE expense_id = ? AND user_id = ?`, sEnc, sp.eid, sp.uid); err != nil {
			return err
		}
	}

	// settlements
	stRows, err := tx.QueryContext(ctx, `SELECT id, amount_cents, note FROM settlements WHERE household_id = ? AND enc_version = 0`, hhID)
	if err != nil {
		return err
	}
	type stRow struct {
		id, amt int64
		note    string
	}
	var sts []stRow
	for stRows.Next() {
		var st stRow
		if err := stRows.Scan(&st.id, &st.amt, &st.note); err != nil {
			stRows.Close()
			return err
		}
		sts = append(sts, st)
	}
	stRows.Close()
	for _, st := range sts {
		aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(hhID, "settlements", st.id, "amount"), st.amt)
		nEnc, _ := crypto.Encrypt(dek, crypto.AAD(hhID, "settlements", st.id, "note"), []byte(st.note))
		if _, err := tx.ExecContext(ctx, `UPDATE settlements SET amount_enc = ?, note_enc = ?, amount_cents = 1, note = '', enc_version = 1 WHERE id = ?`, aEnc, nEnc, st.id); err != nil {
			return err
		}
	}

	// subscriptions
	subRows, err := tx.QueryContext(ctx, `SELECT id, name, amount_cents FROM subscriptions WHERE household_id = ? AND enc_version = 0`, hhID)
	if err != nil {
		return err
	}
	type subRow struct {
		id   int64
		name string
		amt  int64
	}
	var subs []subRow
	for subRows.Next() {
		var sb subRow
		if err := subRows.Scan(&sb.id, &sb.name, &sb.amt); err != nil {
			subRows.Close()
			return err
		}
		subs = append(subs, sb)
	}
	subRows.Close()
	for _, sb := range subs {
		nEnc, _ := crypto.Encrypt(dek, crypto.AAD(hhID, "subscriptions", sb.id, "name"), []byte(sb.name))
		aEnc, _ := crypto.EncryptInt64(dek, crypto.AAD(hhID, "subscriptions", sb.id, "amount"), sb.amt)
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET name_enc = ?, amount_enc = ?, name = '', amount_cents = 1, enc_version = 1 WHERE id = ?`, nEnc, aEnc, sb.id); err != nil {
			return err
		}
	}

	// merchant_category_rules: convert plaintext normalized_merchant to BlindIndex if not 64-char hex
	mcRows, err := tx.QueryContext(ctx, `SELECT normalized_merchant, category_id, updated_at FROM merchant_category_rules WHERE household_id = ? AND length(normalized_merchant) != 64`, hhID)
	if err == nil {
		type mcRow struct {
			m, upd string
			cid    int64
		}
		var mcs []mcRow
		for mcRows.Next() {
			var m mcRow
			if err := mcRows.Scan(&m.m, &m.cid, &m.upd); err != nil {
				break
			}
			mcs = append(mcs, m)
		}
		mcRows.Close()
		for _, m := range mcs {
			hm := crypto.BlindIndex(dek, "merchant", m.m)
			_, _ = tx.ExecContext(ctx, `DELETE FROM merchant_category_rules WHERE household_id = ? AND normalized_merchant = ?`, hhID, m.m)
			_, _ = tx.ExecContext(ctx, `INSERT OR REPLACE INTO merchant_category_rules (household_id, normalized_merchant, category_id, updated_at) VALUES (?, ?, ?, ?)`, hhID, hm, m.cid, m.upd)
		}
	}

	return nil
}

func reencryptReceiptsTx(ctx context.Context, tx *sql.Tx, hhID int64, oldDEK, newDEK []byte) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, data_enc FROM receipt_images WHERE household_id = ? AND enc_version = 1`, hhID)
	if err != nil {
		return err
	}
	type item struct {
		id int64
		ct []byte
	}
	var list []item
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.ct); err != nil {
			rows.Close()
			return err
		}
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		pt, err := crypto.Decrypt(oldDEK, crypto.AAD(hhID, "receipt_images", x.id, "data"), x.ct)
		if err != nil {
			return err
		}
		nct, err := crypto.Encrypt(newDEK, crypto.AAD(hhID, "receipt_images", x.id, "data"), pt)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE receipt_images SET data_enc = ? WHERE id = ?`, nct, x.id); err != nil {
			return err
		}
	}
	return nil
}

func reencryptDraftsTx(ctx context.Context, tx *sql.Tx, hhID int64, oldDEK, newDEK []byte) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, merchant_enc, amount_enc, ocr_error_enc FROM receipt_drafts WHERE household_id = ? AND enc_version = 1`, hhID)
	if err != nil {
		return err
	}
	type item struct {
		id         string
		m, a, e    []byte
	}
	var list []item
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.m, &x.a, &x.e); err != nil {
			rows.Close()
			return err
		}
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		mPt, err := crypto.Decrypt(oldDEK, crypto.AAD(hhID, "receipt_drafts", x.id, "merchant"), x.m)
		if err != nil {
			return err
		}
		aPt, err := crypto.DecryptInt64(oldDEK, crypto.AAD(hhID, "receipt_drafts", x.id, "amount"), x.a)
		if err != nil {
			return err
		}
		ePt, err := crypto.Decrypt(oldDEK, crypto.AAD(hhID, "receipt_drafts", x.id, "ocr_error"), x.e)
		if err != nil {
			return err
		}
		mNew, _ := crypto.Encrypt(newDEK, crypto.AAD(hhID, "receipt_drafts", x.id, "merchant"), mPt)
		aNew, _ := crypto.EncryptInt64(newDEK, crypto.AAD(hhID, "receipt_drafts", x.id, "amount"), aPt)
		eNew, _ := crypto.Encrypt(newDEK, crypto.AAD(hhID, "receipt_drafts", x.id, "ocr_error"), ePt)
		if _, err := tx.ExecContext(ctx, `UPDATE receipt_drafts SET merchant_enc = ?, amount_enc = ?, ocr_error_enc = ? WHERE id = ?`, mNew, aNew, eNew, x.id); err != nil {
			return err
		}
	}
	return nil
}

func reencryptExpensesTx(ctx context.Context, tx *sql.Tx, hhID int64, oldDEK, newDEK []byte) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, merchant_enc, note_enc, amount_enc FROM expenses WHERE household_id = ? AND enc_version = 1`, hhID)
	if err != nil {
		return err
	}
	type item struct {
		id      int64
		m, n, a []byte
	}
	var list []item
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.m, &x.n, &x.a); err != nil {
			rows.Close()
			return err
		}
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		mPt, err := crypto.Decrypt(oldDEK, crypto.AAD(hhID, "expenses", x.id, "merchant"), x.m)
		if err != nil {
			return err
		}
		nPt, err := crypto.Decrypt(oldDEK, crypto.AAD(hhID, "expenses", x.id, "note"), x.n)
		if err != nil {
			return err
		}
		aPt, err := crypto.DecryptInt64(oldDEK, crypto.AAD(hhID, "expenses", x.id, "amount"), x.a)
		if err != nil {
			return err
		}
		mNew, _ := crypto.Encrypt(newDEK, crypto.AAD(hhID, "expenses", x.id, "merchant"), mPt)
		nNew, _ := crypto.Encrypt(newDEK, crypto.AAD(hhID, "expenses", x.id, "note"), nPt)
		aNew, _ := crypto.EncryptInt64(newDEK, crypto.AAD(hhID, "expenses", x.id, "amount"), aPt)
		hmNew := crypto.BlindIndex(newDEK, "merchant", categorize.NormalizeMerchant(string(mPt)))
		if _, err := tx.ExecContext(ctx, `UPDATE expenses SET merchant_enc = ?, note_enc = ?, amount_enc = ?, normalized_merchant = ? WHERE id = ?`, mNew, nNew, aNew, hmNew, x.id); err != nil {
			return err
		}
	}

	spRows, err := tx.QueryContext(ctx, `SELECT sp.expense_id, sp.user_id, sp.share_enc FROM expense_splits sp JOIN expenses e ON e.id = sp.expense_id WHERE e.household_id = ? AND sp.enc_version = 1`, hhID)
	if err != nil {
		return err
	}
	type spItem struct {
		eid, uid int64
		ct       []byte
	}
	var spList []spItem
	for spRows.Next() {
		var x spItem
		if err := spRows.Scan(&x.eid, &x.uid, &x.ct); err != nil {
			spRows.Close()
			return err
		}
		spList = append(spList, x)
	}
	spRows.Close()
	for _, x := range spList {
		aad := crypto.AAD(hhID, "expense_splits", fmt.Sprintf("%d:%d", x.eid, x.uid), "share")
		pt, err := crypto.DecryptInt64(oldDEK, aad, x.ct)
		if err != nil {
			return err
		}
		nct, _ := crypto.EncryptInt64(newDEK, aad, pt)
		if _, err := tx.ExecContext(ctx, `UPDATE expense_splits SET share_enc = ? WHERE expense_id = ? AND user_id = ?`, nct, x.eid, x.uid); err != nil {
			return err
		}
	}
	return nil
}

func reencryptSettlementsTx(ctx context.Context, tx *sql.Tx, hhID int64, oldDEK, newDEK []byte) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, amount_enc, note_enc FROM settlements WHERE household_id = ? AND enc_version = 1`, hhID)
	if err != nil {
		return err
	}
	type item struct {
		id   int64
		a, n []byte
	}
	var list []item
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.a, &x.n); err != nil {
			rows.Close()
			return err
		}
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		aPt, err := crypto.DecryptInt64(oldDEK, crypto.AAD(hhID, "settlements", x.id, "amount"), x.a)
		if err != nil {
			return err
		}
		nPt, err := crypto.Decrypt(oldDEK, crypto.AAD(hhID, "settlements", x.id, "note"), x.n)
		if err != nil {
			return err
		}
		aNew, _ := crypto.EncryptInt64(newDEK, crypto.AAD(hhID, "settlements", x.id, "amount"), aPt)
		nNew, _ := crypto.Encrypt(newDEK, crypto.AAD(hhID, "settlements", x.id, "note"), nPt)
		if _, err := tx.ExecContext(ctx, `UPDATE settlements SET amount_enc = ?, note_enc = ? WHERE id = ?`, aNew, nNew, x.id); err != nil {
			return err
		}
	}
	return nil
}

func reencryptSubscriptionsTx(ctx context.Context, tx *sql.Tx, hhID int64, oldDEK, newDEK []byte) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, name_enc, amount_enc FROM subscriptions WHERE household_id = ? AND enc_version = 1`, hhID)
	if err != nil {
		return err
	}
	type item struct {
		id   int64
		n, a []byte
	}
	var list []item
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.n, &x.a); err != nil {
			rows.Close()
			return err
		}
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		nPt, err := crypto.Decrypt(oldDEK, crypto.AAD(hhID, "subscriptions", x.id, "name"), x.n)
		if err != nil {
			return err
		}
		aPt, err := crypto.DecryptInt64(oldDEK, crypto.AAD(hhID, "subscriptions", x.id, "amount"), x.a)
		if err != nil {
			return err
		}
		nNew, _ := crypto.Encrypt(newDEK, crypto.AAD(hhID, "subscriptions", x.id, "name"), nPt)
		aNew, _ := crypto.EncryptInt64(newDEK, crypto.AAD(hhID, "subscriptions", x.id, "amount"), aPt)
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET name_enc = ?, amount_enc = ? WHERE id = ?`, nNew, aNew, x.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) LogAudit(ctx context.Context, event string, userID, householdID *int64, detail string) error {
	if s.DB == nil {
		return errors.New("db nil")
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO audit_logs (event, user_id, household_id, detail, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, event, userID, householdID, detail, time.Now().UTC().Format(time.RFC3339))
	return err
}
