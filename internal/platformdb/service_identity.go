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
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return globalID, created, nil
}
