// Clock-domain probes (#271 P0 hardening): the dispatcher's lease writes and
// fences use the DB clock (`now()`/`clock_timestamp()`), while the host mints
// the source-url exp and reads its own `time.Now()` for phase logs. The two
// clocks are not guaranteed to agree — a host maintenance sleep left the
// Podman VM 431 s behind and silently 404'd a whole ingest wave (#270). This
// probe makes the offset observable before it becomes an outage.
package repo

import (
	"context"
	"time"
)

// DBTime returns the database's current wall clock (`SELECT now()`). Used by
// the dispatcher skew watcher; never for lease fencing (the SQL predicates
// own that).
func (r *Repo) DBTime(ctx context.Context) (time.Time, error) {
	var t time.Time
	if err := r.pool.QueryRow(ctx, `SELECT now()`).Scan(&t); err != nil {
		return time.Time{}, err
	}
	return t, nil
}
