// Backup barcodes (aliases): alternate labels bound to an existing canonical
// batch. Scanning either the primary barcode or any bound alias reaches the
// same batch aggregate, so exposure timing, revocation and location records
// stay shared. Aliases persist in the batch_codes namespace ledger (see
// store.go): a code is globally unique and may never equal any primary
// barcode, which the schema's PRIMARY KEY guarantees even under concurrency.
//
// BindAlias adds one alias inside a transaction; UnbindAlias removes only an
// alias owned by that batch. Neither operation touches events, accumulated
// exposure or location occupancy.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BindAlias binds alias as a backup barcode for the (canonical) batch barcode
// inside a single transaction. It fails with a ConflictError when:
//   - alias equals an existing primary barcode (alias_conflicts_barcode),
//     including the batch's own primary barcode;
//   - alias is already bound, to this batch (duplicate_alias) or another one
//     (alias_bound_elsewhere); the concurrent racing loser hits the same
//     codes via the namespace PRIMARY KEY.
//
// The returned aliases are all backup codes of the batch after the bind.
func (s *Store) BindAlias(ctx context.Context, barcode, alias string) (*Batch, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	if _, err := scanBatch(tx.QueryRowContext(ctx, batchSelect, barcode)); err != nil {
		return nil, nil, err
	}
	if err := classifyCode(ctx, tx, alias, barcode); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO batch_codes (code, barcode, kind) VALUES (?, ?, 'alias')`,
		alias, barcode); err != nil {
		if !isUniqueViolation(err) {
			return nil, nil, err
		}
		// Lost a concurrent race for the same code: re-classify the winner.
		if err := classifyCode(ctx, tx, alias, barcode); err != nil {
			return nil, nil, err
		}
		return nil, nil, conflict("duplicate_alias", "code %s was taken concurrently", alias)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	aliases, err := s.ListAliases(ctx, barcode)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.GetBatch(ctx, barcode)
	return b, aliases, err
}

// classifyCode returns a *ConflictError when code already occupies the shared
// namespace, nil when it is free for a new alias of barcode, or a plain error
// on a failed lookup.
func classifyCode(ctx context.Context, tx *sql.Tx, code, barcode string) error {
	var owner, kind string
	err := tx.QueryRowContext(ctx,
		`SELECT barcode, kind FROM batch_codes WHERE code = ?`, code).Scan(&owner, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("namespace lookup failed: %w", err)
	}
	switch kind {
	case "primary":
		return conflict("alias_conflicts_barcode",
			"code %s is already the primary barcode of batch %s and cannot be used as an alias", code, owner)
	default: // "alias"
		if owner == barcode {
			return conflict("duplicate_alias",
				"code %s is already bound as a backup barcode of this batch", code)
		}
		return conflict("alias_bound_elsewhere",
			"code %s is already bound as a backup barcode of another batch %s", code, owner)
	}
}

// UnbindAlias removes alias from the batch inside a single transaction. It
// fails with alias_not_bound when the code does not exist, is a primary
// barcode, or belongs to a different batch: the predicate pins ownership so a
// foreign alias can never be deleted. Events, accumulated exposure and
// location occupancy are never touched. The returned aliases are the
// remaining backup codes of the batch.
func (s *Store) UnbindAlias(ctx context.Context, barcode, alias string) (*Batch, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	if _, err := scanBatch(tx.QueryRowContext(ctx, batchSelect, barcode)); err != nil {
		return nil, nil, err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM batch_codes WHERE code = ? AND barcode = ? AND kind = 'alias'`,
		alias, barcode)
	if err != nil {
		return nil, nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return nil, nil, conflict("alias_not_bound",
			"code %s does not exist or is not a backup barcode of this batch", alias)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	aliases, err := s.ListAliases(ctx, barcode)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.GetBatch(ctx, barcode)
	return b, aliases, err
}

// ListAliases returns all backup barcodes of the batch in lexical order. A
// batch without aliases returns an empty (non-nil) slice.
func (s *Store) ListAliases(ctx context.Context, barcode string) ([]string, error) {
	if _, err := s.GetBatch(ctx, barcode); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT code FROM batch_codes WHERE barcode = ? AND kind = 'alias' ORDER BY code`,
		barcode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	return out, rows.Err()
}

// ResolveCode maps any scannable code (a primary barcode or a bound backup
// barcode) to the canonical batch barcode. It returns ErrNotFound when the
// code exists in neither role.
func (s *Store) ResolveCode(ctx context.Context, code string) (string, error) {
	var barcode string
	err := s.db.QueryRowContext(ctx,
		`SELECT barcode FROM batch_codes WHERE code = ?`, code).Scan(&barcode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return barcode, err
}
