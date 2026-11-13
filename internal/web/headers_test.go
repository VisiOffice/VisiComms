package web_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/testutil"
	"github.com/VisiOffice/VisiComms/internal/web"
)

func TestHSTSOnlyWhenServedOverHTTPS(t *testing.T) {
	pool := testutil.DB(t)
	for _, https := range []bool{true, false} {
		s := &web.Server{DB: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), SecureCookies: https}
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/up", nil))
		got := rec.Header().Get("Strict-Transport-Security")
		if https && got != "max-age=63072000; includeSubDomains" {
			t.Errorf("https: HSTS = %q", got)
		}
		if !https && got != "" {
			t.Errorf("local development sent HSTS %q; it would pin localhost to HTTPS", got)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Error("nosniff missing")
		}
	}
}
