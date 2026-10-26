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
	"testing"
)

func TestTrackerLabelPlan(t *testing.T) {
	for _, tc := range []struct {
		name             string
		trackers, labels []string
		action           string
		want             []string
		invalid          bool
	}{
		{"add for subdomain", []string{"https://SUB.tracker.example.invalid/announce"}, []string{"User:Keep"}, "add", []string{"User:Keep", ignoredTrackerLabel}, false},
		{"already present", []string{"https://tracker.example.invalid/announce"}, []string{ignoredTrackerLabel, "user:keep"}, "none", []string{ignoredTrackerLabel, "user:keep"}, false},
		{"remove only owned", []string{"https://other.example.invalid/announce"}, []string{"user:keep", ignoredTrackerLabel, "ARRTIFY:ignored-tracker"}, "remove", []string{"user:keep", "ARRTIFY:ignored-tracker"}, false},
		{"remove to empty array", []string{}, []string{ignoredTrackerLabel}, "remove", []string{}, false},
		{"suffix boundary", []string{"https://nottracker.example.invalid/announce"}, []string{}, "none", []string{}, false},
		{"invalid after matching", []string{"https://tracker.example.invalid/announce", "malformed"}, []string{}, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]string{}, tc.labels...)
			d, labels, err := planTrackerLabel(Torrent{Trackers: tc.trackers, Labels: tc.labels}, []string{"tracker.example.invalid"})
			if (err != nil) != tc.invalid {
				t.Fatalf("error = %v", err)
			}
			if tc.invalid {
				return
			}
			if d.Action != tc.action || !reflect.DeepEqual(labels, tc.want) {
				t.Fatalf("plan = %+v, labels = %v", d, labels)
			}
			if !reflect.DeepEqual(tc.labels, original) {
				t.Fatal("planning changed input labels")
			}
		})
	}
}

func TestTrackerLabelSyncFreshReadAndUncertainWrites(t *testing.T) {
	for _, tc := range []struct {
		name, failure, outcome, status string
		sets, changed, errors          int
	}{
		{"foreign labels refreshed and reordered on confirmation", "reordered", "added", "complete", 1, 1, 0},
		{"remove newly stale owned-only label with empty array", "remove_only", "removed", "complete", 1, 1, 0},
		{"another writer already added managed label", "already_current", "unchanged", "complete", 0, 0, 0},
		{"fresh read fails", "fresh_http", "failed", "failed", 0, 0, 1},
		{"fresh trackers incomplete", "fresh_trackers", "failed", "failed", 0, 0, 1},
		{"fresh labels incomplete", "fresh_labels", "failed", "failed", 0, 0, 1},
		{"fresh read returns wrong hash", "wrong_hash", "failed", "failed", 0, 0, 1},
		{"torrent disappeared before write", "fresh_missing", "skipped", "complete", 0, 0, 0},
		{"tracker no longer ignored", "tracker_changed", "unchanged", "complete", 0, 0, 0},
		{"write response lost", "write_http", "unknown", "partial", 1, 0, 1},
		{"readback fails", "confirm_http", "unknown", "partial", 1, 0, 1},
		{"torrent disappears after write", "confirm_missing", "unknown", "partial", 1, 0, 1},
		{"foreign label lost after write", "confirm_labels", "unknown", "partial", 1, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := strings.Repeat("a", 40)
			reads, sets := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string `json:"method"`
					ID     int    `json:"id"`
					Params struct {
						IDs    []string `json:"ids"`
						Labels []string `json:"labels"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if !reflect.DeepEqual(req.Params.IDs, []string{hash}) {
					t.Errorf("target = %v", req.Params.IDs)
				}
				var result any = map[string]any{}
				if req.Method == "torrent_set" {
					sets++
					wantLabels := []string{"user:new", ignoredTrackerLabel}
					if tc.failure == "remove_only" {
						wantLabels = []string{}
					}
					if !reflect.DeepEqual(req.Params.Labels, wantLabels) {
						t.Errorf("did not preserve fresh labels: %v", req.Params.Labels)
					}
					if tc.failure == "write_http" {
						w.WriteHeader(503)
						return
					}
				} else if req.Method == "torrent_get" {
					reads++
					if reads == 1 && tc.failure == "fresh_http" || reads > 1 && tc.failure == "confirm_http" {
						w.WriteHeader(503)
						return
					}
					var fixture map[string]any
					if err := json.Unmarshal(apiFixture(t, "transmission-4.1.3-torrents.json"), &fixture); err != nil {
						t.Error(err)
						return
					}
					raw := fixture["result"].(map[string]any)["torrents"].([]any)[1].(map[string]any)
					raw["trackers"] = []any{map[string]any{"announce": "https://tracker.example.invalid/announce"}}
					raw["labels"] = []string{"user:new"}
					if reads > 1 {
						raw["labels"] = []string{ignoredTrackerLabel, "user:new"}
					}
					switch tc.failure {
					case "remove_only":
						raw["trackers"] = []any{}
						raw["labels"] = []string{ignoredTrackerLabel}
						if reads > 1 {
							raw["labels"] = []string{}
						}
					case "already_current":
						raw["labels"] = []string{ignoredTrackerLabel}
					case "fresh_trackers":
						delete(raw, "trackers")
					case "fresh_labels":
						delete(raw, "labels")
					case "wrong_hash":
						raw["hash_string"] = strings.Repeat("b", 40)
					case "tracker_changed":
						raw["trackers"] = []any{}
					case "confirm_labels":
						if reads > 1 {
							raw["labels"] = []string{ignoredTrackerLabel}
						}
					}
					items := []any{raw}
					if reads == 1 && tc.failure == "fresh_missing" || reads > 1 && tc.failure == "confirm_missing" {
						items = []any{}
					}
					result = map[string]any{"torrents": items}
				} else {
					t.Errorf("unexpected RPC %s", req.Method)
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer server.Close()
			rpc := &transmissionRPC{endpoint: server.URL}
			report := RunReport{Status: "complete", Decisions: []Decision{{Hash: hash, TrackerLabel: &TrackerLabelDecision{Action: "add"}}}}
			syncTrackerLabels(context.Background(), rpc, []string{"tracker.example.invalid"}, &report, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			if sets != tc.sets || report.TrackerLabelsChanged != tc.changed || report.Errors != tc.errors || report.Status != tc.status || report.Decisions[0].TrackerLabel.Outcome != tc.outcome {
				t.Fatalf("sets=%d, report=%+v, label=%+v", sets, report, report.Decisions[0].TrackerLabel)
			}
			if report.Removed != 0 || report.Attempted != 0 {
				t.Fatal("label sync affected cleanup counters")
			}
		})
	}
}

func TestTrackerLabelSyncStopsAfterPartialProgress(t *testing.T) {
	for _, cancelAfterFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed second read", true: "cancelled after first write"}[cancelAfterFirst], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := strings.Repeat("a", 40)
			calls, sets := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req struct {
					Method string `json:"method"`
					ID     int    `json:"id"`
					Params struct {
						IDs []string `json:"ids"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if !reflect.DeepEqual(req.Params.IDs, []string{first}) {
					w.WriteHeader(503)
					return
				}
				var result any = map[string]any{}
				if req.Method == "torrent_set" {
					sets++
				} else {
					var fixture map[string]any
					if err := json.Unmarshal(apiFixture(t, "transmission-4.1.3-torrents.json"), &fixture); err != nil {
						t.Error(err)
						return
					}
					raw := fixture["result"].(map[string]any)["torrents"].([]any)[1].(map[string]any)
					raw["trackers"] = []any{map[string]any{"announce": "https://tracker.example.invalid/announce"}}
					raw["labels"] = []string{}
					if sets > 0 {
						raw["labels"] = []string{ignoredTrackerLabel}
					}
					result = map[string]any{"torrents": []any{raw}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer server.Close()
			report := RunReport{Status: "complete"}
			for _, hash := range []string{first, strings.Repeat("b", 40), strings.Repeat("c", 40)} {
				report.Decisions = append(report.Decisions, Decision{Hash: hash, TrackerLabel: &TrackerLabelDecision{Action: "add"}})
			}
			// Cancel after the first confirmed write, using its observable event.
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			if cancelAfterFirst {
				logger = slog.New(slog.NewJSONHandler(cancelWriter{cancel}, nil))
			}
			syncTrackerLabels(ctx, &transmissionRPC{endpoint: server.URL}, []string{"tracker.example.invalid"}, &report, logger)
			if report.Status != "partial" || report.Errors != 1 || report.TrackerLabelsChanged != 1 || sets != 1 || calls > 4 {
				t.Fatalf("calls=%d, sets=%d, report=%+v", calls, sets, report)
			}
			for i, want := range []string{"added", "failed", "not_attempted"} {
				if got := report.Decisions[i].TrackerLabel.Outcome; got != want {
					t.Errorf("decision %d = %s, want %s", i, got, want)
				}
			}
		})
	}
}

type cancelWriter struct{ cancel context.CancelFunc }

func (w cancelWriter) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }
