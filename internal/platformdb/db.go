package platformdb

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type DB struct {
	pool *pgxpool.Pool
}

type Identity struct {
	Subject      string
	GlobalUserID string
	Email        string
	DisplayName  string
	Username     string
	Groups       []string
}

type Module struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type GlobalEntity struct {
	GlobalID   string         `json:"global_id"`
	EntityType string         `json:"entity_type"`
	Source     string         `json:"source"`
	SourceID   string         `json:"source_id"`
	TenantID   string         `json:"tenant_id,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

type AuditEvent struct {
	ID           int64          `json:"id"`
	OccurredAt   time.Time      `json:"occurred_at"`
	Subject      string         `json:"subject"`
	GlobalUserID string         `json:"global_user_id"`
	Action       string         `json:"action"`
	ResourceType string         `json:"resource_type"`
	ResourceID   string         `json:"resource_id"`
	RequestID    string         `json:"request_id"`
	SourceIP     string         `json:"source_ip"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

func Open(ctx context.Context, databaseURL string) (*DB, error) {
	if databaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	db := &DB{pool: pool}
	if err := db.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close()                         { db.pool.Close() }
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

func (db *DB) Migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := db.pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

func (db *DB) UpsertIdentity(ctx context.Context, identity Identity) error {
	groups, _ := json.Marshal(identity.Groups)
	_, err := db.pool.Exec(ctx, `
INSERT INTO identities (subject, global_user_id, email, display_name, username, groups_json)
VALUES ($1,$2,$3,$4,$5,$6::jsonb)
ON CONFLICT (subject) DO UPDATE SET
 global_user_id=EXCLUDED.global_user_id,
 email=EXCLUDED.email,
 display_name=EXCLUDED.display_name,
 username=EXCLUDED.username,
 groups_json=EXCLUDED.groups_json,
 last_seen_at=now()`, identity.Subject, identity.GlobalUserID, identity.Email, identity.DisplayName, identity.Username, string(groups))
	return err
}

func (db *DB) ListModules(ctx context.Context) ([]Module, error) {
	rows, err := db.pool.Query(ctx, `SELECT id,name,description,status FROM modules WHERE enabled=TRUE ORDER BY sort_order,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Module
	for rows.Next() {
		var item Module
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.Status); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// LookupGlobalEntity returns an existing mapping without allocating one.
//
// Read paths use it so that reporting a relationship cannot mint a Global ID
// as a side effect: an unmapped reference stays unmapped until something
// deliberately maps it.
func (db *DB) LookupGlobalEntity(ctx context.Context, entityType, source, sourceID string) (GlobalEntity, error) {
	var entity GlobalEntity
	var metadata []byte
	err := db.pool.QueryRow(ctx, `SELECT global_id,entity_type,source,source_id,COALESCE(tenant_id,''),metadata,created_at FROM global_entities WHERE source=$1 AND entity_type=$2 AND source_id=$3`, source, entityType, sourceID).
		Scan(&entity.GlobalID, &entity.EntityType, &entity.Source, &entity.SourceID, &entity.TenantID, &metadata, &entity.CreatedAt)
	if err != nil {
		return GlobalEntity{}, err
	}
	_ = json.Unmarshal(metadata, &entity.Metadata)
	return entity, nil
}

func (db *DB) CreateGlobalEntity(ctx context.Context, entityType, source, sourceID, tenantID string, metadata map[string]any) (GlobalEntity, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return GlobalEntity{}, err
	}
	defer tx.Rollback(ctx)

	var existing GlobalEntity
	var existingMeta []byte
	err = tx.QueryRow(ctx, `SELECT global_id,entity_type,source,source_id,COALESCE(tenant_id,''),metadata,created_at FROM global_entities WHERE source=$1 AND entity_type=$2 AND source_id=$3`, source, entityType, sourceID).
		Scan(&existing.GlobalID, &existing.EntityType, &existing.Source, &existing.SourceID, &existing.TenantID, &existingMeta, &existing.CreatedAt)
	if err == nil {
		_ = json.Unmarshal(existingMeta, &existing.Metadata)
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return GlobalEntity{}, err
	}

	var prefix string
	var value int64
	err = tx.QueryRow(ctx, `UPDATE global_id_counters SET next_value=next_value+1 WHERE entity_type=$1 RETURNING prefix,next_value-1`, entityType).Scan(&prefix, &value)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GlobalEntity{}, fmt.Errorf("unsupported entity_type %q", entityType)
		}
		return GlobalEntity{}, err
	}
	globalID := fmt.Sprintf("%s-%06d", prefix, value)
	metaJSON, _ := json.Marshal(metadata)
	var created time.Time
	err = tx.QueryRow(ctx, `INSERT INTO global_entities (global_id,entity_type,source,source_id,tenant_id,metadata) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6::jsonb) RETURNING created_at`, globalID, entityType, source, sourceID, tenantID, string(metaJSON)).Scan(&created)
	if err != nil {
		return GlobalEntity{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GlobalEntity{}, err
	}
	return GlobalEntity{GlobalID: globalID, EntityType: entityType, Source: source, SourceID: sourceID, TenantID: tenantID, Metadata: metadata, CreatedAt: created}, nil
}

func (db *DB) ResolveGlobalEntity(ctx context.Context, globalID string) (GlobalEntity, error) {
	var item GlobalEntity
	var meta []byte
	err := db.pool.QueryRow(ctx, `SELECT global_id,entity_type,source,source_id,COALESCE(tenant_id,''),metadata,created_at FROM global_entities WHERE global_id=$1`, globalID).
		Scan(&item.GlobalID, &item.EntityType, &item.Source, &item.SourceID, &item.TenantID, &meta, &item.CreatedAt)
	if err != nil {
		return GlobalEntity{}, err
	}
	_ = json.Unmarshal(meta, &item.Metadata)
	return item, nil
}

func (db *DB) InsertAudit(ctx context.Context, event AuditEvent) error {
	metaJSON, _ := json.Marshal(event.Metadata)
	_, err := db.pool.Exec(ctx, `INSERT INTO audit_events (subject,global_user_id,action,resource_type,resource_id,request_id,source_ip,metadata) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, event.Subject, event.GlobalUserID, event.Action, event.ResourceType, event.ResourceID, event.RequestID, event.SourceIP, string(metaJSON))
	return err
}

func (db *DB) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx, `SELECT id,occurred_at,subject,global_user_id,action,resource_type,resource_id,request_id,source_ip,metadata FROM audit_events ORDER BY occurred_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AuditEvent
	for rows.Next() {
		var item AuditEvent
		var meta []byte
		if err := rows.Scan(&item.ID, &item.OccurredAt, &item.Subject, &item.GlobalUserID, &item.Action, &item.ResourceType, &item.ResourceID, &item.RequestID, &item.SourceIP, &meta); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(meta, &item.Metadata)
		result = append(result, item)
	}
	return result, rows.Err()
}

// PoolStats describes the connection pool, for metrics.
//
// It is read from the pool at scrape time rather than mirrored into counters,
// because a mirrored value drifts out of date exactly when it matters.
type PoolStats struct {
	Acquired          int32
	Idle              int32
	Total             int32
	Max               int32
	AcquireCount      int64
	EmptyAcquireCount int64
	CanceledAcquire   int64
}

// Stats returns a snapshot of the connection pool.
func (db *DB) Stats() PoolStats {
	stat := db.pool.Stat()
	return PoolStats{
		Acquired:          stat.AcquiredConns(),
		Idle:              stat.IdleConns(),
		Total:             stat.TotalConns(),
		Max:               stat.MaxConns(),
		AcquireCount:      stat.AcquireCount(),
		EmptyAcquireCount: stat.EmptyAcquireCount(),
		CanceledAcquire:   stat.CanceledAcquireCount(),
	}
}
