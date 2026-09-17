package platformdb

import (
	"context"
	"fmt"
	"time"
)

// WithAdvisoryLock serialises a rare cross-service operation across all
// Integration Core replicas that share this PostgreSQL database.
//
// The callback may perform network I/O. The dedicated pooled connection is
// intentionally held for the duration so the session-scoped lock remains
// owned by one session until the external mutation has finished.
func (db *DB) WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) error {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for advisory lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return fmt.Errorf("take advisory lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key)
	}()

	return fn(ctx)
}
