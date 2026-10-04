// http_check.go — DM07 #316: the credential-free split edge keeps its
// dependency visibility by PROXYING it over the component edges: the
// api-only process registers one HTTP health probe per configured
// component (library/store), each aggregating that component's own
// dependency checks (its /api/health folds postgres, zotero, the
// runners…). The edge opens no pool and holds no DSN — it answers the
// operator's "is the topology alive" question transitively.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPHealthChecker probes one component's /api/health over HTTP.
type HTTPHealthChecker struct {
	url       string
	client    *http.Client
	component string
}

// CheckHTTP builds a component health probe. The budget is deliberately
// tight and independent of the internal-call budget: a health endpoint
// must answer an order of magnitude faster than a content call, and a
// wedged component must mark the edge red instead of stalling it.
func CheckHTTP(component, healthURL string, timeout time.Duration) *HTTPHealthChecker {
	return &HTTPHealthChecker{
		url:       healthURL,
		component: component,
		client:    &http.Client{Timeout: timeout},
	}
}

// Ready reports the component healthy when its health endpoint answers
// ok — the component's OWN dependency checks fold into that verdict.
func (c *HTTPHealthChecker) Ready() error {
	if c == nil || c.url == "" {
		return fmt.Errorf("%s edge not configured", c.component)
	}
	resp, err := c.client.Get(c.url)
	if err != nil {
		return fmt.Errorf("%s component unreachable: %w", c.component, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s component health: status %d", c.component, resp.StatusCode)
	}
	var h struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return fmt.Errorf("%s component health: %w", c.component, err)
	}
	if !h.OK {
		return fmt.Errorf("%s component reports not-ok", c.component)
	}
	return nil
}
