package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	format := flag.String("format", "text", "output format: text or json")
	failOnWarning := flag.Bool("fail-on-warning", false, "exit non-zero when only warnings are found")
	timeout := flag.Duration("timeout", 30*time.Second, "overall timeout")
	flag.Parse()

	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	input, err := read(ctx, dsn)
	if err != nil {
		// The DSN carries a password, so the driver's error is not safe to
		// print verbatim.
		fmt.Fprintln(os.Stderr, "mapping audit failed to read the database:", redactDSN(err.Error(), dsn))
		os.Exit(2)
	}

	findings := Audit(input)
	summary := Summarize(findings)

	switch *format {
	case "json":
		out := struct {
			GeneratedAt string    `json:"generated_at"`
			Summary     Summary   `json:"summary"`
			Findings    []Finding `json:"findings"`
		}{time.Now().UTC().Format(time.RFC3339), summary, findings}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(out); err != nil {
			fmt.Fprintln(os.Stderr, "encode:", err)
			os.Exit(2)
		}
	default:
		fmt.Print(Text(findings, summary))
	}

	if summary.Errors > 0 || (*failOnWarning && summary.Warnings > 0) {
		os.Exit(1)
	}
}

// redactDSN keeps a connection string out of an error message. The DSN is the
// one piece of input this tool holds that is a credential.
func redactDSN(message, dsn string) string {
	if dsn == "" {
		return message
	}
	return strings.ReplaceAll(message, dsn, "[DATABASE_URL]")
}

// read collects everything the audit needs. Every statement is a SELECT: this
// tool is read-only by construction, not by convention.
func read(ctx context.Context, dsn string) (Input, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return Input{}, err
	}
	defer pool.Close()

	var in Input

	rows, err := pool.Query(ctx, `SELECT global_id, entity_type, source, source_id, COALESCE(tenant_id,'') FROM global_entities`)
	if err != nil {
		return Input{}, err
	}
	for rows.Next() {
		var m Mapping
		if err := rows.Scan(&m.GlobalID, &m.EntityType, &m.Source, &m.SourceID, &m.TenantID); err != nil {
			rows.Close()
			return Input{}, err
		}
		in.Mappings = append(in.Mappings, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Input{}, err
	}

	in.Prefixes = map[string]string{}
	prefixRows, err := pool.Query(ctx, `SELECT entity_type, prefix FROM global_id_counters`)
	if err != nil {
		return Input{}, err
	}
	for prefixRows.Next() {
		var entityType, prefix string
		if err := prefixRows.Scan(&entityType, &prefix); err != nil {
			prefixRows.Close()
			return Input{}, err
		}
		in.Prefixes[entityType] = prefix
	}
	prefixRows.Close()
	if err := prefixRows.Err(); err != nil {
		return Input{}, err
	}

	// Relations are collected per holding table rather than through a generic
	// scan, so that a new table with a Global ID column is a deliberate
	// addition here instead of being silently unaudited.
	relationQueries := []struct {
		query string
		field string
	}{
		{`SELECT global_id, COALESCE(client_id,'') FROM servers`, "servers.client_id"},
		{`SELECT global_id, COALESCE(project_id,'') FROM servers`, "servers.project_id"},
		{`SELECT global_id, COALESCE(client_id,'') FROM support_records`, "support_records.client_id"},
	}
	for _, rq := range relationQueries {
		relRows, err := pool.Query(ctx, rq.query)
		if err != nil {
			return Input{}, err
		}
		for relRows.Next() {
			var holder, target string
			if err := relRows.Scan(&holder, &target); err != nil {
				relRows.Close()
				return Input{}, err
			}
			in.Relations = append(in.Relations, Relation{Holder: holder, Field: rq.field, Target: target})
		}
		relRows.Close()
		if err := relRows.Err(); err != nil {
			return Input{}, err
		}
	}

	ownerlessQueries := []struct {
		query string
		field string
	}{
		{`SELECT global_id FROM servers WHERE client_id IS NULL`, "servers.client_id"},
		{`SELECT global_id FROM support_records WHERE client_id IS NULL`, "support_records.client_id"},
	}
	for _, oq := range ownerlessQueries {
		ownerRows, err := pool.Query(ctx, oq.query)
		if err != nil {
			return Input{}, err
		}
		for ownerRows.Next() {
			var holder string
			if err := ownerRows.Scan(&holder); err != nil {
				ownerRows.Close()
				return Input{}, err
			}
			in.OwnerlessHolders = append(in.OwnerlessHolders, Relation{Holder: holder, Field: oq.field})
		}
		ownerRows.Close()
		if err := ownerRows.Err(); err != nil {
			return Input{}, err
		}
	}

	return in, nil
}
