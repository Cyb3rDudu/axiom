package dispatcher

// Host-vs-DB clock skew watcher (#271 P0 hardening).
//
// Lease writes and fences run on the DB clock; the host only mints source-url
// exp values and reads its own time for logs. When the host and the DB VM
// drift (maintenance sleep, NTP step, snapshot restore), the two clocks
// disagree and freshness decisions can silently flip. This watcher samples
// `SELECT now()` at dispatcher start and periodically, and warns once the
// absolute offset crosses the threshold — a 7-minute chronyd step must not
// pass unnoticed again (#270).

import (
	"context"
	"log"
	"time"
)

const (
	// clockSkewWarnThreshold is the offset beyond which a warning is emitted.
	// 30 s is well below the 5-minute default lease and the 300 s pre-#264
	// exp slack, so an outage-causing step is flagged long before it bites.
	clockSkewWarnThreshold = 30 * time.Second
	// clockSkewCheckInterval is the periodic sampling cadence.
	clockSkewCheckInterval = time.Minute
)

// hostDBOffset is host wall clock minus DB wall clock (positive => host ahead).
func hostDBOffset(host, db time.Time) time.Duration { return host.Sub(db) }

// checkClockSkew samples the DB clock once and warns when the host/DB offset
// exceeds ±clockSkewWarnThreshold. Pure enough to unit-test with injected
// clocks; a DB error is logged but never fatal (the probe is observer-only).
func checkClockSkew(
	ctx context.Context,
	logger *log.Logger,
	hostNow func() time.Time,
	dbNow func(context.Context) (time.Time, error),
) {
	dbTime, err := dbNow(ctx)
	if err != nil {
		if logger != nil {
			logger.Printf("clock skew probe: DB now() failed: %v", err)
		}
		return
	}
	offset := hostDBOffset(hostNow(), dbTime)
	if offset > clockSkewWarnThreshold || offset < -clockSkewWarnThreshold {
		if logger == nil {
			return
		}
		ahead := "host ahead of DB"
		if offset < 0 {
			ahead = "host behind DB"
		}
		logger.Printf(
			"CLOCK SKEW WARNING: host-DB offset %s exceeds ±%s (%s); "+
				"freshness must stay DB-side (#271) — investigate host sleep/NTP/VM clock",
			offset, clockSkewWarnThreshold, ahead,
		)
	}
}

// skewWatch runs one immediate probe, then samples on the interval until ctx
// is done. Observer-only: it never marks jobs or blocks the claim loop.
func (d *Dispatcher) skewWatch(ctx context.Context) {
	checkClockSkew(ctx, d.logger, time.Now, d.rep.DBTime)
	ticker := time.NewTicker(clockSkewCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkClockSkew(ctx, d.logger, time.Now, d.rep.DBTime)
		}
	}
}
