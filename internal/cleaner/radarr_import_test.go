package cleaner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRunRadarrImportReport(t *testing.T) {
	for _, tc := range []struct {
		name, change, state, reason string
		apply, failed               bool
	}{
		{name: "current import", state: "current", reason: "current_file"},
		{name: "moved library path", change: "moved", state: "current", reason: "current_file"},
		{name: "replaced import", change: "replaced", state: "replaced", reason: "different_current_file"},
		{name: "missing history", change: "no_history", state: "unknown", reason: "no_import_history"},
		{name: "missing movie", change: "no_movie", state: "unknown", reason: "movie_missing"},
		{name: "missing movie file", change: "no_file", state: "unknown", reason: "movie_file_missing"},
		{name: "repeated identical import", change: "repeated", state: "current", reason: "current_file"},
		{name: "ambiguous imports", change: "ambiguous", state: "unknown", reason: "ambiguous_import_history"},
		{name: "history page two and uppercase hash", change: "paged", state: "current", reason: "current_file"},
		{name: "unmarked torrent skips import APIs", change: "unmarked"},
		{name: "old default label is not selected", change: "old_label"},
		{name: "custom label ignores case", change: "label_case", state: "current", reason: "current_file"},
		{name: "disabled report skips import APIs", change: "disabled"},
		{name: "apply retains informational result", apply: true, state: "current", reason: "current_file"},
		{name: "history error blocks apply", change: "history_error", apply: true, failed: true},
		{name: "incomplete history blocks apply", change: "short_page", apply: true, failed: true},
		{name: "invalid import identity blocks apply", change: "invalid_import", apply: true, failed: true},
		{name: "invalid movies block apply", change: "invalid_movie", apply: true, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := strings.Repeat("a", 40)
			removed := false
			mutations, importReads := 0, 0
			history := []any{}
			if tc.change == "paged" {
				for id := range 100 {
					history = append(history, map[string]any{
						"id": id + 10, "eventType": "grabbed", "downloadId": nil,
					})
				}
			}
			if tc.change != "no_history" {
				fileID := "20"
				if tc.change == "invalid_import" {
					fileID = "secret-canary"
				}
				history = append(history, map[string]any{
					"id": 1, "eventType": "downloadFolderImported", "downloadId": strings.ToUpper(hash),
					"movieId": 10, "data": map[string]any{"fileId": fileID, "importedPath": "/old/movie.mkv"},
				})
			}
			if tc.change == "ambiguous" || tc.change == "repeated" {
				fileID := "20"
				if tc.change == "ambiguous" {
					fileID = "21"
				}
				history = append(history, map[string]any{
					"id": 2, "eventType": "downloadFolderImported", "downloadId": hash,
					"movieId": 10, "data": map[string]any{"fileId": fileID},
				})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					var request struct {
						Method string `json:"method"`
						ID     int    `json:"id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					var result any
					switch request.Method {
					case "session_get":
						result = map[string]any{"version": "4.1.3", "rpc_version_semver": "6.0.1"}
					case "torrent_get":
						items := []any{}
						if !removed {
							labels := []string{"finished-movies", "user:keep"}
							if tc.change == "old_label" {
								labels = []string{"radarr-imported"}
							}
							if tc.change == "label_case" {
								labels = []string{"FINISHED-MOVIES"}
							}
							if tc.change == "unmarked" {
								labels = []string{"user:keep"}
							}
							items = append(items, map[string]any{
								"hash_string": hash, "name": "Synthetic", "download_dir": "/downloads/radarr",
								"upload_ratio": 2, "seconds_seeding": 24 * 3600, "status": 6,
								"is_finished": false, "left_until_done": 0, "metadata_percent_complete": 1,
								"trackers": []any{}, "labels": labels,
							})
						}
						result = map[string]any{"torrents": items}
					case "torrent_remove":
						mutations++
						removed = true
						result = map[string]any{}
					default:
						mutations++
						t.Errorf("unexpected RPC method %s", request.Method)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
					return
				}
				if r.URL.Path == "/radarr/api/v3/movie" {
					importReads++
					movies := []any{}
					fileID := 20
					if tc.change == "replaced" {
						fileID = 21
					}
					if tc.change == "no_file" {
						fileID = 0
					}
					if tc.change != "no_movie" {
						movie := map[string]any{"id": 10, "movieFileId": fileID, "path": "/old"}
						if tc.change == "moved" {
							movie["path"] = "/new"
						}
						if tc.change == "invalid_movie" {
							delete(movie, "movieFileId")
						}
						movies = append(movies, movie)
					}
					_ = json.NewEncoder(w).Encode(movies)
					return
				}
				records := []any{}
				page, total, sortKey := 1, 0, "timeleft"
				if r.URL.Path == "/radarr/api/v3/history" {
					importReads++
					if tc.change == "history_error" {
						http.Error(w, "secret-canary", http.StatusServiceUnavailable)
						return
					}
					page, _ = strconv.Atoi(r.URL.Query().Get("page"))
					total, sortKey = len(history), "date"
					start := (page - 1) * 100
					end := min(start+100, total)
					records = history[start:end]
					if tc.change == "short_page" {
						total++
					}
				} else if !strings.HasSuffix(r.URL.Path, "/api/v3/queue") {
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"page": page, "pageSize": 100, "sortKey": sortKey, "sortDirection": "descending",
					"totalRecords": total, "records": records,
				})
			}))
			defer server.Close()
			configBytes, err := os.ReadFile("../../config.example.yml")
			if err != nil {
				t.Fatal(err)
			}
			configText := strings.Replace(string(configBytes), "radarr:\n", "radarr:\n  imported_label: finished-movies\n", 1)
			if tc.change == "disabled" {
				configText = string(configBytes)
			}
			config, err := ParseConfig(strings.NewReader(configText), testCredentialsLookup)
			if err != nil {
				t.Fatalf("reporting configuration rejected: %v", err)
			}
			config.Transmission.URL = server.URL + "/transmission/rpc"
			config.Sonarr.URL = server.URL + "/sonarr"
			config.Radarr.URL = server.URL + "/radarr"
			if tc.apply {
				config.dryRun = new(false)
			}
			var logs bytes.Buffer
			report := Run(context.Background(), config, slog.New(slog.NewJSONHandler(&logs, nil)))
			if tc.failed {
				if report.Status != "failed" || report.Errors != 1 || mutations != 0 {
					t.Fatalf("report=%+v mutations=%d", report, mutations)
				}
				if strings.Contains(logs.String(), "secret-canary") {
					t.Fatal("unsafe source details leaked")
				}
				return
			}
			if report.Status != "complete" || len(report.Decisions) != 1 {
				t.Fatalf("unexpected report: %+v; logs=%s", report, logs.String())
			}
			encoded, err := json.Marshal(report.Decisions[0])
			if err != nil {
				t.Fatal(err)
			}
			var decision map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &decision); err != nil {
				t.Fatal(err)
			}
			if tc.change == "unmarked" || tc.change == "disabled" || tc.change == "old_label" {
				if importReads != 0 || decision["radarr_import"] != nil {
					t.Fatalf("unmarked torrent inspected: reads=%d decision=%s", importReads, encoded)
				}
				return
			}
			var assessment struct{ State, Reason string }
			if err := json.Unmarshal(decision["radarr_import"], &assessment); err != nil {
				t.Fatalf("missing import assessment: %v; decision=%s", err, encoded)
			}
			if assessment.State != tc.state || assessment.Reason != tc.reason {
				t.Fatalf("assessment=%+v, want %s/%s", assessment, tc.state, tc.reason)
			}
			wantOutcome, wantMutations := "would_delete", 0
			if tc.apply {
				wantOutcome, wantMutations = "removed", 1
			}
			if report.Decisions[0].Outcome != wantOutcome || mutations != wantMutations {
				t.Fatalf("informational result changed cleanup: report=%+v mutations=%d", report, mutations)
			}
			if !strings.Contains(logs.String(), `"radarr_import"`) {
				t.Fatal("assessment missing from structured decision log")
			}
		})
	}
}
