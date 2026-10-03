package dispatcher

// #271 P0 hardening: the skew watcher must warn for a large host/DB offset and
// stay quiet inside the tolerance. Removing the threshold comparison (or the
// probe call) turns the "warns" subtests red.

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

func captureLogger() (*log.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.New(&buf, "", 0), &buf
}

func fixedDB(ts time.Time) func(context.Context) (time.Time, error) {
	return func(context.Context) (time.Time, error) { return ts, nil }
}

func TestCheckClockSkewWarnsOutsideTolerance(t *testing.T) {
	base := time.Date(2026, 9, 14, 22, 37, 25, 0, time.UTC)

	cases := []struct {
		name       string
		hostOffset time.Duration
		wantWarn   bool
	}{
		{"host_ahead_431s", 431 * time.Second, true},
		{"host_behind_431s", -431 * time.Second, true},
		{"through_step_up_boundary", 30*time.Second + time.Millisecond, true},
		{"at_tolerance_no_warn", 30 * time.Second, false},
		{"under_tolerance", 29 * time.Second, false},
		{"zero", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger()
			checkClockSkew(
				context.Background(),
				logger,
				func() time.Time { return base.Add(tc.hostOffset) },
				fixedDB(base),
			)
			warned := strings.Contains(buf.String(), "CLOCK SKEW WARNING")
			if warned != tc.wantWarn {
				t.Fatalf("warn=%v, want %v (log=%q)", warned, tc.wantWarn, buf.String())
			}
		})
	}
}

func TestCheckClockSkewDBErrorIsNonFatal(t *testing.T) {
	logger, buf := captureLogger()
	// A DB error must be logged and must NOT panic or emit a skew warning.
	checkClockSkew(
		context.Background(),
		logger,
		func() time.Time { return time.Now() },
		func(context.Context) (time.Time, error) { return time.Time{}, errors.New("db down") },
	)
	if !strings.Contains(buf.String(), "clock skew probe") {
		t.Fatalf("DB error must be logged, got %q", buf.String())
	}
	if strings.Contains(buf.String(), "CLOCK SKEW WARNING") {
		t.Fatal("a DB read failure must not be mistaken for clock skew")
	}
}

func TestHostDBOffsetSign(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := hostDBOffset(base.Add(5*time.Second), base); got != 5*time.Second {
		t.Fatalf("host ahead offset = %s, want +5s", got)
	}
	if got := hostDBOffset(base.Add(-5*time.Second), base); got != -5*time.Second {
		t.Fatalf("host behind offset = %s, want -5s", got)
	}
}
