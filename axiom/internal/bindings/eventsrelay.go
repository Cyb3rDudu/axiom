// eventsrelay.go — the api-process half of the WS forwarding decision
// (F11 #305): BridgeEvents consumes the store process's internal SSE
// stream and republishes every source event onto the LOCAL broker, so
// /api/ws and /api/runners/live work identically in every topology.
// Derived frames (RunnerStateChanged) are skipped — both processes run
// the deterministic deriver over the source events, so relaying them
// would double-publish. Reconnects with backoff; a bridge that cannot
// connect keeps retrying (the stream is best-effort observability, and
// the store process may come up after the api process).
package bindings

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/events"
)

// BridgeEvents runs the relay until ctx is done: stream events from
// storeURL's /events route onto broker.
func BridgeEvents(ctx context.Context, storeURL string, broker *events.Broker, hc *http.Client, logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}
	if hc == nil {
		hc = &http.Client{}
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := streamOnce(ctx, storeURL, broker, hc)
		if ctx.Err() != nil {
			return // shutdown is not a reconnect case
		}
		if err != nil {
			logger.Printf("bindings: event bridge: %v (retrying in %s)", err, backoff)
		} else if n == 0 {
			// A clean end without events is a server-side close — the
			// same reconnect path, without the alarm.
			logger.Printf("bindings: event bridge: stream ended (reconnecting in %s)", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// streamOnce consumes one SSE connection to (hopefully) exhaustion;
// n counts republished events.
func streamOnce(ctx context.Context, storeURL string, broker *events.Broker, hc *http.Client) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(storeURL, "/")+"/internal/v1/store/events", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, errUnexpectedStatus(resp.StatusCode)
	}
	n := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] == ':' {
			continue // framing keepalive
		}
		if !strings.HasPrefix(string(line), "data: ") {
			continue
		}
		ev, err := unmarshalEventFrame(line[len("data: "):])
		if err != nil || ev == nil {
			continue // unknown frame types skip, not kill
		}
		if _, derived := ev.(events.RunnerStateChanged); derived {
			continue // derived locally on both sides (package doc)
		}
		broker.Publish(ev)
		n++
	}
	return n, sc.Err()
}

func errUnexpectedStatus(code int) error {
	return fmt.Errorf("event stream answered HTTP %d", code)
}
