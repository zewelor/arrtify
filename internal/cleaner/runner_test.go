package cleaner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunAbortsBeforeDecisionsOnArrFailure(t *testing.T) {
	calls := []string{}
	gets := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req struct {
				Method string `json:"method"`
				ID     int    `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			calls = append(calls, req.Method)
			var result any
			switch req.Method {
			case "session_get":
				result = map[string]any{"version": "4.1.3", "rpc_version_semver": "6.0.1"}
			case "torrent_get":
				result = map[string]any{"torrents": []any{map[string]any{"hash_string": strings.Repeat("a", 40), "name": "Synthetic", "download_dir": "/downloads/sonarr", "upload_ratio": 2, "seconds_seeding": 24 * 3600, "status": 6, "is_finished": false, "left_until_done": 0, "metadata_percent_complete": 1, "trackers": []any{}, "labels": []string{}}}}
			default:
				t.Errorf("unexpected RPC %s", req.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			return
		}
		gets = append(gets, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/sonarr/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"page": 1, "pageSize": 100, "sortKey": "timeleft", "sortDirection": "descending", "totalRecords": 0, "records": []any{}})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := testConfig(t)
	c.Transmission.URL = server.URL + "/transmission/rpc"
	c.Sonarr.URL = server.URL + "/sonarr"
	c.Radarr.URL = server.URL + "/radarr"
	report := Run(context.Background(), c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if report.Status != "failed" || report.Scanned != 1 || report.NotAttempted != 1 || report.Errors != 1 || report.Planned != 0 || report.Attempted != 0 || len(report.Decisions) != 1 || report.Decisions[0].Reason != "source_read_failed" {
		t.Fatalf("unexpected report %+v", report)
	}
	if len(gets) != 2 || !strings.HasPrefix(gets[1], "/radarr/") {
		t.Fatalf("did not reach Radarr: %v", gets)
	}
	for _, m := range calls {
		if m != "session_get" && m != "torrent_get" {
			t.Fatalf("mutation %s", m)
		}
	}
}
