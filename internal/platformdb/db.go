package platformdb

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	applyPoolLimits(config)

	pool, err := pgxpool.NewWithConfig(ctx, config)
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

// schemaHistoryDDL records what has been applied.
//
// It is created here rather than in a migration file because it has to exist
// before the first migration can be recorded. Recording is additive: the
// migrations themselves still run on every startup, exactly as before, because
// each one is written to be idempotent.
// checksum is the hash of the file as it was when this database first applied
// it, and is never written again. source_checksum is the hash of the file the
// running binary carries, refreshed on every startup. Keeping both is the whole
// mechanism: one value that cannot move is what a changing one can be compared
// against.
const schemaHistoryDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    name TEXT PRIMARY KEY,
    checksum TEXT NOT NULL,
    source_checksum TEXT,
    first_applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// Additive, so a database written by an older build gains the column without
// losing the checksum it already recorded. Backfilling it to the existing
// checksum is the honest default: what that database applied is all anyone can
// know about it, and claiming drift nobody observed would be worse than
// claiming none.
const schemaHistoryUpgradeDDL = `
ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS source_checksum TEXT;
UPDATE schema_migrations SET source_checksum = checksum WHERE source_checksum IS NULL`

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

	if _, err := db.pool.Exec(ctx, schemaHistoryDDL); err != nil {
		return fmt.Errorf("create schema history: %w", err)
	}
	if _, err := db.pool.Exec(ctx, schemaHistoryUpgradeDDL); err != nil {
		return fmt.Errorf("upgrade schema history: %w", err)
	}

	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := db.pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		// The checksum is what makes an edited migration visible, and only if
		// the first one is left alone. A file that changed after it was
		// applied leaves a database that no longer matches the source
		// supposed to describe it; overwriting the recorded hash with the new
		// file's would destroy the only evidence of that, on the very startup
		// that should have reported it.
		//
		// So checksum is written once and never updated. source_checksum
		// carries what this binary is running, and the two disagreeing is
		// drift.
		digest := sha256.Sum256(body)
		sum := hex.EncodeToString(digest[:])
		if _, err := db.pool.Exec(ctx, `
INSERT INTO schema_migrations (name, checksum, source_checksum) VALUES ($1,$2,$2)
ON CONFLICT (name) DO UPDATE SET source_checksum=EXCLUDED.source_checksum, last_applied_at=now()`,
			name, sum); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	return nil
}

// SchemaState describes how far the schema has been taken.
type SchemaState struct {
	// Level is the highest applied migration filename, which is also the
	// schema's version because migrations are applied in filename order.
	Level string `json:"level"`
	// Applied is how many migrations the database has recorded.
	Applied int `json:"applied"`
	// Drifted names the migrations whose file no longer hashes to what this
	// database applied. Level and Applied cannot show this: an edited
	// migration keeps its filename, so two deployments report an identical
	// schema version while holding different schemas.
	Drifted []string `json:"drifted,omitempty"`
}

// SchemaLevel reports the applied schema, for the release manifest and for
// build metadata. It reads the ledger rather than the embedded files, so it
// describes the database in front of it rather than the binary asking.
func (db *DB) SchemaLevel(ctx context.Context) (SchemaState, error) {
	var state SchemaState
	err := db.pool.QueryRow(ctx, `SELECT COALESCE(MAX(name),''), COUNT(*) FROM schema_migrations`).
		Scan(&state.Level, &state.Applied)
	if err != nil {
		return SchemaState{}, err
	}
	rows, err := db.pool.Query(ctx, `
SELECT name FROM schema_migrations
WHERE source_checksum IS NOT NULL AND source_checksum <> checksum
ORDER BY name`)
	if err != nil {
		return SchemaState{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return SchemaState{}, err
		}
		state.Drifted = append(state.Drifted, name)
	}
	if err := rows.Err(); err != nil {
		return SchemaState{}, err
	}
	return state, nil
}

// EmbeddedSchemaLevel reports the highest migration this binary carries.
//
// Comparing it with SchemaLevel answers the question that matters during a
// deployment: is the database behind the code that is talking to it?
func EmbeddedSchemaLevel() (SchemaState, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return SchemaState{}, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return SchemaState{}, nil
	}
	sort.Strings(names)
	return SchemaState{Level: names[len(names)-1], Applied: len(names)}, nil
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

// Pool defaults.
//
// pgx sizes a pool at max(4, NumCPU), which is a reasonable guess about the
// client and no guess at all about the server. PostgreSQL's own max_connections
// is the real limit, and it is shared with every other client — so a platform
// that sizes its pool by its own core count will, on a larger machine, quietly
// take a share it was never allocated and fail everyone at once when it runs
// out.
//
// These are bounds rather than targets: the pool opens what it needs.
const (
	defaultMaxConns = 20
	defaultMinConns = 2
	// A connection that has been idle this long is closed, so a quiet night
	// does not hold connections a neighbouring service needs.
	defaultMaxConnIdleTime = 5 * time.Minute
	// Connections are recycled regardless of use. This is what stops a
	// long-lived pool from pinning a connection to a database instance that
	// has since been replaced behind a load balancer.
	defaultMaxConnLifetime = 30 * time.Minute
	// A caller waiting for a connection is already in trouble. Bounding the
	// wait turns pool exhaustion into a fast error that says so, rather than
	// a queue that looks like the database is slow.
	defaultConnectTimeout = 5 * time.Second
)

// applyPoolLimits sets the pool bounds, letting the connection string override
// any of them: a deployment knows its own PostgreSQL better than this does.
func applyPoolLimits(config *pgxpool.Config) {
	if config.MaxConns <= 0 || config.MaxConns == int32(max(4, runtime.NumCPU())) {
		config.MaxConns = intFromEnv("DATABASE_MAX_CONNS", defaultMaxConns)
	}
	if config.MinConns <= 0 {
		config.MinConns = intFromEnv("DATABASE_MIN_CONNS", defaultMinConns)
	}
	if config.MaxConnIdleTime <= 0 {
		config.MaxConnIdleTime = defaultMaxConnIdleTime
	}
	if config.MaxConnLifetime <= 0 {
		config.MaxConnLifetime = defaultMaxConnLifetime
	}
	if config.ConnConfig.ConnectTimeout <= 0 {
		config.ConnConfig.ConnectTimeout = defaultConnectTimeout
	}
}

// maxPoolConns is the largest pool this will configure.
//
// PostgreSQL's own default max_connections is 100, shared across every client.
// A pool above this is a misconfiguration rather than a tuning choice, and
// honouring it would let one service exhaust the server for everyone.
const maxPoolConns = 500

// intFromEnv reads a bounded pool size.
//
// The bound is not decoration. strconv.Atoi returns a platform-width int, and
// converting that to the int32 pgx wants will silently wrap: a value of three
// billion becomes a negative connection count, which is not a large pool or a
// rejected one but an undefined one. An out-of-range value is refused in
// favour of the default, and says so, because a deployment that asked for
// something impossible should find out rather than run with a number nobody
// chose.
func intFromEnv(name string, fallback int32) int32 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > maxPoolConns {
		return fallback
	}
	return int32(value)
}

// GlobalIDRequest is one record to map in a batch.
type GlobalIDRequest struct {
	EntityType string
	Source     string
	SourceID   string
	TenantID   string
	Metadata   map[string]any
}

// MapGlobalIDs resolves a page of upstream records to Global IDs in bounded
// work, returning `source_id` to Global ID for the entity type requested.
//
// The listing handlers used to call CreateGlobalEntity once per item, which
// is a transaction — and for contacts and issues, two transactions — for
// every row on the page. A page of fifty contacts cost a hundred round trips
// to render fifty lines, and the cost grew with the page rather than with the
// work.
//
// This does one SELECT for everything already mapped, which is the normal
// case after the first sighting of a collection, and then allocates only what
// is genuinely new. The allocation still goes through CreateGlobalEntity, so
// there is exactly one place where an identifier comes into existence.
func (db *DB) MapGlobalIDs(ctx context.Context, entityType, source string, requests []GlobalIDRequest) (map[string]string, error) {
	mapped := make(map[string]string, len(requests))
	if len(requests) == 0 {
		return mapped, nil
	}

	sourceIDs := make([]string, 0, len(requests))
	seen := make(map[string]bool, len(requests))
	for _, request := range requests {
		if request.SourceID == "" || seen[request.SourceID] {
			continue
		}
		seen[request.SourceID] = true
		sourceIDs = append(sourceIDs, request.SourceID)
	}

	rows, err := db.pool.Query(ctx, `
SELECT source_id, global_id FROM global_entities
WHERE source = $1 AND entity_type = $2 AND source_id = ANY($3)`,
		source, entityType, sourceIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sourceID, globalID string
		if err := rows.Scan(&sourceID, &globalID); err != nil {
			rows.Close()
			return nil, err
		}
		mapped[sourceID] = globalID
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Anything still unmapped is being seen for the first time. This is the
	// slow path by construction: it runs once per record in the platform's
	// lifetime, not once per request.
	for _, request := range requests {
		if request.SourceID == "" {
			continue
		}
		if _, already := mapped[request.SourceID]; already {
			continue
		}
		entity, err := db.CreateGlobalEntity(ctx, entityType, source, request.SourceID, request.TenantID, request.Metadata)
		if err != nil {
			return nil, err
		}
		mapped[request.SourceID] = entity.GlobalID
	}
	return mapped, nil
}
