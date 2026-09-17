package platformdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type ServiceIdentity struct {
	Subject  string
	Name     string
	Username string
	Groups   []string
}

func (db *DB) EnsureServiceIdentity(ctx context.Context, identity ServiceIdentity) (string, bool, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)

	// Serialize first-seen allocation for the same subject, as EnsureIdentity
	// does for human identities. SELECT ... FOR UPDATE below locks a row, and
	// on a first sighting there is no row to lock: every concurrent
	// transaction sees no rows, every one of them bumps the counter, and each
	// caller is handed a different Global ID for the same service. The upsert
	// that follows then lets the last writer win, so the earlier callers walk
	// away holding identifiers the platform does not recognise — and each was
	// told it had created the identity.
	//
	// Service identities make this the normal case rather than the unlucky
	// one: an integration that starts several replicas registers from all of
	// them at once.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, identity.Subject); err != nil {
		return "", false, err
	}

	var globalID string
	err = tx.QueryRow(ctx, `SELECT global_service_id FROM service_identities WHERE subject=$1 FOR UPDATE`, identity.Subject).Scan(&globalID)
	created := false
	if errors.Is(err, pgx.ErrNoRows) {
		var prefix string
		var value int64
		if err := tx.QueryRow(ctx, `UPDATE global_id_counters SET next_value=next_value+1 WHERE entity_type='service' RETURNING prefix,next_value-1`).Scan(&prefix, &value); err != nil {
			return "", false, fmt.Errorf("allocate service Global ID: %w", err)
		}
		globalID = fmt.Sprintf("%s-%06d", prefix, value)
		created = true
	} else if err != nil {
		return "", false, err
	}

	groups, _ := json.Marshal(identity.Groups)
	_, err = tx.Exec(ctx, `
INSERT INTO service_identities (subject,global_service_id,name,username,groups_json)
VALUES ($1,$2,$3,$4,$5::jsonb)
ON CONFLICT (subject) DO UPDATE SET
 name=EXCLUDED.name,
 username=EXCLUDED.username,
 groups_json=EXCLUDED.groups_json,
 last_seen_at=now()`, identity.Subject, globalID, identity.Name, identity.Username, string(groups))
	if err != nil {
		return "", false, err
	}

	if created {
		_, err = tx.Exec(ctx, `
INSERT INTO global_entities (global_id,entity_type,source,source_id,metadata)
VALUES ($1,'service','authentik',$2,$3::jsonb)
ON CONFLICT (source,entity_type,source_id) DO NOTHING`, globalID, identity.Subject, `{"managed_by":"service_identity"}`)
		if err != nil {
			return "", false, err
		}
		// A machine identity appearing for the first time is a security event
		// as much as an operational one, and it happens once per subject
		// ever. Same transaction, same reason as identity.created.
		if err := queueDurableEvent(ctx, tx, "service_identity.created", globalID, "", map[string]any{
			"global_service_id": globalID,
			"subject":           identity.Subject,
		}); err != nil {
			return "", false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return globalID, created, nil
}
