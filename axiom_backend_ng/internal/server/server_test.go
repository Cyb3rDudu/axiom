package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
)

func TestRouterRoutesHealthAndRoot(t *testing.T) {
	t.Parallel()
	s := New(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		path       string
		wantStatus int
	}{
		{"/", http.StatusOK},
		{"/health", http.StatusOK},
		{"/does-not-exist", http.StatusNotFound},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("%s: got %d, want %d", tc.path, rec.Code, tc.wantStatus)
			}
		})
	}
}
