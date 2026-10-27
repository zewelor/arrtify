package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zewelor/arrtify/internal/cleaner"
)

var binary string
var evidence []scenarioResult
var containerEvidence []scenarioResult

type scenarioResult struct {
	Name          string            `json:"name"`
	Passed        bool              `json:"passed"`
	Exit          int               `json:"exit"`
	Requests      int64             `json:"requests"`
	Mutations     int64             `json:"mutations"`
	Report        cleaner.RunReport `json:"report"`
	requestStart  int64
	mutationStart int64
}

type rpcCall struct {
	Method string                     `json:"method"`
	Params map[string]json.RawMessage `json:"params"`
}

func TestMain(m *testing.M) {
	if os.Getenv("ARRTIFY_E2E_FIXTURE_SERVER") == "1" {
		os.Exit(runFixtureServer())
	}

	dir, err := os.MkdirTemp("", "arrtify-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "arrtify")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/arrtify")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build CLI: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.MkdirAll("../../artifacts", 0755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	} else {
		if len(evidence) > 0 {
			b, err := json.MarshalIndent(evidence, "", "  ")
			if err == nil {
				err = os.WriteFile("../../artifacts/e2e.json", append(b, '\n'), 0644)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
		}
		if len(containerEvidence) > 0 {
			b, err := json.MarshalIndent(containerEvidence, "", "  ")
			if err == nil {
				err = os.WriteFile("../../artifacts/container-e2e.json", append(b, '\n'), 0644)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
		}
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

func runFixtureServer() int {
	fixture := &apiFixture{failService: os.Getenv("ARRTIFY_E2E_FIXTURE_FAIL_SERVICE")}
	listener, err := net.Listen("tcp", ":18080")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture listen failed")
		return 1
	}
	server := &http.Server{Handler: http.HandlerFunc(fixture.serve)}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()
	fmt.Fprintln(os.Stdout, "ARRTIFY_FIXTURE_READY")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-serveDone:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "fixture server stopped")
			return 1
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintln(os.Stderr, "fixture shutdown failed")
		return 1
	}
	state := map[string]int64{
		"fixture_requests":  fixture.requests.Load(),
		"fixture_mutations": fixture.mutations.Load(),
	}
	if err := json.NewEncoder(os.Stdout).Encode(state); err != nil {
		fmt.Fprintln(os.Stderr, "fixture state output failed")
		return 1
	}
	return 0
}

type apiFixture struct {
	requests, mutations            atomic.Int64
	failService                    string
	invalidTorrent, invalidVersion bool
	belowThresholds                bool
	started                        chan struct{}
	syncTrackerLabels              bool
	failLabelReadback              bool
	labelWriteSeen                 bool
	allowTorrentRemove             bool
	eligibleF                      bool
	uncertainRemoveHash            string
	protectHashOnRevalidation      string
	deletedHashes                  map[string]bool
	ignoredTrackerHashes           map[string]bool
	labels                         map[string][]string
	sonarrReads                    atomic.Int64
	rpcMu                          sync.Mutex
	rpcCalls                       []rpcCall
	trace                          []string
}

func recordScenario(t *testing.T, name string, fixture *apiFixture) *scenarioResult {
	t.Helper()
	result := &scenarioResult{
		Name:          name,
		Exit:          -1,
		requestStart:  fixture.requests.Load(),
		mutationStart: fixture.mutations.Load(),
	}
	t.Cleanup(func() {
		result.Passed = !t.Failed()
		result.Requests = fixture.requests.Load() - result.requestStart
		result.Mutations = fixture.mutations.Load() - result.mutationStart
		evidence = append(evidence, *result)
	})
	return result
}

func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if r.Method == http.MethodPost {
		var req struct {
			Method string                     `json:"method"`
			ID     int                        `json:"id"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if req.Method != "session_get" && req.Method != "torrent_get" && !(f.syncTrackerLabels && req.Method == "torrent_set") && !(f.allowTorrentRemove && req.Method == "torrent_remove") {
			f.mutations.Add(1)
			http.Error(w, "unexpected mutation", 500)
			return
		}
		if f.started != nil {
			close(f.started)
			<-r.Context().Done()
			return
		}
		if r.Header.Get("X-Transmission-Session-Id") != "synthetic-session" {
			w.Header().Set("X-Transmission-Session-Id", "synthetic-session")
			w.WriteHeader(409)
			return
		}
		f.recordRPCCall(rpcCall{Method: req.Method, Params: req.Params})
		var result any
		if req.Method == "session_get" {
			version := "4.1.3"
			if f.invalidVersion {
				version = "4.2.0"
			}
			result = map[string]any{"version": version, "rpc_version_semver": "6.0.1"}
		} else if req.Method == "torrent_get" {
			f.recordTrace("rpc:torrent_get:" + targetHash(req.Params))
			torrents, err := f.torrents(req.Params)
			if err != nil {
				http.Error(w, "invalid torrent_get arguments", 400)
				return
			}
			result = map[string]any{"torrents": torrents}
		} else if req.Method == "torrent_set" {
			if err := f.setLabels(req.Params); err != nil {
				http.Error(w, "invalid torrent_set arguments", 400)
				return
			}
			f.mutations.Add(1)
			result = map[string]any{}
		} else {
			hash, err := f.removeTorrent(req.Params)
			if err != nil {
				http.Error(w, "invalid torrent_remove arguments", 400)
				return
			}
			f.recordTrace("rpc:torrent_remove:" + hash)
			f.mutations.Add(1)
			f.rpcMu.Lock()
			if f.deletedHashes == nil {
				f.deletedHashes = map[string]bool{}
			}
			f.deletedHashes[hash] = true
			f.rpcMu.Unlock()
			if hash == f.uncertainRemoveHash {
				http.Error(w, "transmission response was interrupted", http.StatusBadGateway)
				return
			}
			result = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		return
	}
	service := "radarr"
	if strings.HasPrefix(r.URL.Path, "/sonarr/") {
		service = "sonarr"
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	f.recordTrace(fmt.Sprintf("arr:%s:%d", service, page))
	if f.failService == service {
		http.Error(w, "api-secret-canary", 403)
		return
	}
	if r.Method != "GET" || r.Header.Get("X-Api-Key") != "api-secret-canary" {
		http.Error(w, "invalid request", 400)
		return
	}
	if r.URL.Path == "/radarr/api/v3/history" {
		if f.failService == "radarr_history" {
			http.Error(w, "api-secret-canary", http.StatusServiceUnavailable)
			return
		}
		records := []any{
			map[string]any{"id": 1, "eventType": "downloadFolderImported", "downloadId": hashFor('a'),
				"movieId": 10, "data": map[string]string{"fileId": "1"}},
			map[string]any{"id": 2, "eventType": "downloadFolderImported", "downloadId": hashFor('b'),
				"movieId": 11, "data": map[string]string{"fileId": "3"}},
			map[string]any{"id": 3, "eventType": "grabbed", "downloadId": hashFor('d')},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"page": 1, "pageSize": 100, "sortKey": "date", "sortDirection": "descending",
			"totalRecords": len(records), "records": records,
		})
		return
	}
	if r.URL.Path == "/radarr/api/v3/movie" {
		_ = json.NewEncoder(w).Encode([]any{
			map[string]int{"id": 10, "movieFileId": 2}, map[string]int{"id": 11, "movieFileId": 3},
		})
		return
	}
	if r.URL.Path == "/sonarr/api/v3/history" {
		if f.failService == "sonarr_history" {
			http.Error(w, "api-secret-canary", http.StatusServiceUnavailable)
			return
		}
		records := []any{}
		for id, ref := range []struct {
			hash                  byte
			series, episode, file int
		}{
			{hash: 'b', series: 42, episode: 101, file: 500},
			{hash: 'b', series: 42, episode: 102, file: 501},
			{hash: 'f', series: 43, episode: 201, file: 700},
			{hash: 'f', series: 43, episode: 202, file: 700},
		} {
			records = append(records, map[string]any{
				"id": id + 1, "eventType": "downloadFolderImported", "downloadId": hashFor(ref.hash),
				"seriesId": ref.series, "episodeId": ref.episode,
				"data": map[string]string{"fileId": strconv.Itoa(ref.file)},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"page": 1, "pageSize": 100, "sortKey": "date", "sortDirection": "descending",
			"totalRecords": len(records), "records": records,
		})
		return
	}
	if r.URL.Path == "/sonarr/api/v3/episode" {
		if f.failService == "sonarr_episode" {
			http.Error(w, "api-secret-canary", http.StatusServiceUnavailable)
			return
		}
		episodes := []any{}
		switch r.URL.Query().Get("seriesId") {
		case "42":
			episodes = []any{
				map[string]int{"id": 101, "seriesId": 42, "episodeFileId": 500},
				map[string]int{"id": 102, "seriesId": 42, "episodeFileId": 600},
			}
		case "43":
			episodes = []any{
				map[string]int{"id": 201, "seriesId": 43, "episodeFileId": 701},
				map[string]int{"id": 202, "seriesId": 43, "episodeFileId": 701},
			}
		default:
			http.Error(w, "invalid seriesId", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(episodes)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/api/v3/queue") {
		http.Error(w, "invalid request", 400)
		return
	}
	parameter := "includeUnknownMovieItems"
	if service == "sonarr" {
		parameter = "includeUnknownSeriesItems"
	}
	if r.URL.Query().Get(parameter) != "true" {
		http.Error(w, "unknown items missing", 400)
		return
	}
	records := []any{}
	total := 1
	if service == "sonarr" {
		protectHash := ""
		if f.sonarrReads.Add(1) > 2 {
			protectHash = f.protectHashOnRevalidation
		}
		total = 101
		if page == 1 {
			for i := 1; i <= 100; i++ {
				var hash any
				if i == 1 && protectHash != "" {
					hash = protectHash
				} else if i%2 == 0 {
					hash = ""
				}
				records = append(records, map[string]any{"id": i, "downloadId": hash, "status": "warning"})
			}
		} else {
			records = append(records, map[string]any{"id": 101, "downloadId": strings.Repeat("B", 40), "status": "completed"})
		}
	} else {
		records = append(records, map[string]any{"id": 1, "downloadId": strings.Repeat("c", 40), "status": "failed"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"page": page, "pageSize": 100, "sortKey": "timeleft", "sortDirection": "descending", "totalRecords": total, "records": records, "extra": "allowed"})
}

func (f *apiFixture) recordRPCCall(call rpcCall) {
	f.rpcMu.Lock()
	defer f.rpcMu.Unlock()
	f.rpcCalls = append(f.rpcCalls, call)
}

func (f *apiFixture) rpcCallSnapshot() []rpcCall {
	f.rpcMu.Lock()
	defer f.rpcMu.Unlock()
	return append([]rpcCall(nil), f.rpcCalls...)
}

func (f *apiFixture) recordTrace(event string) {
	f.rpcMu.Lock()
	defer f.rpcMu.Unlock()
	f.trace = append(f.trace, event)
}

func (f *apiFixture) traceSnapshot() []string {
	f.rpcMu.Lock()
	defer f.rpcMu.Unlock()
	return append([]string(nil), f.trace...)
}

func targetHash(params map[string]json.RawMessage) string {
	var ids []string
	if err := json.Unmarshal(params["ids"], &ids); err != nil || len(ids) == 0 {
		return "*"
	}
	return ids[0]
}

func (f *apiFixture) removeTorrent(params map[string]json.RawMessage) (string, error) {
	var ids []string
	if err := json.Unmarshal(params["ids"], &ids); err != nil || len(ids) != 1 {
		return "", fmt.Errorf("torrent_remove must target one hash")
	}
	var deleteLocalData bool
	if err := json.Unmarshal(params["delete_local_data"], &deleteLocalData); err != nil || !deleteLocalData {
		return "", fmt.Errorf("torrent_remove must delete local data")
	}
	hash := ids[0]
	if len(hash) != 40 || hash != strings.ToLower(hash) {
		return "", fmt.Errorf("torrent_remove hash is not canonical")
	}
	return hash, nil
}

func (f *apiFixture) torrents(arguments map[string]json.RawMessage) ([]map[string]any, error) {
	var fields []string
	if err := json.Unmarshal(arguments["fields"], &fields); err != nil || !containsString(fields, "labels") {
		return nil, fmt.Errorf("torrent_get must request labels")
	}
	var ids []string
	if raw, ok := arguments["ids"]; ok {
		if err := json.Unmarshal(raw, &ids); err != nil || len(ids) == 0 {
			return nil, fmt.Errorf("invalid ids")
		}
	}
	torrents := []map[string]any{}
	for _, c := range []byte{'a', 'b', 'c', 'd', 'e', 'f'} {
		hash := strings.Repeat(string(c), 40)
		if len(ids) > 0 && !containsString(ids, hash) {
			continue
		}
		f.rpcMu.Lock()
		deleted := f.deletedHashes[hash]
		f.rpcMu.Unlock()
		if deleted {
			continue
		}
		trackerList := []any{}
		if c == 'd' || f.ignoredTrackerHashes[hash] {
			trackerList = []any{map[string]any{"announce": "udp://announce.tracker.example.invalid:80/path?passkey=tracker-secret-canary"}}
		}
		labels := f.currentLabels(hash)
		f.rpcMu.Lock()
		readbackMismatch := f.failLabelReadback && f.labelWriteSeen && len(ids) == 1 && ids[0] == hash
		f.rpcMu.Unlock()
		if readbackMismatch {
			labels = []string{}
		}
		tr := map[string]any{
			"hash_string": hash, "name": "Synthetic " + string(c), "download_dir": "/downloads/sonarr",
			"upload_ratio": 2, "seconds_seeding": 24 * 3600, "status": 6, "is_finished": false,
			"left_until_done": 0, "metadata_percent_complete": 1, "trackers": trackerList, "labels": labels,
		}
		switch c {
		case 'e':
			tr["download_dir"] = "/downloads/radarr"
			tr["upload_ratio"] = -2
			tr["seconds_seeding"] = 168 * 3600
		case 'f':
			if !f.eligibleF {
				tr["status"] = 0
			}
		}
		if f.belowThresholds && c == 'a' {
			tr["download_dir"] = "/downloads/unlinked"
			tr["upload_ratio"] = 0
			tr["seconds_seeding"] = 132127
		}
		if f.invalidTorrent {
			tr["trackers"] = nil
		}
		torrents = append(torrents, tr)
	}
	return torrents, nil
}

func (f *apiFixture) currentLabels(hash string) []string {
	f.rpcMu.Lock()
	defer f.rpcMu.Unlock()
	return append([]string{}, f.labels[hash]...)
}

func (f *apiFixture) setLabels(arguments map[string]json.RawMessage) error {
	var ids, labels []string
	if err := json.Unmarshal(arguments["ids"], &ids); err != nil || len(ids) != 1 {
		return fmt.Errorf("torrent_set must target one hash")
	}
	if err := json.Unmarshal(arguments["labels"], &labels); err != nil {
		return fmt.Errorf("torrent_set labels missing")
	}
	if len(ids[0]) != 40 || ids[0] != strings.ToLower(ids[0]) {
		return fmt.Errorf("torrent_set hash is not canonical")
	}
	known := false
	for _, c := range []byte{'a', 'b', 'c', 'd', 'e', 'f'} {
		if ids[0] == strings.Repeat(string(c), 40) {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("torrent_set hash is unknown")
	}
	f.rpcMu.Lock()
	if f.labels == nil {
		f.labels = map[string][]string{}
	}
	f.labels[ids[0]] = append([]string{}, labels...)
	f.labelWriteSeen = true
	f.rpcMu.Unlock()
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cliEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "TRANSMISSION_USER=user-secret-canary", "TRANSMISSION_PASSWORD=password-secret-canary", "SONARR_API_KEY=api-secret-canary", "RADARR_API_KEY=api-secret-canary"}
}
func configFile(t *testing.T, url string, invalid bool) string {
	t.Helper()
	b, err := os.ReadFile("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "http://transmission.example.invalid:9091/transmission/rpc", url+"/transmission/rpc")
	s = strings.ReplaceAll(s, "http://sonarr.example.invalid:8989", url+"/sonarr")
	s = strings.ReplaceAll(s, "http://radarr.example.invalid:7878", url+"/radarr")
	if invalid {
		s += "unknown: config-secret-canary\n"
	}
	file := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(file, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func configFileWithoutCleanupCandidates(t *testing.T, url string) string {
	t.Helper()
	file := configFile(t, url, false)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	config := strings.ReplaceAll(string(data), "ratio: 2", "ratio: 9999")
	config = strings.Replace(config, "max_seed_time: 168h", "max_seed_time: 336h", 1)
	if err := os.WriteFile(file, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func verifyOutput(t *testing.T, out, logs []byte, code int) cleaner.RunReport {
	t.Helper()
	for _, secret := range []string{"user-secret-canary", "password-secret-canary", "api-secret-canary", "tracker-secret-canary", "config-secret-canary"} {
		if bytes.Contains(out, []byte(secret)) || bytes.Contains(logs, []byte(secret)) {
			t.Fatal("secret leaked")
		}
	}
	var report cleaner.RunReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("exit %d invalid report: %v\n%s\n%s", code, err, out, logs)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if _, legacy := payload["version"]; legacy || payload["app_version"] != report.Version {
		t.Fatal("ambiguous application version field")
	}
	if report.RunID == "" || report.Version == "" || report.DurationSeconds < 0 {
		t.Fatalf("missing run metadata %+v", report)
	}
	if report.Scanned != report.Skipped+report.WouldDelete+report.Removed+report.Failed+report.Unknown+report.NotAttempted {
		t.Fatalf("unbalanced outcomes: %+v", report)
	}
	if len(report.Decisions) != report.Scanned || (report.Mode == "dry-run" && (report.Attempted != 0 || report.Removed != 0 || report.Unknown != 0)) {
		t.Fatalf("unexpected decisions/mutations: %+v", report)
	}
	expected := map[string]map[string]any{}
	for _, d := range report.Decisions {
		encoded, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		expected[d.Hash] = fields
	}
	decoder := json.NewDecoder(bytes.NewReader(logs))
	events := []string{}
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if _, legacy := event["version"]; legacy {
			t.Fatal("ambiguous version field in log")
		}
		if event["run_id"] != report.RunID || event["mode"] != report.Mode || event["app_version"] != report.Version {
			t.Fatal("log/report metadata mismatch")
		}
		if event["event"] == "run_finished" {
			for _, key := range []string{"scanned", "planned", "attempted", "skipped", "would_delete", "removed", "failed", "unknown", "not_attempted", "errors", "status"} {
				if !reflect.DeepEqual(event[key], payload[key]) {
					t.Fatalf("summary/report mismatch for %s", key)
				}
			}
			for _, key := range []string{"tracker_labels_planned", "tracker_labels_changed", "dry_run", "sync_tracker_label", "radarr_imported_label", "sonarr_imported_label"} {
				if _, present := payload[key]; present && !reflect.DeepEqual(event[key], payload[key]) {
					t.Fatalf("summary/report mismatch for %s", key)
				}
			}
		}
		if event["event"] == "torrent_decision" {
			hash, _ := event["torrent_hash"].(string)
			want, ok := expected[hash]
			if !ok {
				t.Fatalf("logged unknown decision %s", hash)
			}
			for _, key := range []string{"upload_ratio", "upload_ratio_kind", "seed_time_seconds", "seed_time", "rule", "criteria", "radarr_import", "sonarr_import"} {
				actual, logged := event[key]
				wanted, present := want[key]
				if logged != present || !reflect.DeepEqual(actual, wanted) {
					t.Fatalf("log/report mismatch for %s: %v versus %v", key, actual, wanted)
				}
			}
			if want["rule"] != nil && want["criteria"] == nil {
				t.Fatal("matched rule lacks decision criteria")
			}
		}
		events = append(events, event["event"].(string))
	}
	if len(events) < 2 || events[0] != "run_started" || events[len(events)-1] != "run_finished" {
		t.Fatalf("events: %v", events)
	}
	return report
}

func TestImportReportingStartupStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		check  bool
	}{
		{name: "both omitted"},
		{name: "Sonarr configured in offline check", labels: map[string]string{"sonarr": "tv-finished", "radarr": ""}, check: true},
		{name: "Radarr configured and Sonarr empty", labels: map[string]string{"sonarr": "", "radarr": "movies-finished"}},
		{name: "both configured", labels: map[string]string{"sonarr": "tv-finished", "radarr": "movies-finished"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &apiFixture{}
			result := recordScenario(t, "import reporting startup: "+tc.name, fixture)
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			config := configFile(t, server.URL, false)
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			for service, label := range tc.labels {
				text = strings.Replace(text, service+":\n", service+":\n  imported_label: "+strconv.Quote(label)+"\n", 1)
			}
			if err := os.WriteFile(config, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{}
			if tc.check {
				args = append(args, "--check")
			}
			out, logs, code := runCLIWithConfig(t, config, args...)
			result.Exit = code
			result.Report = verifyOutput(t, out, logs, code)
			if code != 0 || result.Report.Status != "complete" || fixture.mutations.Load() != 0 {
				t.Fatalf("startup status changed execution: %+v", result.Report)
			}
			if tc.check && fixture.requests.Load() != 0 {
				t.Fatal("offline check accessed APIs")
			}
			seen := map[string]bool{}
			decoder := json.NewDecoder(bytes.NewReader(logs))
			for decoder.More() {
				var event map[string]any
				if err := decoder.Decode(&event); err != nil {
					t.Fatal(err)
				}
				if event["event"] != "import_reporting" {
					continue
				}
				service, _ := event["service"].(string)
				if (service != "sonarr" && service != "radarr") || seen[service] {
					t.Fatalf("unexpected or duplicate service: %v", event)
				}
				label := tc.labels[service]
				if event["level"] != "INFO" || event["enabled"] != (label != "") || event["label"] != label {
					t.Fatalf("incorrect reporting status: %v", event)
				}
				if event["setting"] != service+".imported_label" {
					t.Fatalf("missing actionable setting: %v", event)
				}
				message, _ := event["msg"].(string)
				if label == "" && !strings.Contains(message, "not configured") {
					t.Fatalf("disabled report lacks explanation: %v", event)
				}
				seen[service] = true
			}
			if len(seen) != 2 {
				t.Fatalf("expected startup reporting status for both services; got %v", seen)
			}
		})
	}
}

func TestArrImportReports(t *testing.T) {
	for _, failure := range []string{"", "radarr_history", "sonarr_history", "sonarr_episode"} {
		t.Run("failure="+failure, func(t *testing.T) {
			fixture := &apiFixture{labels: map[string][]string{
				hashFor('a'): {"movies-finished"}, hashFor('b'): {"MOVIES-FINISHED", "TV-FINISHED"},
				hashFor('d'): {"movies-finished", "tv-finished", "user:keep"},
				hashFor('e'): {"radarr-imported"}, hashFor('f'): {"tv-finished"},
			}}
			fixture.failService = failure
			result := recordScenario(t, "read-only Arr import reports "+failure, fixture)
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			config := configFile(t, server.URL, false)
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Replace(string(data), "radarr:\n", "radarr:\n  imported_label: movies-finished\n", 1)
			text = strings.Replace(text, "sonarr:\n", "sonarr:\n  imported_label: tv-finished\n", 1)
			if err := os.WriteFile(config, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			env := cliEnv()
			if failure != "" {
				env = append(env, "ARRTIFY_DRY_RUN=false")
			}
			out, logs, code := runCLIWithConfigEnv(t, config, env)
			result.Exit = code
			result.Report = verifyOutput(t, out, logs, code)
			report := result.Report
			labelsMatch := report.RadarrImportedLabel == "movies-finished" && report.SonarrImportedLabel == "tv-finished"
			if !labelsMatch || fixture.mutations.Load() != 0 {
				t.Fatalf("report=%+v mutations=%d", report, fixture.mutations.Load())
			}
			if failure != "" {
				if code != 1 || report.Status != "failed" || report.NotAttempted != 6 || report.Errors != 1 {
					t.Fatalf("source failure was not loud: %+v", report)
				}
				return
			}
			if code != 0 || report.Status != "complete" || report.WouldDelete != 2 || report.Skipped != 4 {
				t.Fatalf("import reporting changed policy: %+v", report)
			}
			for _, decision := range report.Decisions {
				assessment := decision.RadarrImport
				switch decision.Hash {
				case hashFor('a'):
					if assessment == nil || assessment.State != "replaced" {
						t.Fatalf("replacement not reported: %+v", assessment)
					}
				case hashFor('b'):
					if assessment == nil || assessment.State != "current" || decision.Reason != "arr_queue" {
						t.Fatalf("current import or queue protection lost: %+v", decision)
					}
				case hashFor('d'):
					if assessment == nil || assessment.State != "unknown" || decision.Reason != "ignored_tracker" {
						t.Fatalf("unknown import or tracker protection lost: %+v", decision)
					}
				default:
					if assessment != nil {
						t.Fatalf("unmarked torrent assessed: %+v", decision)
					}
				}
				switch decision.Hash {
				case hashFor('b'):
					if decision.SonarrImport == nil || decision.SonarrImport.State != "current" || decision.SonarrImport.ImportedEpisodes != 2 {
						t.Fatalf("mixed season pack lost a current file: %+v", decision)
					}
				case hashFor('d'):
					if decision.SonarrImport == nil || decision.SonarrImport.State != "unknown" {
						t.Fatalf("missing Sonarr history not reported: %+v", decision)
					}
				case hashFor('f'):
					if decision.SonarrImport == nil || decision.SonarrImport.State != "replaced" || decision.SonarrImport.ImportedEpisodes != 2 {
						t.Fatalf("replaced multi-episode file not reported: %+v", decision)
					}
				default:
					if decision.SonarrImport != nil {
						t.Fatalf("unmarked Sonarr torrent assessed: %+v", decision)
					}
				}
			}
		})
	}
}

func TestCLI(t *testing.T) {
	for _, tc := range []struct {
		name                                                      string
		args                                                      []string
		failService                                               string
		invalidConfig, missingEnv, invalidTorrent, invalidVersion bool
		belowThresholds                                           bool
		exit, scanned, planned, skipped, notAttempted             int
		mode, status                                              string
	}{
		{name: "dry-run protects both full queues", exit: 0, scanned: 6, planned: 2, skipped: 4, mode: "dry-run", status: "complete"},
		{name: "threshold explanation", belowThresholds: true, exit: 0, scanned: 6, planned: 1, skipped: 5, mode: "dry-run", status: "complete"},
		{name: "offline check", args: []string{"--check"}, mode: "check", status: "complete"},
		{name: "sonarr unavailable", failService: "sonarr", exit: 1, scanned: 6, notAttempted: 6, mode: "dry-run", status: "failed"},
		{name: "radarr unavailable", failService: "radarr", exit: 1, scanned: 6, notAttempted: 6, mode: "dry-run", status: "failed"},
		{name: "invalid trackers", invalidTorrent: true, exit: 1, mode: "dry-run", status: "failed"},
		{name: "incompatible version", invalidVersion: true, exit: 1, mode: "dry-run", status: "failed"},
		{name: "invalid config", invalidConfig: true, exit: 2, mode: "dry-run", status: "failed"},
		{name: "missing secrets", missingEnv: true, exit: 2, mode: "dry-run", status: "failed"},
		{name: "apply flag unsupported", args: []string{"--apply"}, exit: 2, mode: "dry-run", status: "failed"},
		{name: "unsupported apply with check", args: []string{"--check", "--apply"}, exit: 2, mode: "check", status: "failed"},
		{name: "removed sync flag rejected", args: []string{"--sync-tracker-label"}, exit: 2, mode: "dry-run", status: "failed"},
		{name: "unknown flag redacted", args: []string{"--password-secret-canary"}, exit: 2, mode: "dry-run", status: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &apiFixture{failService: tc.failService, invalidTorrent: tc.invalidTorrent, invalidVersion: tc.invalidVersion, belowThresholds: tc.belowThresholds}
			result := recordScenario(t, tc.name, fixture)
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			args := append([]string{"--config", configFile(t, server.URL, tc.invalidConfig)}, tc.args...)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Env = cliEnv()
			if tc.missingEnv {
				cmd.Env = []string{}
			}
			var out, logs bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &logs
			err := cmd.Run()
			code := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					code = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			result.Exit = code
			result.Report = verifyOutput(t, out.Bytes(), logs.Bytes(), code)
			report := result.Report
			if code != tc.exit || report.Status != tc.status || report.Mode != tc.mode || report.Scanned != tc.scanned || report.Planned != tc.planned || report.WouldDelete != tc.planned || report.Skipped != tc.skipped || report.NotAttempted != tc.notAttempted {
				t.Fatalf("exit %d, report %+v\n%s", code, report, logs.String())
			}
			if code == 2 || tc.mode == "check" {
				if fixture.requests.Load() != 0 {
					t.Fatal("configuration/check made network requests")
				}
			}
			if fixture.mutations.Load() != 0 {
				t.Fatal("dry-run sent mutation")
			}
		})
	}
}

func TestApplyStopsAfterUncertainRemoval(t *testing.T) {
	fixture := &apiFixture{
		allowTorrentRemove:  true,
		eligibleF:           true,
		uncertainRemoveHash: hashFor('e'),
	}
	report, _, logs, code := runApplyScenario(t, "apply stops after uncertain removal", fixture, false)
	if code != 1 || report.Mode != "apply" || report.DryRun || report.SyncTrackerLabel || report.Status != "partial" || report.Errors != 1 {
		t.Fatalf("exit=%d report=%+v\n%s", code, report, logs)
	}
	if report.Scanned != 6 || report.Planned != 3 || report.WouldDelete != 0 || report.Attempted != 2 || report.Skipped != 3 || report.Removed != 1 || report.Failed != 0 || report.Unknown != 1 || report.NotAttempted != 1 {
		t.Fatalf("unexpected partial-apply counters: %+v", report)
	}
	assertReportOutcomes(t, report, map[string]string{
		hashFor('a'): "removed",
		hashFor('b'): "skipped",
		hashFor('c'): "skipped",
		hashFor('d'): "skipped",
		hashFor('e'): "unknown",
		hashFor('f'): "not_attempted",
	})
	assertLoggedOutcomes(t, logs, map[string]string{
		hashFor('a'): "removed",
		hashFor('b'): "skipped",
		hashFor('c'): "skipped",
		hashFor('d'): "skipped",
		hashFor('e'): "unknown",
		hashFor('f'): "not_attempted",
	})
	assertRemovalCalls(t, fixture.rpcCallSnapshot(), []string{hashFor('a'), hashFor('e')})
	assertPartialApplyTrace(t, fixture.traceSnapshot())
	if fixture.mutations.Load() != 2 {
		t.Fatalf("got %d mutation requests, want the confirmed and uncertain remove only", fixture.mutations.Load())
	}
	fixture.rpcMu.Lock()
	deletedA, deletedE, deletedF := fixture.deletedHashes[hashFor('a')], fixture.deletedHashes[hashFor('e')], fixture.deletedHashes[hashFor('f')]
	fixture.rpcMu.Unlock()
	if !deletedA || !deletedE || deletedF {
		t.Fatalf("fixture effects after partial apply: a=%t e=%t f=%t", deletedA, deletedE, deletedF)
	}
}

func TestApplyRemovesPlannedCandidates(t *testing.T) {
	fixture := &apiFixture{allowTorrentRemove: true, eligibleF: true}
	report, _, logs, code := runApplyScenario(t, "apply removes planned candidates", fixture, false)
	if code != 0 || report.Mode != "apply" || report.DryRun || report.SyncTrackerLabel || report.Status != "complete" || report.Errors != 0 {
		t.Fatalf("exit=%d report=%+v\n%s", code, report, logs)
	}
	if report.Scanned != 6 || report.Planned != 3 || report.WouldDelete != 0 || report.Attempted != 3 || report.Skipped != 3 || report.Removed != 3 || report.Failed != 0 || report.Unknown != 0 || report.NotAttempted != 0 {
		t.Fatalf("unexpected apply counters: %+v", report)
	}
	assertRemovalCalls(t, fixture.rpcCallSnapshot(), []string{hashFor('a'), hashFor('e'), hashFor('f')})
	if fixture.mutations.Load() != 3 {
		t.Fatalf("got %d mutation requests, want three confirmed removals", fixture.mutations.Load())
	}
}

func TestApplySkipsCandidateProtectedDuringRevalidation(t *testing.T) {
	fixture := &apiFixture{
		allowTorrentRemove:        true,
		protectHashOnRevalidation: hashFor('a'),
	}
	report, out, logs, code := runApplyScenario(t, "apply skips candidate protected during revalidation", fixture, false)
	if code != 0 || report.Mode != "apply" || report.DryRun || report.Status != "complete" || report.Errors != 0 {
		t.Fatalf("exit=%d report=%+v\n%s", code, report, logs)
	}
	if report.Scanned != 6 || report.Planned != 2 || report.WouldDelete != 0 || report.Attempted != 1 || report.Skipped != 5 || report.Removed != 1 || report.Failed != 0 || report.Unknown != 0 || report.NotAttempted != 0 {
		t.Fatalf("unexpected revalidation counters: %+v", report)
	}
	payload := decodeReportPayload(t, out)
	if decision := torrentDecision(t, payload, hashFor('a')); decision["reason"] != "arr_queue" || decision["outcome"] != "skipped" {
		t.Fatalf("candidate protected by a refreshed queue: %v", decision)
	}
	if decision := torrentDecision(t, payload, hashFor('e')); decision["outcome"] != "removed" {
		t.Fatalf("remaining candidate was not removed: %v", decision)
	}
	assertRemovalCalls(t, fixture.rpcCallSnapshot(), []string{hashFor('e')})
	if fixture.mutations.Load() != 1 {
		t.Fatalf("got %d mutation requests, want only the unprotected candidate", fixture.mutations.Load())
	}
}

func TestApplyInitialArrFailurePreventsRemoval(t *testing.T) {
	fixture := &apiFixture{allowTorrentRemove: true, eligibleF: true, failService: "sonarr"}
	report, _, logs, code := runApplyScenario(t, "apply initial Arr failure prevents removal", fixture, false)
	if code != 1 || report.Mode != "apply" || report.DryRun || report.Status != "failed" || report.Errors != 1 || report.Scanned != 6 || report.Planned != 0 || report.Attempted != 0 || report.WouldDelete != 0 || report.Skipped != 0 || report.Removed != 0 || report.Failed != 0 || report.Unknown != 0 || report.NotAttempted != 6 {
		t.Fatalf("exit=%d report=%+v\n%s", code, report, logs)
	}
	assertRemovalCalls(t, fixture.rpcCallSnapshot(), []string{})
	if fixture.mutations.Load() != 0 {
		t.Fatalf("initial Arr failure allowed removals: mutations=%d calls=%v", fixture.mutations.Load(), fixture.rpcCallSnapshot())
	}
}

func TestLabelReadbackFailureBlocksCleanup(t *testing.T) {
	fixture := &apiFixture{
		syncTrackerLabels:  true,
		failLabelReadback:  true,
		allowTorrentRemove: true,
	}
	report, out, logs, code := runApplyScenario(t, "label readback failure blocks cleanup", fixture, true)
	if code != 1 || report.Mode != "apply" || report.DryRun || !report.SyncTrackerLabel || report.Status != "partial" || report.Errors != 1 {
		t.Fatalf("exit=%d report=%+v\n%s", code, report, logs)
	}
	if report.Scanned != 6 || report.Planned != 2 || report.Attempted != 0 || report.Skipped != 4 || report.WouldDelete != 0 || report.Removed != 0 || report.Failed != 0 || report.Unknown != 0 || report.NotAttempted != 2 || report.TrackerLabelsPlanned != 1 || report.TrackerLabelsChanged != 0 {
		t.Fatalf("label failure did not block planned cleanup: %+v", report)
	}
	assertTrackerLabelDecision(t, trackerLabelDecision(t, decodeReportPayload(t, out), hashFor('d')), true, "add", "unknown")
	if !bytes.Contains(logs, []byte(`"event":"tracker_label_failed"`)) || bytes.Contains(logs, []byte(`"event":"delete_requested"`)) {
		t.Fatalf("logs do not show label failure blocking removals:\n%s", logs)
	}
	assertTorrentLabelMutations(t, fixture.rpcCallSnapshot(), map[string][]string{hashFor('d'): {"arrtify:ignored-tracker"}})
	assertRemovalCalls(t, fixture.rpcCallSnapshot(), []string{})
	if fixture.mutations.Load() != 1 {
		t.Fatalf("got %d mutation requests, want only the failed-confirmation label write", fixture.mutations.Load())
	}
}

func TestLabelOutcomeSurvivesUncertainCleanup(t *testing.T) {
	const managedLabel = "arrtify:ignored-tracker"
	hashA := hashFor('a')
	fixture := &apiFixture{
		syncTrackerLabels:   true,
		allowTorrentRemove:  true,
		uncertainRemoveHash: hashA,
		labels: map[string][]string{
			hashA:        {"user:keep", managedLabel},
			hashFor('d'): {managedLabel},
		},
	}
	report, out, logs, code := runApplyScenario(t, "label outcome survives uncertain cleanup", fixture, true)
	if code != 1 || report.Mode != "apply" || report.Status != "partial" || report.Errors != 1 || !report.SyncTrackerLabel {
		t.Fatalf("exit=%d report=%+v\n%s", code, report, logs)
	}
	if report.Planned != 2 || report.Attempted != 1 || report.Skipped != 4 || report.Removed != 0 || report.Unknown != 1 || report.NotAttempted != 1 || report.TrackerLabelsChanged != 1 {
		t.Fatalf("unexpected cross-phase counters: %+v", report)
	}
	decision := torrentDecision(t, decodeReportPayload(t, out), hashA)
	if decision["outcome"] != "unknown" {
		t.Fatalf("uncertain cleanup outcome: %v", decision)
	}
	assertTrackerLabelDecision(t, decision["tracker_label"].(map[string]any), false, "remove", "removed")
	if torrentDecision(t, decodeReportPayload(t, out), hashFor('e'))["outcome"] != "not_attempted" {
		t.Fatal("remaining cleanup candidate was not marked not_attempted")
	}
	fixture.rpcMu.Lock()
	remainingLabels := append([]string{}, fixture.labels[hashA]...)
	fixture.rpcMu.Unlock()
	if !sameStrings(remainingLabels, []string{"user:keep"}) {
		t.Fatalf("managed-label removal lost or changed the foreign label: %v", remainingLabels)
	}
	assertRemovalCalls(t, fixture.rpcCallSnapshot(), []string{hashA})
	if fixture.mutations.Load() != 2 {
		t.Fatalf("got %d mutations, want one label write and one uncertain remove", fixture.mutations.Load())
	}
}

func assertReportOutcomes(t *testing.T, report cleaner.RunReport, want map[string]string) {
	t.Helper()
	got := make(map[string]string, len(report.Decisions))
	for _, decision := range report.Decisions {
		got[decision.Hash] = decision.Outcome
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("report outcomes: got %v, want %v", got, want)
	}
}

func assertLoggedOutcomes(t *testing.T, logs []byte, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(logs)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid structured log: %v\n%s", err, logs)
		}
		if event["event"] == "torrent_decision" {
			hash, _ := event["torrent_hash"].(string)
			outcome, _ := event["outcome"].(string)
			got[hash] = outcome
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("logged outcomes: got %v, want %v\n%s", got, want, logs)
	}
}

func assertRemovalCalls(t *testing.T, calls []rpcCall, wantHashes []string) {
	t.Helper()
	got := removalHashes(t, calls)
	if !reflect.DeepEqual(got, wantHashes) {
		t.Fatalf("torrent_remove requests: got %v, want %v", got, wantHashes)
	}
}

func removalHashes(t *testing.T, calls []rpcCall) []string {
	t.Helper()
	got := []string{}
	for _, call := range calls {
		if call.Method != "torrent_remove" {
			continue
		}
		ids := stringArgument(t, call.Params, "ids")
		if len(ids) != 1 || len(ids[0]) != 40 || ids[0] != strings.ToLower(ids[0]) {
			t.Fatalf("torrent_remove ids must contain one canonical hash: %v", ids)
		}
		var deleteLocalData bool
		if err := json.Unmarshal(call.Params["delete_local_data"], &deleteLocalData); err != nil || !deleteLocalData {
			t.Fatalf("torrent_remove for %s must set delete_local_data=true", ids[0])
		}
		got = append(got, ids[0])
	}
	return got
}

func assertPartialApplyTrace(t *testing.T, trace []string) {
	t.Helper()
	plan := traceIndex(trace, 0, "rpc:torrent_get:*")
	firstRefresh := traceIndex(trace, plan+1, "rpc:torrent_get:"+hashFor('a'))
	firstRemove := traceIndex(trace, firstRefresh+1, "rpc:torrent_remove:"+hashFor('a'))
	firstReadback := traceIndex(trace, firstRemove+1, "rpc:torrent_get:"+hashFor('a'))
	secondRefresh := traceIndex(trace, firstReadback+1, "rpc:torrent_get:"+hashFor('e'))
	secondRemove := traceIndex(trace, secondRefresh+1, "rpc:torrent_remove:"+hashFor('e'))
	if plan < 0 || firstRefresh < 0 || firstRemove < 0 || firstReadback < 0 || secondRefresh < 0 || secondRemove < 0 {
		t.Fatalf("partial apply did not plan, confirm the first remove, and stop after the uncertain second: %v", trace)
	}
	if !hasCompleteQueueRead(trace[plan+1:firstRefresh]) || !hasCompleteQueueRead(trace[firstRefresh+1:firstRemove]) || !hasCompleteQueueRead(trace[secondRefresh+1:secondRemove]) {
		t.Fatalf("initial plan or per-candidate Arr revalidation was incomplete: %v", trace)
	}
}

func traceIndex(trace []string, start int, want string) int {
	for i := max(start, 0); i < len(trace); i++ {
		if trace[i] == want {
			return i
		}
	}
	return -1
}

func hasCompleteQueueRead(trace []string) bool {
	seen := map[string]bool{}
	for _, event := range trace {
		seen[event] = true
	}
	return seen["arr:sonarr:1"] && seen["arr:sonarr:2"] && seen["arr:radarr:1"]
}

func TestTrackerLabelDryRun(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit false", true: "omitted"}[omitted], func(t *testing.T) {
			fixture := &apiFixture{}
			result := recordScenario(t, "tracker label dry-run plans without writes", fixture)
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()

			config := configFile(t, server.URL, false)
			if omitted {
				data, err := os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config, []byte(strings.Replace(string(data), "sync_tracker_label: false\n", "", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, logs, code := runCLIWithConfig(t, config)
			result.Exit = code
			result.Report = verifyOutput(t, out, logs, code)
			payload := decodeReportPayload(t, out)
			if code != 0 || result.Report.Mode != "dry-run" || result.Report.Status != "complete" {
				t.Fatalf("exit %d, report %+v\n%s", code, result.Report, logs)
			}
			if result.Report.Scanned != 6 || result.Report.WouldDelete != 2 || result.Report.Removed != 0 || result.Report.Attempted != 0 {
				t.Fatalf("dry-run did not retain cleanup reporting: %+v", result.Report)
			}
			if reportCount(t, payload, "tracker_labels_planned") != 1 || reportCount(t, payload, "tracker_labels_changed") != 0 {
				t.Fatalf("dry-run tracker label counters: %v", payload)
			}
			decision := trackerLabelDecision(t, payload, hashFor('d'))
			assertTrackerLabelDecision(t, decision, true, "add", "would_add")
			if fixture.mutations.Load() != 0 {
				t.Fatalf("dry-run sent %d mutations", fixture.mutations.Load())
			}

		})
	}
}

func TestGlobalDryRunOverridesTrackerLabelSync(t *testing.T) {
	for _, value := range []string{"", "true"} {
		t.Run("ARRTIFY_DRY_RUN="+value, func(t *testing.T) {
			fixture := &apiFixture{syncTrackerLabels: true, labels: map[string][]string{}}
			result := recordScenario(t, "global dry-run suppresses enabled label sync", fixture)
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			config := configFile(t, server.URL, false)
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			s := strings.Replace(string(data), "sync_tracker_label: false", "sync_tracker_label: true", 1)
			if err := os.WriteFile(config, []byte(s), 0600); err != nil {
				t.Fatal(err)
			}
			var env []string
			if value != "" {
				env = []string{"ARRTIFY_DRY_RUN=" + value}
			}
			out, logs, code := runCLIWithConfigEnv(t, config, env)
			result.Exit = code
			result.Report = verifyOutput(t, out, logs, code)
			if code != 0 || result.Report.Mode != "dry-run" || result.Report.Status != "complete" || result.Report.TrackerLabelsPlanned != 1 || result.Report.TrackerLabelsChanged != 0 || !result.Report.DryRun || !result.Report.SyncTrackerLabel || fixture.mutations.Load() != 0 {
				t.Fatalf("exit=%d, mutations=%d, report=%+v\n%s", code, fixture.mutations.Load(), result.Report, logs)
			}
			assertTrackerLabelDecision(t, trackerLabelDecision(t, decodeReportPayload(t, out), hashFor('d')), true, "add", "would_add")
		})
	}
}

func TestTrackerLabelSync(t *testing.T) {
	const managedLabel = "arrtify:ignored-tracker"
	hashA, hashB, hashC, hashD, hashF := hashFor('a'), hashFor('b'), hashFor('c'), hashFor('d'), hashFor('f')
	fixture := &apiFixture{
		syncTrackerLabels:    true,
		ignoredTrackerHashes: map[string]bool{hashB: true, hashC: true, hashF: true},
		labels: map[string][]string{
			hashA: {managedLabel, "user:keep-a"},
			hashB: {managedLabel, "user:keep-b"},
			hashC: {"user:keep-c"},
			hashD: {"user:keep-d"},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	config := configFileWithoutCleanupCandidates(t, server.URL)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(strings.Replace(string(data), "sync_tracker_label: false", "sync_tracker_label: true", 1)), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("adds and removes only the managed label", func(t *testing.T) {
		result := recordScenario(t, "tracker label sync adds and removes managed tag", fixture)
		out, logs, code := runCLIWithConfigEnv(t, config, []string{"ARRTIFY_DRY_RUN=false"})
		result.Exit = code
		result.Report = verifyOutput(t, out, logs, code)
		payload := decodeReportPayload(t, out)
		if code != 0 || result.Report.Mode != "apply" || result.Report.Status != "complete" {
			t.Fatalf("exit %d, report %+v\n%s", code, result.Report, logs)
		}
		if reportCount(t, payload, "tracker_labels_planned") != 4 || reportCount(t, payload, "tracker_labels_changed") != 4 {
			t.Fatalf("sync tracker label counters: %v", payload)
		}
		if result.Report.Scanned != 6 || result.Report.WouldDelete != 0 || result.Report.Removed != 0 || result.Report.Attempted != 0 {
			t.Fatalf("label sync changed cleanup counters: %+v", result.Report)
		}
		assertTrackerLabelDecision(t, trackerLabelDecision(t, payload, hashA), false, "remove", "removed")
		assertTrackerLabelDecision(t, trackerLabelDecision(t, payload, hashB), true, "none", "unchanged")
		assertTrackerLabelDecision(t, trackerLabelDecision(t, payload, hashC), true, "add", "added")
		assertTrackerLabelDecision(t, trackerLabelDecision(t, payload, hashD), true, "add", "added")
		assertTrackerLabelDecision(t, trackerLabelDecision(t, payload, hashFor('e')), false, "none", "unchanged")
		assertTrackerLabelDecision(t, trackerLabelDecision(t, payload, hashF), true, "add", "added")
		if decision := torrentDecision(t, payload, hashC); decision["reason"] != "arr_queue" {
			t.Fatalf("expected queue-protected torrent to retain a tracker-label plan, got %v", decision)
		}
		if decision := torrentDecision(t, payload, hashF); decision["reason"] == "rule_satisfied" {
			t.Fatalf("expected ineligible torrent to retain its independent tracker-label decision, got %v", decision)
		}

		calls := fixture.rpcCallSnapshot()
		assertTorrentLabelMutations(t, calls, map[string][]string{
			hashA: {"user:keep-a"},
			hashC: {"user:keep-c", managedLabel},
			hashD: {"user:keep-d", managedLabel},
			hashF: {managedLabel},
		})
		if fixture.mutations.Load() != 4 {
			t.Fatalf("got %d torrent_set calls, want 4", fixture.mutations.Load())
		}
	})

	t.Run("second run performs no unnecessary mutation", func(t *testing.T) {
		result := recordScenario(t, "tracker label sync second run is unchanged", fixture)
		callsBefore := len(fixture.rpcCallSnapshot())
		out, logs, code := runCLIWithConfigEnv(t, config, []string{"ARRTIFY_DRY_RUN=false"})
		result.Exit = code
		result.Report = verifyOutput(t, out, logs, code)
		payload := decodeReportPayload(t, out)
		if code != 0 || result.Report.Mode != "apply" || result.Report.Status != "complete" {
			t.Fatalf("exit %d, report %+v\n%s", code, result.Report, logs)
		}
		if reportCount(t, payload, "tracker_labels_planned") != 0 || reportCount(t, payload, "tracker_labels_changed") != 0 {
			t.Fatalf("idempotent sync tracker label counters: %v", payload)
		}
		calls := fixture.rpcCallSnapshot()
		for _, call := range calls[callsBefore:] {
			if call.Method == "torrent_set" {
				t.Fatal("second sync sent an unnecessary torrent_set")
			}
		}
		if result.Report.Removed != 0 || result.Report.Attempted != 0 || fixture.mutations.Load() != 4 {
			t.Fatalf("second sync changed cleanup or mutation counters: report=%+v mutations=%d", result.Report, fixture.mutations.Load())
		}
	})
}

func TestTrackerLabelCheckDoesNotAccessServices(t *testing.T) {
	fixture := &apiFixture{}
	result := recordScenario(t, "check validates enabled label sync without network", fixture)
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	config := configFile(t, server.URL, false)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(strings.Replace(string(data), "sync_tracker_label: false", "sync_tracker_label: true", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	out, logs, code := runCLIWithConfigEnv(t, config, []string{"ARRTIFY_DRY_RUN=false"}, "--check")
	result.Exit = code
	result.Report = verifyOutput(t, out, logs, code)
	if code != 0 || result.Report.Mode != "check" || result.Report.Status != "complete" || fixture.requests.Load() != 0 || fixture.mutations.Load() != 0 {
		t.Fatalf("exit %d, requests=%d mutations=%d report=%+v\n%s", code, fixture.requests.Load(), fixture.mutations.Load(), result.Report, logs)
	}
}

func TestInvalidDryRunEnvironmentDoesNotAccessServices(t *testing.T) {
	for _, value := range []string{"", "FALSE", "1", "config-secret-canary"} {
		t.Run("value="+value, func(t *testing.T) {
			fixture := &apiFixture{}
			result := recordScenario(t, "invalid dry-run environment rejected before network", fixture)
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			config := configFile(t, server.URL, false)
			out, logs, code := runCLIWithConfigEnv(t, config, []string{"ARRTIFY_DRY_RUN=" + value})
			result.Exit = code
			result.Report = verifyOutput(t, out, logs, code)
			if code != 2 || result.Report.Status != "failed" || fixture.requests.Load() != 0 || fixture.mutations.Load() != 0 {
				t.Fatalf("exit=%d requests=%d mutations=%d report=%+v", code, fixture.requests.Load(), fixture.mutations.Load(), result.Report)
			}
		})
	}
}

func TestYAMLDryRunRejectedEvenWithEnvironment(t *testing.T) {
	fixture := &apiFixture{}
	result := recordScenario(t, "YAML cannot configure dry-run even with environment set", fixture)
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	config := configFile(t, server.URL, false)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(data, []byte("\ndry_run: false\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	out, logs, code := runCLIWithConfigEnv(t, config, []string{"ARRTIFY_DRY_RUN=true"})
	result.Exit = code
	result.Report = verifyOutput(t, out, logs, code)
	if code != 2 || result.Report.Status != "failed" || fixture.requests.Load() != 0 || fixture.mutations.Load() != 0 {
		t.Fatalf("exit=%d requests=%d mutations=%d report=%+v", code, fixture.requests.Load(), fixture.mutations.Load(), result.Report)
	}
}

func runCLI(t *testing.T, url string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	return runCLIWithConfig(t, configFile(t, url, false), args...)
}

func runCLIWithConfig(t *testing.T, config string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	return runCLIWithConfigEnv(t, config, nil, args...)
}

func runCLIWithConfigEnv(t *testing.T, config string, env []string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	commandArgs := append([]string{"--config", config}, args...)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, commandArgs...)
	cmd.Env = append(cliEnv(), env...)
	var out, logs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &logs
	code := 0
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return out.Bytes(), logs.Bytes(), code
}

func runApplyScenario(t *testing.T, name string, fixture *apiFixture, syncLabels bool) (cleaner.RunReport, []byte, []byte, int) {
	t.Helper()
	result := recordScenario(t, name, fixture)
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	config := configFile(t, server.URL, false)
	if syncLabels {
		data, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, []byte(strings.Replace(string(data), "sync_tracker_label: false", "sync_tracker_label: true", 1)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, logs, code := runCLIWithConfigEnv(t, config, []string{"ARRTIFY_DRY_RUN=false"})
	result.Exit = code
	result.Report = verifyOutput(t, out, logs, code)
	return result.Report, out, logs, code
}

func decodeReportPayload(t *testing.T, output []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(output, &payload); err != nil {
		t.Fatalf("invalid report payload: %v\n%s", err, output)
	}
	return payload
}

func reportCount(t *testing.T, payload map[string]any, key string) int {
	t.Helper()
	value, ok := payload[key].(float64)
	if !ok {
		t.Fatalf("report field %s missing or not numeric: %v", key, payload[key])
	}
	return int(value)
}

func trackerLabelDecision(t *testing.T, payload map[string]any, hash string) map[string]any {
	t.Helper()
	decision, ok := trackerLabelDecisionMaybe(payload, hash)
	if !ok {
		t.Fatalf("missing tracker label decision for torrent %s", hash)
	}
	return decision
}

func trackerLabelDecisionMaybe(payload map[string]any, hash string) (map[string]any, bool) {
	decision, ok := torrentDecisionMaybe(payload, hash)
	if !ok {
		return nil, false
	}
	trackerLabel, ok := decision["tracker_label"].(map[string]any)
	return trackerLabel, ok
}

func torrentDecision(t *testing.T, payload map[string]any, hash string) map[string]any {
	t.Helper()
	decision, ok := torrentDecisionMaybe(payload, hash)
	if !ok {
		t.Fatalf("missing cleanup decision for torrent %s", hash)
	}
	return decision
}

func torrentDecisionMaybe(payload map[string]any, hash string) (map[string]any, bool) {
	decisions, ok := payload["decisions"].([]any)
	if !ok {
		return nil, false
	}
	for _, item := range decisions {
		decision, ok := item.(map[string]any)
		if !ok || decision["torrent_hash"] != hash {
			continue
		}
		return decision, true
	}
	return nil, false
}

func assertTrackerLabelDecision(t *testing.T, decision map[string]any, ignored bool, action, outcome string) {
	t.Helper()
	if decision["ignored"] != ignored || decision["action"] != action || decision["outcome"] != outcome {
		t.Fatalf("tracker label decision: got %v, want ignored=%t action=%q outcome=%q", decision, ignored, action, outcome)
	}
}

func assertTorrentLabelMutations(t *testing.T, calls []rpcCall, want map[string][]string) {
	t.Helper()
	fullFields := []string{
		"hash_string", "name", "download_dir", "upload_ratio", "seconds_seeding", "status",
		"is_finished", "left_until_done", "metadata_percent_complete", "trackers", "labels",
	}
	seen := map[string]bool{}
	for i, call := range calls {
		if call.Method != "torrent_set" {
			continue
		}
		ids := stringArgument(t, call.Params, "ids")
		if len(ids) != 1 || len(ids[0]) != 40 || ids[0] != strings.ToLower(ids[0]) {
			t.Fatalf("torrent_set ids must contain one canonical hash: %v", ids)
		}
		hash := ids[0]
		wantLabels, ok := want[hash]
		if !ok || seen[hash] {
			t.Fatalf("unexpected or duplicate torrent_set target %q", hash)
		}
		seen[hash] = true
		gotLabels := stringArgument(t, call.Params, "labels")
		if !sameStrings(gotLabels, wantLabels) {
			t.Fatalf("torrent_set labels for %s: got %v, want full desired labels %v", hash, gotLabels, wantLabels)
		}
		if i == 0 || calls[i-1].Method != "torrent_get" || !targetedGet(t, calls[i-1], hash, fullFields) {
			t.Fatalf("torrent_set for %s was not preceded by a fresh full torrent_get", hash)
		}
		readBack := false
		for j := i + 1; j < len(calls); j++ {
			if calls[j].Method == "torrent_set" {
				break
			}
			if calls[j].Method == "torrent_get" && targetedGet(t, calls[j], hash, fullFields) {
				readBack = true
				break
			}
		}
		if !readBack {
			t.Fatalf("torrent_set for %s was not followed by a full readback", hash)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("torrent_set targets: got %v, want %v", seen, want)
	}
}

func targetedGet(t *testing.T, call rpcCall, hash string, requiredFields []string) bool {
	t.Helper()
	ids := stringArgument(t, call.Params, "ids")
	if len(ids) != 1 || ids[0] != hash {
		return false
	}
	fields := stringArgument(t, call.Params, "fields")
	for _, required := range requiredFields {
		if !containsString(fields, required) {
			return false
		}
	}
	return true
}

func stringArgument(t *testing.T, params map[string]json.RawMessage, key string) []string {
	t.Helper()
	var values []string
	if err := json.Unmarshal(params[key], &values); err != nil {
		t.Fatalf("invalid %s argument: %v", key, err)
	}
	return values
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aCopy, bCopy := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(aCopy)
	sort.Strings(bCopy)
	return reflect.DeepEqual(aCopy, bCopy)
}

func hashFor(c byte) string {
	return strings.Repeat(string(c), 40)
}

func TestConfigFileSelection(t *testing.T) {
	for _, tc := range []struct {
		name, env              string
		setEnv, invalidDefault bool
		args                   []string
		exit, scanned          int
	}{
		{name: "default file", args: []string{"--check"}},
		{name: "environment file", setEnv: true, env: "custom config.yml", invalidDefault: true, args: []string{"--check"}},
		{name: "environment dry-run", setEnv: true, env: "custom config.yml", invalidDefault: true, scanned: 6},
		{name: "missing environment file", setEnv: true, env: "path-secret-canary.yml", args: []string{"--check"}, exit: 2},
		{name: "invalid environment file", setEnv: true, env: "invalid.yml", args: []string{"--check"}, exit: 2},
		{name: "empty environment path", setEnv: true, args: []string{"--check"}, exit: 2},
		{name: "flag overrides environment", setEnv: true, env: "invalid.yml", args: []string{"--config", "config.yml", "--check"}},
		{name: "flag overrides empty environment", setEnv: true, args: []string{"--config", "config.yml", "--check"}},
		{name: "explicit empty flag is invalid", setEnv: true, env: "custom config.yml", args: []string{"--config", "", "--check"}, exit: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &apiFixture{}
			result := recordScenario(t, tc.name, fixture)
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			dir := t.TempDir()
			for _, file := range []struct {
				name    string
				invalid bool
			}{
				{name: "config.yml", invalid: tc.invalidDefault},
				{name: "custom config.yml"},
				{name: "invalid.yml", invalid: true},
			} {
				data, err := os.ReadFile(configFile(t, server.URL, file.invalid))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, file.name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, tc.args...)
			cmd.Dir = dir
			cmd.Env = cliEnv()
			if tc.setEnv {
				path := tc.env
				if path != "" {
					path = filepath.Join(dir, path)
				}
				cmd.Env = append(cmd.Env, "ARRTIFY_CONFIG_FILE="+path)
			}
			var out, logs bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &logs
			code := 0
			if err := cmd.Run(); err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			result.Exit = code
			result.Report = verifyOutput(t, out.Bytes(), logs.Bytes(), code)
			if code != tc.exit || result.Report.Scanned != tc.scanned {
				t.Fatalf("exit %d, report %+v", code, result.Report)
			}
			if bytes.Contains(out.Bytes(), []byte("path-secret-canary")) || bytes.Contains(logs.Bytes(), []byte("path-secret-canary")) {
				t.Fatal("configuration path leaked")
			}
			if tc.scanned == 0 && fixture.requests.Load() != 0 {
				t.Fatal("configuration check made network requests")
			}
			if tc.scanned != 0 && (result.Report.WouldDelete != 2 || fixture.requests.Load() == 0) {
				t.Fatal("environment-selected dry-run did not evaluate the synthetic APIs")
			}
			if fixture.mutations.Load() != 0 {
				t.Fatal("configuration selection sent mutation")
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	fixture := &apiFixture{started: make(chan struct{})}
	result := recordScenario(t, "SIGTERM during read", fixture)
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--config", configFile(t, server.URL, false))
	cmd.Env = cliEnv()
	var out, logs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.started:
	case <-ctx.Done():
		_ = cmd.Wait()
		t.Fatal("no request before deadline")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	exit, ok := err.(*exec.ExitError)
	if ok {
		result.Exit = exit.ExitCode()
	} else if err == nil {
		result.Exit = 0
	}
	if !ok || result.Exit != 1 {
		t.Fatalf("exit: %v", err)
	}
	result.Report = verifyOutput(t, out.Bytes(), logs.Bytes(), result.Exit)
	report := result.Report
	if report.Status != "failed" || report.Errors != 1 || fixture.mutations.Load() != 0 {
		t.Fatalf("report: %+v", report)
	}
}
