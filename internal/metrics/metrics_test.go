package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helper: scrape the registry and return the body as a string.
func scrape(t *testing.T, m *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: status = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func TestNew_RegistersStandardCollectors(t *testing.T) {
	m := New()
	body := scrape(t, m)
	for _, want := range []string{
		"go_goroutines",          // Go runtime collector
		"process_cpu_seconds_total", // process collector
		"docstore_commits_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestMiddleware_RecordsRequestAndDuration(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := m.Middleware(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}

	body := scrape(t, m)
	// Cardinality discipline: pattern, not raw path.
	wantLine := `docstore_http_requests_total{code="418",method="GET",pattern="GET /api/{id}"}`
	if !strings.Contains(body, wantLine) {
		t.Errorf("scrape missing %q\nbody:\n%s", wantLine, body)
	}
	if !strings.Contains(body, "docstore_http_request_duration_seconds_bucket") {
		t.Error("duration histogram not present in scrape")
	}
}

func TestMiddleware_UnmatchedPathBucketedAsOther(t *testing.T) {
	m := New()
	mux := http.NewServeMux() // no patterns registered → everything is a 404
	h := m.Middleware(mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/no-such-path", nil))

	body := scrape(t, m)
	wantLine := `docstore_http_requests_total{code="404",method="GET",pattern="other"}`
	if !strings.Contains(body, wantLine) {
		t.Errorf("scrape missing %q\nbody:\n%s", wantLine, body)
	}
}

func TestMiddleware_DefaultStatusIs200(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) {
		// Note: never call WriteHeader; first Write should imply 200.
		_, _ = w.Write([]byte("hi"))
	})
	h := m.Middleware(mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))

	body := scrape(t, m)
	if !strings.Contains(body, `code="200"`) {
		t.Errorf("expected 200 label in metrics; body:\n%s", body)
	}
}

func TestCommitsTotal_Increments(t *testing.T) {
	m := New()
	m.CommitsTotal().Inc()
	m.CommitsTotal().Inc()

	body := scrape(t, m)
	if !strings.Contains(body, "docstore_commits_total 2") {
		t.Errorf("expected commits=2 in scrape; body:\n%s", body)
	}
}

func TestSecretOps_LabeledCounter(t *testing.T) {
	m := New()
	m.SecretOps().WithLabelValues("created").Inc()
	m.SecretOps().WithLabelValues("accessed").Inc()
	m.SecretOps().WithLabelValues("accessed").Inc()

	body := scrape(t, m)
	for _, want := range []string{
		`docstore_secrets_operations_total{operation="created"} 1`,
		`docstore_secrets_operations_total{operation="accessed"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\nbody:\n%s", want, body)
		}
	}
}
