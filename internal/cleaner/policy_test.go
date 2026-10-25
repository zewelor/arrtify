package cleaner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDecisionExplainsAppliedThresholds(t *testing.T) {
	for _, tc := range []struct {
		name          string
		category      string
		ratio         float64
		limit         float64
		seeded        int64
		minTime       Duration
		maxTime       *Duration
		protected     bool
		wantOutcome   string
		wantRatioKind string
		wantSeedTime  string
		wantCriteria  map[string]any
	}{
		{
			name:         "minimum time met but ratio below threshold",
			limit:        2,
			category:     "unlinked",
			ratio:        0,
			seeded:       132127,
			minTime:      Duration(24 * time.Hour),
			maxTime:      durationPointer(72 * time.Hour),
			wantOutcome:  "skipped",
			wantSeedTime: "36h42m7s",
			wantCriteria: map[string]any{
				"min_upload_ratio":     float64(2),
				"min_seed_time":        "24h0m0s",
				"max_seed_time":        "72h0m0s",
				"min_upload_ratio_met": false,
				"min_seed_time_met":    true,
				"max_seed_time_met":    false,
			},
		},
		{
			name:         "ratio and minimum equal",
			limit:        2,
			category:     "sonarr",
			ratio:        2,
			seeded:       24 * 3600,
			minTime:      Duration(24 * time.Hour),
			wantOutcome:  "would_delete",
			wantSeedTime: "24h0m0s",
			wantCriteria: map[string]any{
				"min_upload_ratio":     float64(2),
				"min_seed_time":        "24h0m0s",
				"min_upload_ratio_met": true,
				"min_seed_time_met":    true,
			},
		},
		{
			name:         "ratio met but minimum missing",
			limit:        2,
			category:     "sonarr",
			ratio:        3,
			seeded:       24*3600 - 1,
			minTime:      Duration(24 * time.Hour),
			wantOutcome:  "skipped",
			wantSeedTime: "23h59m59s",
			wantCriteria: map[string]any{
				"min_upload_ratio":     float64(2),
				"min_seed_time":        "24h0m0s",
				"min_upload_ratio_met": true,
				"min_seed_time_met":    false,
			},
		},
		{
			name:          "maximum independent of minimum",
			wantRatioKind: "infinite",
			limit:         2,
			category:      "radarr",
			ratio:         -2,
			seeded:        3600,
			minTime:       Duration(24 * time.Hour),
			maxTime:       durationPointer(time.Hour),
			wantOutcome:   "would_delete",
			wantSeedTime:  "1h0m0s",
			wantCriteria: map[string]any{
				"min_upload_ratio":     float64(2),
				"min_seed_time":        "24h0m0s",
				"max_seed_time":        "1h0m0s",
				"min_upload_ratio_met": false,
				"min_seed_time_met":    false,
				"max_seed_time_met":    true,
			},
		},
		{
			name:     "unavailable ratio is a sentinel",
			category: "sonarr", ratio: -1, limit: 2, seeded: 24 * 3600,
			minTime: Duration(24 * time.Hour), wantOutcome: "skipped",
			wantSeedTime: "24h0m0s", wantRatioKind: "not_available",
			wantCriteria: map[string]any{
				"min_upload_ratio": float64(2), "min_seed_time": "24h0m0s",
				"min_upload_ratio_met": false, "min_seed_time_met": true,
			},
		},
		{
			name:         "explicit zero thresholds",
			limit:        0,
			category:     "sonarr",
			ratio:        0,
			seeded:       0,
			minTime:      Duration(0),
			wantOutcome:  "would_delete",
			wantSeedTime: "0h0m0s",
			wantCriteria: map[string]any{
				"min_upload_ratio":     float64(0),
				"min_seed_time":        "0s",
				"min_upload_ratio_met": true,
				"min_seed_time_met":    true,
			},
		},
		{
			name:         "large counter does not wrap display",
			limit:        2,
			category:     "sonarr",
			ratio:        0,
			seeded:       9223372036854775807,
			minTime:      Duration(24 * time.Hour),
			wantOutcome:  "skipped",
			wantSeedTime: "2562047788015215h30m7s",
			wantCriteria: map[string]any{
				"min_upload_ratio":     float64(2),
				"min_seed_time":        "24h0m0s",
				"min_upload_ratio_met": false,
				"min_seed_time_met":    true,
			},
		},
		{
			name:         "Arr guard does not claim thresholds evaluated",
			limit:        2,
			category:     "sonarr",
			ratio:        3,
			seeded:       24 * 3600,
			minTime:      Duration(24 * time.Hour),
			protected:    true,
			wantOutcome:  "skipped",
			wantSeedTime: "24h0m0s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := testConfig(t)
			config.Rules = []Rule{{Name: "chosen", Categories: []string{tc.category}, Ratio: &tc.limit, MinSeedTime: &tc.minTime, MaxSeedTime: tc.maxTime}}
			torrent := sampleTorrent()
			torrent.Hash = strings.Repeat("A", 40)
			torrent.DownloadDir = "/downloads/" + tc.category
			torrent.Ratio = tc.ratio
			torrent.SecondsSeeding = tc.seeded
			decision, err := Evaluate(config, torrent, tc.protected)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(decision)
			if err != nil {
				t.Fatal(err)
			}
			var observation struct {
				UploadRatio     float64 `json:"upload_ratio"`
				SeedTimeSeconds int64   `json:"seed_time_seconds"`
			}
			if err := json.Unmarshal(data, &observation); err != nil {
				t.Fatal(err)
			}
			if observation.UploadRatio != tc.ratio || observation.SeedTimeSeconds != tc.seeded {
				t.Fatalf("observation replaced by a threshold: %s", data)
			}
			var output map[string]any
			if err := json.Unmarshal(data, &output); err != nil {
				t.Fatal(err)
			}
			if _, legacy := output["seconds_seeding"]; legacy {
				t.Fatalf("ambiguous legacy time name: %s", data)
			}
			if output["seed_time_seconds"] == nil {
				t.Fatalf("missing measured seconds: %s", data)
			}
			wantKind := tc.wantRatioKind
			if wantKind == "" {
				wantKind = "finite"
			}
			if output["upload_ratio_kind"] != wantKind {
				t.Fatalf("unexplained ratio representation: %s", data)
			}
			if decision.Outcome != tc.wantOutcome || output["seed_time"] != tc.wantSeedTime {
				t.Fatalf("unexplained decision: %s", data)
			}
			criteria, ok := output["criteria"].(map[string]any)
			if tc.wantCriteria == nil {
				if _, present := output["rule"]; present {
					t.Fatalf("unused rule reported: %s", data)
				}
				if _, present := output["criteria"]; present {
					t.Fatalf("unevaluated criteria reported: %s", data)
				}
				return
			}
			if _, legacy := criteria["ratio"]; legacy {
				t.Fatalf("unqualified ratio threshold: %s", data)
			}
			if _, legacy := criteria["ratio_met"]; legacy {
				t.Fatalf("unqualified ratio predicate: %s", data)
			}
			if !ok || len(criteria) != len(tc.wantCriteria) {
				t.Fatalf("missing or extra criteria: %s", data)
			}
			for key, want := range tc.wantCriteria {
				if criteria[key] != want {
					t.Errorf("%s: got %v, want %v", key, criteria[key], want)
				}
			}
		})
	}
}

func durationPointer(value time.Duration) *Duration { d := Duration(value); return &d }
