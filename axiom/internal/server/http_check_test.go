// http_check_test.go — DM07 #316 review round 2 witnesses: the checker
// registry is concurrent-safe (the F11 internal edges serve /api/health
// from a listener that runs while later components still register —
// verified with -race), and the HTTP probe's failure modes are pinned
// without needing a live topology.
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHealthEndpointConcurrentRegistration — the race the round-2 review
// demonstrated: reads (handleHealth via HealthEndpoint, the internal-edge
// mount) racing writes (RegisterCheck from later-starting components).
// Run under -race this must stay clean; without the registry mutex it
// was a fatal "concurrent map read and map write".
func TestHealthEndpointConcurrentRegistration(t *testing.T) {
	s := New("127.0.0.1:0", nil)
	srv := httptest.NewServer(s.HealthEndpoint())
	defer srv.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// the writer side: a later component registering checks while the
	// edge already serves health (the real sequence: the internal edge
	// listener starts, then query-runner/ingest-runner register).
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.RegisterCheck(probeName(i, j), fakeChecker{nil})
			}
		}(i)
	}
	// the reader side: the api pod's readiness probe polling the edge.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Errorf("health poll: %v", err)
				return
			}
			resp.Body.Close()
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func probeName(i, j int) string {
	return "probe-" + string(rune('a'+i)) + "-" + string(rune('0'+j%10))
}

// TestCheckHTTPFailureModes — every failure path of the component probe,
// against httptest endpoints: unreachable, non-200, not-ok body, and the
// tight timeout (a wedged component marks the edge red instead of
// stalling it).
func TestCheckHTTPFailureModes(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ok.Close()
	notOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":false}`))
	}))
	defer notOK.Close()
	badStatus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer badStatus.Close()
	wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer wedged.Close()

	if err := (CheckHTTP("library", ok.URL, time.Second)).Ready(); err != nil {
		t.Fatalf("healthy component must probe ok: %v", err)
	}
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"not-ok body", notOK.URL},
		{"non-200", badStatus.URL},
	} {
		if err := (CheckHTTP("library", tc.url, time.Second)).Ready(); err == nil {
			t.Fatalf("%s must fail the probe", tc.name)
		}
	}
	// the tight budget: a wedged endpoint fails WITHIN the budget, it
	// does not stall the health response for the endpoint's own delay.
	start := time.Now()
	if err := (CheckHTTP("library", wedged.URL, 100*time.Millisecond)).Ready(); err == nil {
		t.Fatal("wedged component must fail the probe")
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("probe must respect its tight budget, took %s", elapsed)
	}
	// unconfigured: a typed-nil checker must produce an ERROR, not a
	// nil-dereference panic (the round-2 NIT).
	var nilChecker *HTTPHealthChecker
	if err := nilChecker.Ready(); err == nil {
		t.Fatal("typed-nil probe must error, not panic or pass")
	}
	// unreachable endpoint (closed server): fails with the component name.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := closed.URL
	closed.Close()
	if err := (CheckHTTP("store", url, time.Second)).Ready(); err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("unreachable endpoint must fail naming the component, got %v", err)
	}
}
