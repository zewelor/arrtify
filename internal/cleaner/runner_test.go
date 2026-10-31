package cleaner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
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

func TestRunApplyRevalidatesAndConfirmsRemoval(t *testing.T) {
	for _, tc := range []struct {
		name, change, outcome string
		attempted             int
	}{
		{name: "successful single-hash removal", outcome: "removed", attempted: 1},
		{name: "hash enters Sonarr queue", change: "sonarr_queue", outcome: "skipped"},
		{name: "hash enters Radarr queue", change: "radarr_queue", outcome: "skipped"},
		{name: "tracker becomes ignored", change: "tracker", outcome: "skipped"},
		{name: "category changes", change: "category", outcome: "skipped"},
		{name: "category changes to another eligible rule", change: "eligible_category", outcome: "skipped"},
		{name: "torrent starts downloading", change: "state", outcome: "skipped"},
		{name: "ratio falls below threshold", change: "ratio", outcome: "skipped"},
		{name: "torrent disappears", change: "missing", outcome: "skipped"},
		{name: "fresh torrent is incomplete", change: "invalid", outcome: "failed"},
		{name: "fresh Sonarr queue fails", change: "sonarr_error", outcome: "failed"},
		{name: "fresh Radarr queue fails", change: "radarr_error", outcome: "failed"},
		{name: "remove is explicitly rejected", change: "rejected", outcome: "failed", attempted: 1},
		{name: "remove response is lost", change: "remove_http", outcome: "unknown", attempted: 1},
		{name: "remove response is malformed", change: "remove_json", outcome: "unknown", attempted: 1},
		{name: "internal RPC error is uncertain", change: "internal_error", outcome: "unknown", attempted: 1},
		{name: "conflicting RPC envelope is uncertain", change: "conflicting_error", outcome: "unknown", attempted: 1},
		{name: "malformed RPC error is uncertain", change: "malformed_error", outcome: "unknown", attempted: 1},
		{name: "confirmation fails", change: "confirm_http", outcome: "unknown", attempted: 1},
		{name: "confirmation still sees torrent", change: "confirm_present", outcome: "unknown", attempted: 1},
		{name: "cancel before remove", change: "cancel_before", outcome: "failed"},
		{name: "cancel during remove", change: "cancel_during", outcome: "unknown", attempted: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hash := strings.Repeat("a", 40)
			var mu sync.Mutex
			reads, removes := 0, 0
			queues := map[string]int{"sonarr": 0, "radarr": 0}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodGet {
					service := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
					queues[service]++
					fresh := queues[service] > 1
					if fresh && tc.change == service+"_error" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if fresh && service == "radarr" && tc.change == "cancel_before" {
						cancel()
					}
					records := []any{}
					if fresh && tc.change == service+"_queue" {
						records = append(records, map[string]any{"id": 1, "downloadId": strings.ToUpper(hash)})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"page": 1, "pageSize": 100, "sortKey": "timeleft", "sortDirection": "ascending",
						"totalRecords": len(records), "records": records,
					})
					return
				}
				var request struct {
					Method string                     `json:"method"`
					ID     int                        `json:"id"`
					Params map[string]json.RawMessage `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				result := map[string]any{}
				switch request.Method {
				case "session_get":
					result = map[string]any{"version": "4.1.3", "rpc_version_semver": "6.0.1"}
				case "torrent_get":
					reads++
					if reads > 1 {
						var ids []string
						if json.Unmarshal(request.Params["ids"], &ids) != nil || !reflect.DeepEqual(ids, []string{hash}) {
							t.Errorf("fresh read target = %v", ids)
						}
					}
					if reads > 2 && tc.change == "confirm_http" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					raw := map[string]any{
						"hash_string": hash, "name": "Synthetic", "download_dir": "/downloads/sonarr",
						"upload_ratio": 2, "seconds_seeding": 24 * 3600, "status": 6,
						"is_finished": false, "left_until_done": 0, "metadata_percent_complete": 1,
						"trackers": []any{}, "labels": []string{},
					}
					if reads > 1 {
						switch tc.change {
						case "tracker":
							raw["trackers"] = []any{map[string]any{"announce": "https://tracker.example.invalid/announce"}}
						case "category":
							raw["download_dir"] = "/downloads/other"
						case "eligible_category":
							raw["download_dir"] = "/downloads/radarr"
						case "state":
							raw["status"], raw["left_until_done"] = 4, 1
						case "ratio":
							raw["upload_ratio"] = 0
						case "invalid":
							delete(raw, "trackers")
						}
					}
					items := []any{raw}
					missing := reads > 1 && tc.change == "missing"
					confirmed := removes > 0 && tc.change != "confirm_present"
					if missing || confirmed {
						items = []any{}
					}
					result["torrents"] = items
				case "torrent_remove":
					removes++
					if reads != 2 || queues["sonarr"] != 2 || queues["radarr"] != 2 {
						t.Errorf("remove without fresh torrent and both queues: reads=%d queues=%v", reads, queues)
					}
					var ids []string
					var deleteData bool
					validIDs := json.Unmarshal(request.Params["ids"], &ids) == nil && reflect.DeepEqual(ids, []string{hash})
					validDelete := json.Unmarshal(request.Params["delete_local_data"], &deleteData) == nil && deleteData
					if len(request.Params) != 2 || !validIDs || !validDelete {
						t.Errorf("unsafe remove params: %v", request.Params)
					}
					switch tc.change {
					case "rejected", "internal_error", "conflicting_error", "malformed_error":
						code := -32602
						if tc.change == "internal_error" {
							code = -32603
						}
						envelope := map[string]any{
							"jsonrpc": "2.0", "id": request.ID,
							"error": map[string]any{"code": code, "message": "secret-canary"},
						}
						if tc.change == "conflicting_error" {
							envelope["result"] = map[string]any{}
						}
						if tc.change == "malformed_error" {
							envelope["error"] = map[string]any{"code": code}
						}
						_ = json.NewEncoder(w).Encode(envelope)
						return
					case "remove_http":
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					case "remove_json":
						_, _ = io.WriteString(w, "invalid secret-canary")
						return
					case "cancel_during":
						cancel()
						<-r.Context().Done()
						return
					}
				default:
					t.Errorf("unexpected RPC %s", request.Method)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
			}))
			defer server.Close()
			c := testConfig(t)
			apply := false
			c.dryRun = &apply
			c.Transmission.URL = server.URL + "/transmission/rpc"
			c.Sonarr.URL = server.URL + "/sonarr"
			c.Radarr.URL = server.URL + "/radarr"
			var logs strings.Builder
			report := Run(ctx, c, slog.New(slog.NewJSONHandler(&logs, nil)))
			mu.Lock()
			defer mu.Unlock()
			status, failures := "complete", 0
			if tc.outcome == "failed" {
				status, failures = "failed", 1
			}
			if tc.outcome == "unknown" {
				status, failures = "partial", 1
			}
			if len(report.Decisions) != 1 || report.Decisions[0].Outcome != tc.outcome {
				t.Fatalf("expected %s, got %+v", tc.outcome, report)
			}
			if report.Status != status || report.Errors != failures || report.Planned != 1 || report.Attempted != tc.attempted {
				t.Fatalf("unexpected counters: %+v", report)
			}
			if removes != tc.attempted || report.WouldDelete != 0 {
				t.Fatalf("removes=%d, report=%+v", removes, report)
			}
			outcomes := map[string]int{
				"removed": report.Removed, "skipped": report.Skipped, "failed": report.Failed, "unknown": report.Unknown,
			}
			if outcomes[tc.outcome] != 1 || report.Scanned != 1 || report.NotAttempted != 0 {
				t.Fatalf("outcomes do not account for the torrent: %+v", report)
			}
			if strings.Contains(logs.String(), "secret-canary") {
				t.Fatal("raw RPC data leaked into logs")
			}
			wantEvents := []string{}
			if tc.attempted > 0 {
				wantEvents = append(wantEvents, "delete_requested")
			}
			switch tc.outcome {
			case "removed":
				wantEvents = append(wantEvents, "torrent_removed")
			case "failed":
				wantEvents = append(wantEvents, "cleanup_failed")
			case "unknown":
				wantEvents = append(wantEvents, "operation_unknown")
			}
			gotEvents := []string{}
			decoder := json.NewDecoder(strings.NewReader(logs.String()))
			for decoder.More() {
				var event map[string]any
				if err := decoder.Decode(&event); err != nil {
					t.Fatal(err)
				}
				if event["event"] == "torrent_decision" {
					break
				}
				if event["torrent_hash"] != hash || event["torrent_name"] != "Synthetic" {
					t.Fatalf("cleanup event does not identify the torrent: %v", event)
				}
				gotEvents = append(gotEvents, event["event"].(string))
			}
			if !reflect.DeepEqual(gotEvents, wantEvents) {
				t.Fatalf("events before torrent_decision: got %v, want %v", gotEvents, wantEvents)
			}
		})
	}
}
