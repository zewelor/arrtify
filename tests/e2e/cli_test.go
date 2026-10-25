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
	"strconv"
	"strings"
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
	Name      string            `json:"name"`
	Passed    bool              `json:"passed"`
	Exit      int               `json:"exit"`
	Requests  int64             `json:"requests"`
	Mutations int64             `json:"mutations"`
	Report    cleaner.RunReport `json:"report"`
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
}

func recordScenario(t *testing.T, name string, fixture *apiFixture) *scenarioResult {
	t.Helper()
	result := &scenarioResult{Name: name, Exit: -1}
	t.Cleanup(func() {
		result.Passed = !t.Failed()
		result.Requests = fixture.requests.Load()
		result.Mutations = fixture.mutations.Load()
		evidence = append(evidence, *result)
	})
	return result
}

func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if r.Method == http.MethodPost {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if req.Method != "session_get" && req.Method != "torrent_get" {
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
		var result any
		if req.Method == "session_get" {
			version := "4.1.3"
			if f.invalidVersion {
				version = "4.2.0"
			}
			result = map[string]any{"version": version, "rpc_version_semver": "6.0.1"}
		} else {
			torrents := []map[string]any{}
			for _, c := range []byte{'a', 'b', 'c', 'd', 'e', 'f'} {
				tr := map[string]any{"hash_string": strings.Repeat(string(c), 40), "name": "Synthetic " + string(c), "download_dir": "/downloads/sonarr", "upload_ratio": 2, "seconds_seeding": 24 * 3600, "status": 6, "is_finished": false, "left_until_done": 0, "metadata_percent_complete": 1, "trackers": []any{}}
				switch c {
				case 'd':
					tr["trackers"] = []any{map[string]any{"announce": "udp://announce.tracker.example.invalid:80/path?passkey=tracker-secret-canary"}}
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
			result = map[string]any{"torrents": torrents}
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
