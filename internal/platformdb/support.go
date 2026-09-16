package platformdb

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ekucher/bsystem-integration-core/internal/support"
)

// ErrSupportRecordNotFound means no support record carries that Global ID.
var ErrSupportRecordNotFound = errors.New("support record not found")

const supportColumns = `
 global_id, kind, title, summary, severity, status, COALESCE(client_id,''),
 reported_by, created_at, updated_at, acknowledged_at, resolved_at`

func scanSupportRecord(row pgx.Row) (support.Record, error) {
	var record support.Record
	err := row.Scan(&record.ID, &record.Kind, &record.Title, &record.Summary,
		&record.Severity, &record.Status, &record.ClientID, &record.ReportedBy,
		&record.CreatedAt, &record.UpdatedAt, &record.AcknowledgedAt, &record.ResolvedAt)
	if err != nil {
		return support.Record{}, err
	}
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	record.AcknowledgedAt = utcOrNil(record.AcknowledgedAt)
	record.ResolvedAt = utcOrNil(record.ResolvedAt)
	record.Relations = []support.Relation{}
	return record, nil
}

func utcOrNil(moment *time.Time) *time.Time {
	if moment == nil {
		return nil
	}
	utc := moment.UTC()
	return &utc
}

// CreateSupportRecord stores a record and its relations in one transaction.
func (db *DB) CreateSupportRecord(ctx context.Context, record support.Record) (support.Record, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return support.Record{}, err
	}
	defer tx.Rollback(ctx)

	stored, err := scanSupportRecord(tx.QueryRow(ctx, `
INSERT INTO support_records (global_id,kind,title,summary,severity,status,client_id,reported_by)
VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)
RETURNING`+supportColumns,
		record.ID, record.Kind, record.Title, record.Summary, record.Severity,
		record.Status, record.ClientID, record.ReportedBy))
	if err != nil {
		return support.Record{}, err
	}

	for _, relation := range record.Relations {
		if _, err := tx.Exec(ctx, `
INSERT INTO support_relations (support_id,entity_type,global_id) VALUES ($1,$2,$3)
ON CONFLICT DO NOTHING`, stored.ID, relation.EntityType, relation.GlobalID); err != nil {
			return support.Record{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return support.Record{}, err
	}
	stored.Relations = record.Relations
	if stored.Relations == nil {
		stored.Relations = []support.Relation{}
	}
	return stored, nil
}

// SupportUpdate is the set of fields an update may change. A nil field is one
// the caller did not mention, which is distinct from one they cleared.
type SupportUpdate struct {
	Title    *string
	Summary  *string
	Severity *string
	Status   *string
	// Acknowledge and Resolve record the SLA moments. They are set only when
	// the status first reaches them, and never moved again: reopening a
	// resolved record must not erase that it was once resolved on time.
	Acknowledge bool
	Resolve     bool
	// Relations replaces the whole set when non-nil.
	Relations []support.Relation
}

// UpdateSupportRecord applies an update and returns the stored record.
func (db *DB) UpdateSupportRecord(ctx context.Context, globalID string, update SupportUpdate) (support.Record, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return support.Record{}, err
	}
	defer tx.Rollback(ctx)

	record, err := scanSupportRecord(tx.QueryRow(ctx, `
UPDATE support_records SET
 title = COALESCE($2, title),
 summary = COALESCE($3, summary),
 severity = COALESCE($4, severity),
 status = COALESCE($5, status),
 acknowledged_at = CASE WHEN $6 AND acknowledged_at IS NULL THEN now() ELSE acknowledged_at END,
 resolved_at = CASE WHEN $7 AND resolved_at IS NULL THEN now() ELSE resolved_at END,
 updated_at = now()
WHERE global_id = $1
RETURNING`+supportColumns,
		globalID, update.Title, update.Summary, update.Severity, update.Status,
		update.Acknowledge, update.Resolve))
	if errors.Is(err, pgx.ErrNoRows) {
		return support.Record{}, ErrSupportRecordNotFound
	}
	if err != nil {
		return support.Record{}, err
	}

	if update.Relations != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM support_relations WHERE support_id=$1`, globalID); err != nil {
			return support.Record{}, err
		}
		for _, relation := range update.Relations {
			if _, err := tx.Exec(ctx, `
INSERT INTO support_relations (support_id,entity_type,global_id) VALUES ($1,$2,$3)
ON CONFLICT DO NOTHING`, globalID, relation.EntityType, relation.GlobalID); err != nil {
				return support.Record{}, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return support.Record{}, err
	}

	relations, err := db.supportRelations(ctx, globalID)
	if err != nil {
		return support.Record{}, err
	}
	record.Relations = relations
	return record, nil
}

// GetSupportRecord returns one record with its relations.
func (db *DB) GetSupportRecord(ctx context.Context, globalID string) (support.Record, error) {
	record, err := scanSupportRecord(db.pool.QueryRow(ctx,
		`SELECT`+supportColumns+` FROM support_records WHERE global_id=$1`, strings.TrimSpace(globalID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return support.Record{}, ErrSupportRecordNotFound
	}
	if err != nil {
		return support.Record{}, err
	}
	relations, err := db.supportRelations(ctx, record.ID)
	if err != nil {
		return support.Record{}, err
	}
	record.Relations = relations
	return record, nil
}

func (db *DB) supportRelations(ctx context.Context, globalID string) ([]support.Relation, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT entity_type, global_id FROM support_relations WHERE support_id=$1 ORDER BY entity_type, global_id`, globalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	relations := []support.Relation{}
	for rows.Next() {
		var relation support.Relation
		if err := rows.Scan(&relation.EntityType, &relation.GlobalID); err != nil {
			return nil, err
		}
		relations = append(relations, relation)
	}
	return relations, rows.Err()
}

// SupportQuery bounds a support listing.
type SupportQuery struct {
	Kind     string
	Status   string
	Severity string
	ClientID string
	// OpenOnly restricts to records still being worked.
	OpenOnly bool
	AfterID  string
	Limit    int
}

// ListSupportRecords returns one page ordered by Global ID, with the total.
//
// Relations are not loaded for a listing. A list is read to find a record,
// and fetching every relation for every row would make the common case pay
// for the uncommon one.
func (db *DB) ListSupportRecords(ctx context.Context, query SupportQuery) ([]support.Record, int, error) {
	const filter = `
WHERE ($1 = '' OR kind = $1)
  AND ($2 = '' OR status = $2)
  AND ($3 = '' OR severity = $3)
  AND ($4 = '' OR client_id = $4)
  AND (NOT $5 OR status NOT IN ('resolved','closed'))`

	var total int
	if err := db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM support_records`+filter,
		query.Kind, query.Status, query.Severity, query.ClientID, query.OpenOnly).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := db.pool.Query(ctx, `SELECT`+supportColumns+` FROM support_records`+filter+`
  AND ($6 = '' OR global_id > $6)
ORDER BY global_id
LIMIT $7`, query.Kind, query.Status, query.Severity, query.ClientID, query.OpenOnly,
		query.AfterID, query.Limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	records := []support.Record{}
	for rows.Next() {
		record, err := scanSupportRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, record)
	}
	return records, total, rows.Err()
}

// SLAPolicies returns the configured targets by severity. An absent severity
// has no policy, which is reported as an unset SLA rather than as on track.
func (db *DB) SLAPolicies(ctx context.Context) (map[string]support.Policy, error) {
	rows, err := db.pool.Query(ctx, `SELECT severity, respond_minutes, resolve_minutes FROM support_sla_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := map[string]support.Policy{}
	for rows.Next() {
		var severity string
		var respond, resolve int
		if err := rows.Scan(&severity, &respond, &resolve); err != nil {
			return nil, err
		}
		policies[severity] = support.Policy{
			Respond: time.Duration(respond) * time.Minute,
			Resolve: time.Duration(resolve) * time.Minute,
		}
	}
	return policies, rows.Err()
}
