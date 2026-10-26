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
	ignoredTrackerHashes           map[string]bool
	labels                         map[string][]string
	rpcMu                          sync.Mutex
	rpcCalls                       []rpcCall
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
		if req.Method != "session_get" && req.Method != "torrent_get" && !(f.syncTrackerLabels && req.Method == "torrent_set") {
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
			torrents, err := f.torrents(req.Params)
			if err != nil {
				http.Error(w, "invalid torrent_get arguments", 400)
				return
			}
			result = map[string]any{"torrents": torrents}
		} else {
			if err := f.setLabels(req.Params); err != nil {
				http.Error(w, "invalid torrent_set arguments", 400)
				return
			}
			f.mutations.Add(1)
			result = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		return
	}
	service := "radarr"
	if strings.HasPrefix(r.URL.Path, "/sonarr/") {
		service = "sonarr"
	}
	if f.failService == service {
		http.Error(w, "api-secret-canary", 403)
		return
	}
	if r.Method != "GET" || r.Header.Get("X-Api-Key") != "api-secret-canary" || !strings.HasSuffix(r.URL.Path, "/api/v3/queue") {
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
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	records := []any{}
	total := 1
	if service == "sonarr" {
		total = 101
		if page == 1 {
			for i := 1; i <= 100; i++ {
				var hash any
				if i%2 == 0 {
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
	var torrents []map[string]any
	for _, c := range []byte{'a', 'b', 'c', 'd', 'e', 'f'} {
		hash := strings.Repeat(string(c), 40)
		if len(ids) > 0 && !containsString(ids, hash) {
			continue
		}
		trackerList := []any{}
		if c == 'd' || f.ignoredTrackerHashes[hash] {
			trackerList = []any{map[string]any{"announce": "udp://announce.tracker.example.invalid:80/path?passkey=tracker-secret-canary"}}
		}
		labels := f.currentLabels(hash)
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
			tr["status"] = 0
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
	f.labels[ids[0]] = append([]string{}, labels...)
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
	if len(report.Decisions) != report.Scanned || report.Attempted != 0 || report.Removed != 0 || report.Unknown != 0 {
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
			for _, key := range []string{"tracker_labels_planned", "tracker_labels_changed"} {
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
			for _, key := range []string{"upload_ratio", "upload_ratio_kind", "seed_time_seconds", "seed_time", "rule", "criteria"} {
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
		{name: "apply rejected", args: []string{"--apply"}, exit: 2, mode: "apply", status: "failed"},
		{name: "conflicting flags", args: []string{"--check", "--apply"}, exit: 2, mode: "apply", status: "failed"},
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
	config := configFile(t, server.URL, false)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(strings.Replace(string(data), "sync_tracker_label: false", "sync_tracker_label: true", 1)), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("adds and removes only the managed label", func(t *testing.T) {
		result := recordScenario(t, "tracker label sync adds and removes managed tag", fixture)
		out, logs, code := runCLIWithConfig(t, config)
		result.Exit = code
		result.Report = verifyOutput(t, out, logs, code)
		payload := decodeReportPayload(t, out)
		if code != 0 || result.Report.Mode != "label-sync" || result.Report.Status != "complete" {
			t.Fatalf("exit %d, report %+v\n%s", code, result.Report, logs)
		}
		if reportCount(t, payload, "tracker_labels_planned") != 4 || reportCount(t, payload, "tracker_labels_changed") != 4 {
			t.Fatalf("sync tracker label counters: %v", payload)
		}
		if result.Report.Scanned != 6 || result.Report.WouldDelete != 2 || result.Report.Removed != 0 || result.Report.Attempted != 0 {
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
		out, logs, code := runCLIWithConfig(t, config)
		result.Exit = code
		result.Report = verifyOutput(t, out, logs, code)
		payload := decodeReportPayload(t, out)
		if code != 0 || result.Report.Mode != "label-sync" || result.Report.Status != "complete" {
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
	out, logs, code := runCLIWithConfig(t, config, "--check")
	result.Exit = code
	result.Report = verifyOutput(t, out, logs, code)
	if code != 0 || result.Report.Mode != "check" || result.Report.Status != "complete" || fixture.requests.Load() != 0 || fixture.mutations.Load() != 0 {
		t.Fatalf("exit %d, requests=%d mutations=%d report=%+v\n%s", code, fixture.requests.Load(), fixture.mutations.Load(), result.Report, logs)
	}
}

func runCLI(t *testing.T, url string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	return runCLIWithConfig(t, configFile(t, url, false), args...)
}

func runCLIWithConfig(t *testing.T, config string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	commandArgs := append([]string{"--config", config}, args...)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, commandArgs...)
	cmd.Env = cliEnv()
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
