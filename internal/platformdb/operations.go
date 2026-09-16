package platformdb

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ekucher/bsystem-integration-core/internal/operations"
)

// ErrServerNotFound means no server carries that Global ID.
var ErrServerNotFound = errors.New("server not found")

const serverColumns = `
 global_id, name, environment, status, COALESCE(client_id,''), COALESCE(project_id,''),
 source, source_id, last_event_at, created_at`

func scanServer(row pgx.Row) (operations.Server, error) {
	var server operations.Server
	// last_event_at is scanned as a nullable timestamp rather than coalesced
	// to a sentinel. A server that has never been reported on has no last
	// event, and representing that as a date — any date — makes callers
	// compare against a magic value to find out.
	var lastEvent *time.Time
	err := row.Scan(&server.ID, &server.Name, &server.Environment, &server.Status,
		&server.ClientID, &server.ProjectID, &server.Source, &server.SourceID,
		&lastEvent, &server.CreatedAt)
	if err != nil {
		return operations.Server{}, err
	}
	if lastEvent != nil {
		utc := lastEvent.UTC()
		server.LastEventAt = &utc
	}
	server.CreatedAt = server.CreatedAt.UTC()
	return server, nil
}

// UpsertServer stores a server against an already-allocated Global ID.
//
// Name, environment and the relations are last-write-wins from the reporter,
// which is the authoritative source for all of them. Status is deliberately
// not written here: it is a consequence of reported events, and letting a
// registration set it would let a reporter declare a server healthy without
// saying anything happened.
func (db *DB) UpsertServer(ctx context.Context, server operations.Server) (operations.Server, error) {
	row := db.pool.QueryRow(ctx, `
INSERT INTO servers (global_id,name,environment,client_id,project_id,source,source_id)
VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7)
ON CONFLICT (global_id) DO UPDATE SET
 name=EXCLUDED.name,
 environment=EXCLUDED.environment,
 client_id=EXCLUDED.client_id,
 project_id=EXCLUDED.project_id,
 updated_at=now()
RETURNING`+serverColumns,
		server.ID, server.Name, server.Environment, server.ClientID, server.ProjectID,
		server.Source, server.SourceID)
	return scanServer(row)
}

// GetServer returns one server by Global ID.
func (db *DB) GetServer(ctx context.Context, globalID string) (operations.Server, error) {
	row := db.pool.QueryRow(ctx, `SELECT`+serverColumns+` FROM servers WHERE global_id=$1`, strings.TrimSpace(globalID))
	server, err := scanServer(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return operations.Server{}, ErrServerNotFound
	}
	return server, err
}

// ServerQuery bounds a server listing.
type ServerQuery struct {
	// Status and Environment are optional filters.
	Status      string
	Environment string
	// AfterID walks the collection: Global IDs strictly after it.
	AfterID string
	Limit   int
}

// ListServers returns one page ordered by Global ID, with the total.
//
// Servers are ordered by Global ID rather than by status or recency: the
// order has to be stable while a page is being walked, and every other
// candidate changes whenever a reporter says something.
func (db *DB) ListServers(ctx context.Context, query ServerQuery) ([]operations.Server, int, error) {
	var total int
	err := db.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM servers
WHERE ($1 = '' OR status = $1) AND ($2 = '' OR environment = $2)`,
		query.Status, query.Environment).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	rows, err := db.pool.Query(ctx, `
SELECT`+serverColumns+` FROM servers
WHERE ($1 = '' OR status = $1)
  AND ($2 = '' OR environment = $2)
  AND ($3 = '' OR global_id > $3)
ORDER BY global_id
LIMIT $4`, query.Status, query.Environment, query.AfterID, query.Limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	servers := []operations.Server{}
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, 0, err
		}
		servers = append(servers, server)
	}
	return servers, total, rows.Err()
}

// InsertOperationsEvent stores a report and applies whatever it says about
// the server's status, in one transaction.
//
// The two belong together: a stored event whose status was not applied leaves
// a dashboard disagreeing with its own history, and an applied status with no
// event behind it cannot be explained to whoever asks why.
func (db *DB) InsertOperationsEvent(ctx context.Context, report operations.Report, status string) (operations.Report, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return operations.Report{}, err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
INSERT INTO operations_events (server_id,event,severity,summary,source,correlation_id,occurred_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)
RETURNING id`,
		report.ServerID, report.Event, report.Severity, report.Summary,
		report.Source, report.CorrelationID, report.OccurredAt.UTC()).Scan(&report.ID)
	if err != nil {
		return operations.Report{}, err
	}

	// last_event_at moves for every report, because "nothing has been heard
	// from this server" is itself an operational fact. Status moves only when
	// the event says something about whether the server is up.
	if status == "" {
		_, err = tx.Exec(ctx, `UPDATE servers SET last_event_at=$2, updated_at=now() WHERE global_id=$1`,
			report.ServerID, report.OccurredAt.UTC())
	} else {
		_, err = tx.Exec(ctx, `UPDATE servers SET status=$2, last_event_at=$3, updated_at=now() WHERE global_id=$1`,
			report.ServerID, status, report.OccurredAt.UTC())
	}
	if err != nil {
		return operations.Report{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return operations.Report{}, err
	}
	report.OccurredAt = report.OccurredAt.UTC()
	return report, nil
}

// OperationsEventQuery bounds an event listing.
type OperationsEventQuery struct {
	// ServerID and Event are optional filters.
	ServerID string
	Event    string
	// AfterID walks the collection: rows strictly older than it.
	AfterID int64
	Limit   int
}

// ListOperationsEvents returns one page, newest first, with the total.
func (db *DB) ListOperationsEvents(ctx context.Context, query OperationsEventQuery) ([]operations.Report, int, error) {
	var total int
	err := db.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM operations_events
WHERE ($1 = '' OR server_id = $1) AND ($2 = '' OR event = $2)`,
		query.ServerID, query.Event).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	rows, err := db.pool.Query(ctx, `
SELECT id, server_id, event, severity, summary, source, correlation_id, occurred_at
FROM operations_events
WHERE ($1 = '' OR server_id = $1)
  AND ($2 = '' OR event = $2)
  AND ($3 = 0 OR id < $3)
ORDER BY id DESC
LIMIT $4`, query.ServerID, query.Event, query.AfterID, query.Limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	reports := []operations.Report{}
	for rows.Next() {
		var report operations.Report
		if err := rows.Scan(&report.ID, &report.ServerID, &report.Event, &report.Severity,
			&report.Summary, &report.Source, &report.CorrelationID, &report.OccurredAt); err != nil {
			return nil, 0, err
		}
		report.OccurredAt = report.OccurredAt.UTC()
		reports = append(reports, report)
	}
	return reports, total, rows.Err()
}
