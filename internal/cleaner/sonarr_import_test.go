package cleaner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestReadSonarrImports(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name             string
		history          []any
		episodes         map[int][]any
		state            string
		reason           string
		importedEpisodes int
	}{
		{
			name: "season pack and shared multi-episode file",
			history: []any{
				sonarrImportHistory(1, hash, 42, 101, "500"),
				sonarrImportHistory(2, hash, 42, 101, "500"), // repeated import ref
				sonarrImportHistory(3, hash, 42, 102, "500"), // two episodes share one file
				sonarrImportHistory(4, hash, 42, 103, "501"),
			},
			episodes: map[int][]any{42: {
				sonarrCurrentEpisode(101, 42, 500),
				sonarrCurrentEpisode(102, 42, 500),
				sonarrCurrentEpisode(103, 42, 501),
			}},
			state: "current", reason: "current_episode_file", importedEpisodes: 3,
		},
		{
			name:     "page two uppercase hash",
			history:  sonarrPagedHistory(hash),
			episodes: map[int][]any{42: {sonarrCurrentEpisode(101, 42, 500)}},
			state:    "current", reason: "current_episode_file", importedEpisodes: 1,
		},
		{
			name:    "no matching import history",
			history: []any{sonarrImportHistory(1, strings.Repeat("b", 40), 42, 101, "500")},
			state:   "unknown", reason: "no_import_history", importedEpisodes: 0,
		},
		{
			name: "ambiguous episode import beats a current file",
			history: []any{
				sonarrImportHistory(1, hash, 42, 101, "500"),
				sonarrImportHistory(2, hash, 42, 101, "501"),
			},
			episodes: map[int][]any{42: {sonarrCurrentEpisode(101, 42, 500)}},
			state:    "unknown", reason: "ambiguous_import_history", importedEpisodes: 1,
		},
		{
			name: "current episode beats another missing episode",
			history: []any{
				sonarrImportHistory(1, hash, 42, 101, "500"),
				sonarrImportHistory(2, hash, 42, 102, "501"),
			},
			episodes: map[int][]any{42: {sonarrCurrentEpisode(101, 42, 500)}},
			state:    "current", reason: "current_episode_file", importedEpisodes: 2,
		},
		{
			name:     "episode missing",
			history:  []any{sonarrImportHistory(1, hash, 42, 101, "500")},
			episodes: map[int][]any{42: {sonarrCurrentEpisode(102, 42, 500)}},
			state:    "unknown", reason: "episode_missing", importedEpisodes: 1,
		},
		{
			name:     "episode file missing",
			history:  []any{sonarrImportHistory(1, hash, 42, 101, "500")},
			episodes: map[int][]any{42: {sonarrCurrentEpisode(101, 42, 0)}},
			state:    "unknown", reason: "episode_file_missing", importedEpisodes: 1,
		},
		{
			name: "all current files differ",
			history: []any{
				sonarrImportHistory(1, hash, 42, 101, "500"),
				sonarrImportHistory(2, hash, 42, 102, "501"),
			},
			episodes: map[int][]any{
				42: {sonarrCurrentEpisode(101, 42, 600), sonarrCurrentEpisode(102, 42, 601)},
			},
			state: "replaced", reason: "different_current_files", importedEpisodes: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &sonarrImportTestAPI{history: tc.history, episodes: tc.episodes}
			server := newSonarrImportTestServer(t, api)
			defer server.Close()

			c := testConfig(t)
			c.Sonarr.URL = server.URL
			c.Sonarr.ImportedLabel = "tv-finished"
			got, err := readSonarrImports(context.Background(), c, []Torrent{{
				Hash: hash, Labels: []string{"user:keep", "TV-FINISHED"},
			}})
			if err != nil {
				t.Fatalf("readSonarrImports: %v", err)
			}
			decision := got[hash]
			if decision == nil {
				t.Fatalf("missing assessment for %s: %#v", hash, got)
			}
			if decision.State != tc.state || decision.Reason != tc.reason {
				t.Fatalf("assessment=%+v, want %s/%s", decision, tc.state, tc.reason)
			}
			if decision.ImportedEpisodes != tc.importedEpisodes {
				t.Fatalf("imported episodes=%d, want %d", decision.ImportedEpisodes, tc.importedEpisodes)
			}
			var encoded map[string]json.RawMessage
			body, err := json.Marshal(decision)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &encoded); err != nil {
				t.Fatal(err)
			}
			var importedEpisodes int
			if err := json.Unmarshal(encoded["imported_episodes"], &importedEpisodes); err != nil {
				t.Fatalf("imported_episodes JSON field: %v; decision=%s", err, body)
			}
			if importedEpisodes != tc.importedEpisodes {
				t.Fatalf("JSON imported_episodes=%d, want %d", importedEpisodes, tc.importedEpisodes)
			}
			if tc.name == "season pack and shared multi-episode file" && api.episodeRequests["42"] != 1 {
				t.Fatalf("episode series 42 requested %d times, want once", api.episodeRequests["42"])
			}
		})
	}
}

func TestReadSonarrImportsSkipsUnselectedTorrents(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		labels     []string
	}{
		{name: "disabled label", labels: []string{"TV-FINISHED"}},
		{name: "torrent has no labels", configured: "tv-finished"},
		{name: "legacy label is not the configured label", configured: "tv-finished", labels: []string{"sonarr-imported"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &sonarrImportTestAPI{}
			server := newSonarrImportTestServer(t, api)
			defer server.Close()

			c := testConfig(t)
			c.Sonarr.URL = server.URL
			c.Sonarr.ImportedLabel = yamlLabel(tc.configured)
			got, err := readSonarrImports(context.Background(), c, []Torrent{{
				Hash: strings.Repeat("a", 40), Labels: tc.labels,
			}})
			if err != nil {
				t.Fatalf("readSonarrImports: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("assessments=%#v, want empty", got)
			}
			if len(api.requests) != 0 {
				t.Fatalf("unselected torrent triggered Sonarr requests: %v", api.requests)
			}
		})
	}
}

func TestReadSonarrImportsRejectsIncompleteData(t *testing.T) {
	hash := strings.Repeat("a", 40)
	validImport := sonarrImportHistory(1, hash, 42, 101, "500")
	for _, tc := range []struct {
		name               string
		history            []any
		historyStatus      int
		historyBody        string
		shortHistory       bool
		episodes           map[int][]any
		episodeStatus      int
		episodeBody        string
		wantEpisodeRequest bool
	}{
		{name: "history HTTP failure", historyStatus: http.StatusServiceUnavailable},
		{name: "malformed history JSON", historyBody: "{"},
		{name: "short history page", history: []any{validImport}, shortHistory: true},
		{
			name: "invalid matching import IDs",
			history: []any{map[string]any{
				"id": 1, "eventType": "downloadFolderImported", "downloadId": hash,
				"seriesId": 42, "episodeId": 101, "data": map[string]any{"fileId": "not-a-number"},
			}},
		},
		{
			name:    "episode missing id",
			history: []any{validImport}, episodes: map[int][]any{42: {map[string]any{"seriesId": 42, "episodeFileId": 500}}},
			wantEpisodeRequest: true,
		},
		{
			name:    "episode missing series ID",
			history: []any{validImport}, episodes: map[int][]any{42: {map[string]any{"id": 101, "episodeFileId": 500}}},
			wantEpisodeRequest: true,
		},
		{
			name:    "episode missing file ID",
			history: []any{validImport}, episodes: map[int][]any{42: {map[string]any{"id": 101, "seriesId": 42}}},
			wantEpisodeRequest: true,
		},
		{
			name:    "duplicate episode IDs",
			history: []any{validImport}, episodes: map[int][]any{42: {
				sonarrCurrentEpisode(101, 42, 500), sonarrCurrentEpisode(101, 42, 500),
			}},
			wantEpisodeRequest: true,
		},
		{
			name:    "series mismatch",
			history: []any{validImport}, episodes: map[int][]any{42: {sonarrCurrentEpisode(101, 43, 500)}},
			wantEpisodeRequest: true,
		},
		{
			name:    "episode HTTP failure",
			history: []any{validImport}, episodeStatus: http.StatusServiceUnavailable,
			wantEpisodeRequest: true,
		},
		{
			name:    "malformed episode JSON",
			history: []any{validImport}, episodeBody: "[",
			wantEpisodeRequest: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &sonarrImportTestAPI{
				history: tc.history, historyStatus: tc.historyStatus, historyBody: tc.historyBody,
				shortHistory: tc.shortHistory, episodes: tc.episodes,
				episodeStatus: tc.episodeStatus, episodeBody: tc.episodeBody,
			}
			server := newSonarrImportTestServer(t, api)
			defer server.Close()

			c := testConfig(t)
			c.Sonarr.URL = server.URL
			c.Sonarr.ImportedLabel = "tv-finished"
			got, err := readSonarrImports(context.Background(), c, []Torrent{{
				Hash: hash, Labels: []string{"tv-finished"},
			}})
			if err == nil {
				t.Fatalf("incomplete Sonarr data was accepted: %#v", got)
			}
			if strings.Contains(err.Error(), "secret-canary") {
				t.Fatalf("unsafe response detail leaked in error: %v", err)
			}
			if api.episodeRequests["42"] > 0 != tc.wantEpisodeRequest {
				t.Fatalf("episode request count=%d, want requested=%t", api.episodeRequests["42"], tc.wantEpisodeRequest)
			}
		})
	}
}

func sonarrImportHistory(id int, hash string, seriesID, episodeID int, fileID string) map[string]any {
	return map[string]any{
		"id": id, "eventType": "downloadFolderImported", "downloadId": hash,
		"seriesId": seriesID, "episodeId": episodeID,
		"data": map[string]any{"fileId": fileID},
	}
}

func sonarrCurrentEpisode(id, seriesID, episodeFileID int) map[string]any {
	return map[string]any{"id": id, "seriesId": seriesID, "episodeFileId": episodeFileID}
}

func sonarrPagedHistory(hash string) []any {
	result := make([]any, 0, 101)
	for id := 1; id <= 100; id++ {
		result = append(result, map[string]any{"id": id, "eventType": "grabbed"})
	}
	result = append(result, sonarrImportHistory(101, strings.ToUpper(hash), 42, 101, "500"))
	return result
}

type sonarrImportTestAPI struct {
	history         []any
	historyStatus   int
	historyBody     string
	shortHistory    bool
	episodes        map[int][]any
	episodeStatus   int
	episodeBody     string
	requests        []string
	episodeRequests map[string]int
}

func newSonarrImportTestServer(t *testing.T, api *sonarrImportTestAPI) *httptest.Server {
	t.Helper()
	api.episodeRequests = make(map[string]int)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Sonarr import assessment made %s request to %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		api.requests = append(api.requests, r.URL.Path)
		switch r.URL.Path {
		case "/api/v3/history":
			if r.URL.Query().Get("sortKey") != "date" || r.URL.Query().Get("sortDirection") != "descending" || r.URL.Query().Get("pageSize") != "100" {
				t.Errorf("history query=%v, want date descending pages of 100", r.URL.Query())
			}
			if api.historyStatus != 0 {
				http.Error(w, "secret-canary", api.historyStatus)
				return
			}
			if api.historyBody != "" {
				_, _ = w.Write([]byte(api.historyBody))
				return
			}
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if err != nil || page < 1 {
				t.Errorf("invalid history page %q", r.URL.Query().Get("page"))
				http.Error(w, "invalid page", http.StatusBadRequest)
				return
			}
			start := (page - 1) * 100
			end := min(start+100, len(api.history))
			records := []any{}
			if start < len(api.history) {
				records = api.history[start:end]
			}
			total := len(api.history)
			if api.shortHistory {
				total++
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"page": page, "pageSize": 100, "sortKey": "date", "sortDirection": "descending",
				"totalRecords": total, "records": records,
			})
		case "/api/v3/episode":
			seriesID := r.URL.Query().Get("seriesId")
			api.episodeRequests[seriesID]++
			if api.episodeStatus != 0 {
				http.Error(w, "secret-canary", api.episodeStatus)
				return
			}
			if api.episodeBody != "" {
				_, _ = w.Write([]byte(api.episodeBody))
				return
			}
			id, err := strconv.Atoi(seriesID)
			if err != nil {
				t.Errorf("invalid episode seriesId %q", seriesID)
				http.Error(w, "invalid series", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(api.episodes[id])
		default:
			t.Errorf("unexpected Sonarr endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}
