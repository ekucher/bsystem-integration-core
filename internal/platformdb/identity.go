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

	// Serialize first-seen allocation for the same OIDC subject without a global lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, identity.Subject); err != nil {
		return "", false, err
	}

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

	// The announcement joins the allocation's transaction. A USR-* is minted
	// once in the lifetime of an OIDC subject and cannot be re-derived from a
	// later event, so a consumer that misses this one has no second chance at
	// it. Queueing it here means it exists if and only if the identity does.
	if created {
		if err := queueDurableEvent(ctx, tx, "identity.created", globalID, "", map[string]any{
			"global_user_id": globalID,
			"subject":        identity.Subject,
		}); err != nil {
			return "", false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return globalID, created, nil
}

func (db *DB) ListIdentities(ctx context.Context) ([]IdentityView, error) {
	rows, err := db.pool.Query(ctx, `
SELECT
    global_user_id,
    subject,
    email,
    display_name,
    username,
    groups_json,
    first_seen_at,
    last_seen_at
FROM identities
ORDER BY username, global_user_id
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []IdentityView{}
	for rows.Next() {
		var item IdentityView
		var groupsJSON []byte
		if err := rows.Scan(
			&item.ID,
			&item.Subject,
			&item.Email,
			&item.DisplayName,
			&item.Username,
			&groupsJSON,
			&item.FirstSeenAt,
			&item.LastSeenAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(groupsJSON, &item.Groups); err != nil {
			return nil, fmt.Errorf("decode identity groups: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

