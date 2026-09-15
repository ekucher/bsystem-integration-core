package platformdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (db *DB) EnsureIdentity(ctx context.Context, identity Identity) (string, bool, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)

	var globalID string
	err = tx.QueryRow(ctx, `SELECT global_user_id FROM identities WHERE subject=$1 FOR UPDATE`, identity.Subject).Scan(&globalID)
	created := false
	if errors.Is(err, pgx.ErrNoRows) {
		var prefix string
		var value int64
		if err := tx.QueryRow(ctx, `UPDATE global_id_counters SET next_value=next_value+1 WHERE entity_type='user' RETURNING prefix,next_value-1`).Scan(&prefix, &value); err != nil {
			return "", false, fmt.Errorf("allocate user Global ID: %w", err)
		}
		globalID = fmt.Sprintf("%s-%06d", prefix, value)
		created = true
	} else if err != nil {
		return "", false, err
	}

	groups, _ := json.Marshal(identity.Groups)
	_, err = tx.Exec(ctx, `
INSERT INTO identities (subject,global_user_id,email,display_name,username,groups_json)
VALUES ($1,$2,$3,$4,$5,$6::jsonb)
ON CONFLICT (subject) DO UPDATE SET
 email=EXCLUDED.email,
 display_name=EXCLUDED.display_name,
 username=EXCLUDED.username,
 groups_json=EXCLUDED.groups_json,
 last_seen_at=now()`, identity.Subject, globalID, identity.Email, identity.DisplayName, identity.Username, string(groups))
	if err != nil {
		return "", false, err
	}

	if created {
		_, err = tx.Exec(ctx, `
INSERT INTO global_entities (global_id,entity_type,source,source_id,metadata)
VALUES ($1,'user','authentik',$2,$3::jsonb)
ON CONFLICT (source,entity_type,source_id) DO NOTHING`, globalID, identity.Subject, `{"managed_by":"identity"}`)
		if err != nil {
			return "", false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return globalID, created, nil
}
