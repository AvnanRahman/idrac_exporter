package collector

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrlhansen/idrac_exporter/internal/config"
)

// A BMC that advertises filter support but rejects $filter on the log (iDRAC Lclog)
func TestQueryEventLogFilterFallback(t *testing.T) {
	var filtered, unfiltered atomic.Int32
	now := time.Now().Format(time.RFC3339)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s request", r.Method)
		}
		if r.URL.Query().Has("$filter") {
			filtered.Add(1)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		unfiltered.Add(1)
		w.Write([]byte(`{"Members":[
			{"Id":"1","Message":"warn","Severity":"Warning","Created":"` + now + `"},
			{"Id":"2","Message":"ok","Severity":"OK","Created":"` + now + `"}]}`))
	}))
	defer server.Close()

	config.Config = &config.RootConfig{Timeout: 5, Concurrency: 2, MetricsPrefix: "idrac"}
	auth := &config.AuthConfig{Scheme: "https", Username: "user", Password: "pass", BasicAuth: true}
	client := &Client{redfish: NewRedfish(strings.TrimPrefix(server.URL, "https://"), auth)}
	client.features.Filter = true

	cfg := &config.EventConfig{Enabled: true, SeverityLevel: 1, MaxAgeSeconds: 3600}
	path := "/redfish/v1/Managers/iDRAC.Embedded.1/LogServices/Lclog/Entries"

	for i := range 2 {
		events, ok := client.QueryEventLog(path, cfg)
		if !ok || events == nil {
			t.Fatalf("query %d failed", i)
		}
		var ids []string
		for e := range events.Items() {
			ids = append(ids, e.Id)
		}
		if len(ids) != 1 || ids[0] != "1" {
			t.Errorf("query %d: expected only the warning entry, got %v", i, ids)
		}
	}

	if filtered.Load() != 1 || unfiltered.Load() != 2 {
		t.Errorf("filtered=%d unfiltered=%d, want 1 and 2 (filter only tried once)", filtered.Load(), unfiltered.Load())
	}
}
