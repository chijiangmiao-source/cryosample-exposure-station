// Cabinet location occupancy: an independent module on top of the same
// SQLite database. It tracks which cabinet slot currently holds which
// in-cabinet batch, separately from the exposure state machine:
//
//   - a batch occupies at most one location and a location holds at most one
//     batch (enforced by the location_occupancy table's PRIMARY KEY and the
//     UNIQUE constraint on location);
//   - a batch outside the cabinet, a freshly returned batch and a not-yet-
//     shelved batch simply have no occupancy row;
//   - the takeout/return event transactions delete the row themselves
//     (see store.go), so the occupancy can never dangle against a batch that
//     is out of the cabinet.
//
// PutLocation performs the first placement or a move atomically; every
// rejected request rolls its whole transaction back, leaving the previous
// occupancy (as well as the event log and accumulated exposure) untouched.
package store

import (
	"context"
)

// PutLocation places or moves an in-cabinet, usable (not scrapped and not
// outside the cabinet) batch into the given location in one transaction. The
// first placement inserts the occupancy row; a move replaces it. The table's
// PRIMARY KEY / UNIQUE constraints guarantee that two batches racing for the
// same location can never both win.
//
// It fails with a ConflictError when:
//   - the batch is outside the cabinet (batch_not_in_cabinet),
//   - the batch is scrapped (batch_scrapped),
//   - the target slot is held by another batch (location_occupied),
//     including when the slot was grabbed concurrently.
//
// Putting the batch into the slot it already holds is an idempotent success.
// Every failure rolls the whole transaction back: the previous occupancy (if
// any), the event log and the accumulated total are untouched.
func (s *Store) PutLocation(ctx context.Context, barcode, location string) (*Batch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	b, err := scanBatch(tx.QueryRowContext(ctx, batchSelect, barcode))
	if err != nil {
		return nil, err
	}
	if b.State != StateIn {
		return nil, conflict("batch_not_in_cabinet",
			"batch %s is outside the cabinet and cannot be placed until it is returned", barcode)
	}
	if b.Status == StatusScrapped {
		return nil, conflict("batch_scrapped",
			"scrapped batch %s cannot occupy a cabinet location", barcode)
	}

	// Moving: release the old slot inside the same transaction so a rejected
	// move rolls back and leaves the old occupancy exactly as it was. A first
	// placement (and an idempotent same-slot PUT) has nothing to delete.
	current := b.Location
	if current != nil && *current != location {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM location_occupancy WHERE barcode = ?`, barcode); err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO location_occupancy (barcode, location) VALUES (?, ?)`,
		barcode, location); err != nil {
		if isUniqueViolation(err) {
			// A duplicate PUT into the slot the batch already holds is an
			// idempotent success; end the untouched transaction first so the
			// single SQLite connection is free again.
			if current != nil && *current == location {
				if rerr := tx.Rollback(); rerr != nil {
					return nil, rerr
				}
				return b, nil
			}
			var holder string
			if qerr := tx.QueryRowContext(ctx,
				`SELECT barcode FROM location_occupancy WHERE location = ?`, location).
				Scan(&holder); qerr == nil && holder != barcode {
				return nil, conflict("location_occupied",
					"location %s is already occupied by batch %s", location, holder)
			}
			return nil, conflict("location_occupied",
				"location %s was taken by another batch concurrently", location)
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBatch(ctx, barcode)
}

// ReleaseLocation vacates the batch's slot (the independent 腾空 operation).
// It conflicts with location_not_occupied when the batch currently holds no
// slot; a released slot can immediately be taken by placing another batch.
// A takeout releases the slot implicitly through its own event transaction.
func (s *Store) ReleaseLocation(ctx context.Context, barcode string) (*Batch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	b, err := scanBatch(tx.QueryRowContext(ctx, batchSelect, barcode))
	if err != nil {
		return nil, err
	}
	if b.Location == nil {
		return nil, conflict("location_not_occupied", "batch %s currently occupies no location", barcode)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM location_occupancy WHERE barcode = ?`, barcode); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBatch(ctx, barcode)
}
