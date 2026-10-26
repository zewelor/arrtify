package cleaner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func apiFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func apiConfig(serverURL string) Config {
	return Config{
		Transmission: Endpoint{URL: serverURL + "/transmission/rpc"},
		Credentials:  Credentials{TransmissionUser: "synthetic-user", TransmissionPassword: "synthetic-password"},
	}
}

func TestTransmissionReadHandshakeFieldsAndCanonicalOrder(t *testing.T) {
	session := apiFixture(t, "transmission-4.1.3-session.json")
	torrents := apiFixture(t, "transmission-4.1.3-torrents.json")
	var bodies [][]byte
	var methods []string
	var sessionRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transmission/rpc" {
			t.Errorf("path = %q", r.URL.Path)
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "synthetic-user" || password != "synthetic-password" {
			t.Errorf("unexpected basic auth")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		bodies = append(bodies, body)
		var request struct {
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("invalid JSON-RPC request")
			return
		}
		methods = append(methods, request.Method)
		switch request.Method {
		case "session_get":
			sessionRequests++
			var fields []string
			if err := json.Unmarshal(request.Params["fields"], &fields); err != nil || !reflect.DeepEqual(fields, []string{"version", "rpc_version_semver"}) {
				t.Errorf("session_get fields = %v, err %v", fields, err)
			}
			if sessionRequests == 1 {
				w.Header().Set("X-Transmission-Session-Id", "synthetic-session-id")
				w.Header().Set("X-Transmission-Rpc-Version", "6.0.1")
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(session)
		case "torrent_get":
			var fields []string
			want := []string{"hash_string", "name", "download_dir", "upload_ratio", "seconds_seeding", "status", "is_finished", "left_until_done", "metadata_percent_complete", "trackers", "labels"}
			if err := json.Unmarshal(request.Params["fields"], &fields); err != nil || !reflect.DeepEqual(fields, want) {
				t.Errorf("torrent_get fields = %v, err %v", fields, err)
			}
			if _, ok := request.Params["ids"]; ok {
				t.Errorf("unexpected ids filter")
			}
			if r.Header.Get("X-Transmission-Session-Id") != "synthetic-session-id" {
				t.Errorf("session id not reused")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(torrents)
		default:
			t.Errorf("unexpected RPC method %q", request.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	c := apiConfig(server.URL)
	items, err := ReadTransmission(context.Background(), c)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(items) != 2 || items[0].Hash != strings.Repeat("a", 40) || items[1].Hash != strings.Repeat("b", 40) {
		t.Fatalf("torrents not canonicalized and sorted: %+v", items)
	}
	if items[0].Trackers == nil || len(items[0].Trackers) != 0 || len(items[1].Trackers) != 1 {
		t.Fatalf("trackers not parsed: %+v", items)
	}
	if !reflect.DeepEqual(methods, []string{"session_get", "session_get", "torrent_get"}) {
		t.Fatalf("RPC methods = %v", methods)
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("session handshake did not resend identical body")
	}
	for _, method := range methods {
		if method != "session_get" && method != "torrent_get" {
			t.Fatalf("non-read method sent: %s", method)
		}
	}
}

func TestTransmissionRejectsUnsupportedVersions(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"old app":             func(result map[string]any) { result["version"] = "4.0.6 (synthetic)" },
		"wrong rpc minor":     func(result map[string]any) { result["rpc_version_semver"] = "6.1.0" },
		"wrong rpc major":     func(result map[string]any) { result["rpc_version_semver"] = "7.0.0" },
		"missing rpc version": func(result map[string]any) { delete(result, "rpc_version_semver") },
		"null app version":    func(result map[string]any) { result["version"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			body := mutateJSON(t, apiFixture(t, "transmission-4.1.3-session.json"), func(envelope map[string]any) {
				edit(envelope["result"].(map[string]any))
			})
			var torrentRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string `json:"method"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Method == "torrent_get" {
					torrentRequests.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			_, err := ReadTransmission(context.Background(), apiConfig(server.URL))
			if err == nil {
				t.Fatal("unsupported or incomplete session accepted")
			}
			if torrentRequests.Load() != 0 {
				t.Fatal("torrent list read before version was accepted")
			}
		})
	}
}

func TestTransmissionRejectsMissingNullAndInvalidTorrentFields(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing hash":         func(row map[string]any) { delete(row, "hash_string") },
		"null name":            func(row map[string]any) { row["name"] = nil },
		"invalid download dir": func(row map[string]any) { row["download_dir"] = "relative/path" },
		"invalid ratio":        func(row map[string]any) { row["upload_ratio"] = -3.0 },
		"negative seed time":   func(row map[string]any) { row["seconds_seeding"] = -1.0 },
		"invalid status":       func(row map[string]any) { row["status"] = 7.0 },
		"invalid left":         func(row map[string]any) { row["left_until_done"] = -1.0 },
		"invalid metadata":     func(row map[string]any) { row["metadata_percent_complete"] = 1.1 },
		"null trackers":        func(row map[string]any) { row["trackers"] = nil },
		"missing announce":     func(row map[string]any) { row["trackers"] = []any{map[string]any{"id": 1.0}} },
		"missing labels":       func(row map[string]any) { delete(row, "labels") },
		"null labels":          func(row map[string]any) { row["labels"] = nil },
		"non-string label":     func(row map[string]any) { row["labels"] = []any{42} },
		"empty label":          func(row map[string]any) { row["labels"] = []any{""} },
		"comma in label":       func(row map[string]any) { row["labels"] = []any{"one,two"} },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			session := apiFixture(t, "transmission-4.1.3-session.json")
			torrents := mutateJSON(t, apiFixture(t, "transmission-4.1.3-torrents.json"), func(envelope map[string]any) {
				rows := envelope["result"].(map[string]any)["torrents"].([]any)
				edit(rows[0].(map[string]any))
			})
			server := transmissionResponseServer(t, session, torrents)
			defer server.Close()
			if _, err := ReadTransmission(context.Background(), apiConfig(server.URL)); err == nil {
				t.Fatal("invalid torrent accepted")
			}
		})
	}
}

func TestTransmissionIntegerCountersUseBoundedExactJSONNumbers(t *testing.T) {
	t.Run("integral decimal is accepted", func(t *testing.T) {
		torrents := apiFixture(t, "transmission-4.1.3-torrents.json")
		needle := []byte(`"seconds_seeding": 7,`)
		if !bytes.Contains(torrents, needle) {
			t.Fatal("fixture counter not found")
		}
		torrents = bytes.Replace(torrents, needle, []byte(`"seconds_seeding": 7.0,`), 1)
		server := transmissionResponseServer(t, apiFixture(t, "transmission-4.1.3-session.json"), torrents)
		defer server.Close()
		if _, err := ReadTransmission(context.Background(), apiConfig(server.URL)); err != nil {
			t.Fatalf("integral decimal counter rejected: %v", err)
		}
	})

	for name, replacement := range map[string]string{
		"quoted number":        `"seconds_seeding": "7",`,
		"fractional number":    `"seconds_seeding": 7.5,`,
		"large positive power": `"seconds_seeding": 1e999999999,`,
		"large negative power": `"seconds_seeding": 1e-999999999,`,
		"out of range integer": `"seconds_seeding": 9223372036854775808,`,
	} {
		t.Run(name, func(t *testing.T) {
			torrents := apiFixture(t, "transmission-4.1.3-torrents.json")
			needle := []byte(`"seconds_seeding": 7,`)
			if !bytes.Contains(torrents, needle) {
				t.Fatal("fixture counter not found")
			}
			torrents = bytes.Replace(torrents, needle, []byte(replacement), 1)
			server := transmissionResponseServer(t, apiFixture(t, "transmission-4.1.3-session.json"), torrents)
			defer server.Close()
			if _, err := ReadTransmission(context.Background(), apiConfig(server.URL)); err == nil {
				t.Fatal("invalid integer spelling accepted")
			}
		})
	}
}

func TestTransmissionRejectsDuplicateCanonicalHashes(t *testing.T) {
	session := apiFixture(t, "transmission-4.1.3-session.json")
	torrents := mutateJSON(t, apiFixture(t, "transmission-4.1.3-torrents.json"), func(envelope map[string]any) {
		rows := envelope["result"].(map[string]any)["torrents"].([]any)
		rows[1].(map[string]any)["hash_string"] = strings.Repeat("B", 40)
	})
	server := transmissionResponseServer(t, session, torrents)
	defer server.Close()
	if _, err := ReadTransmission(context.Background(), apiConfig(server.URL)); err == nil {
		t.Fatal("duplicate hash accepted")
	}
}

func TestTransmissionHTTPResponseLimitRedirectAndCancellation(t *testing.T) {
	t.Run("response limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
		}))
		defer server.Close()
		_, err := ReadTransmission(context.Background(), apiConfig(server.URL))
		if err == nil || strings.Contains(err.Error(), strings.Repeat("x", 20)) {
			t.Fatalf("oversized response was not safely rejected: %v", err)
		}
	})

	t.Run("redirect does not forward credentials or leak endpoint", func(t *testing.T) {
		var targetHits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHits.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Errorf("authorization forwarded to redirect target")
			}
		}))
		defer target.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/capture", http.StatusFound)
		}))
		defer source.Close()
		c := apiConfig(source.URL)
		_, err := ReadTransmission(context.Background(), c)
		if err == nil || strings.Contains(err.Error(), source.URL) || strings.Contains(err.Error(), c.Credentials.TransmissionPassword) {
			t.Fatalf("unsafe redirect error: %v", err)
		}
		if targetHits.Load() != 0 {
			t.Fatal("redirect target received a request")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer server.Close()
		defer close(release)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := ReadTransmission(ctx, apiConfig(server.URL))
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("request did not reach the test server")
		}
		cancel()
		select {
		case err := <-done:
			if err == nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was not preserved: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("client did not return after cancellation")
		}
	})
}

func TestReadQueuePaginatesNormalizesAndIgnoresStatus(t *testing.T) {
	page2 := apiFixture(t, "sonarr-v3-queue-page-2.json")
	var pages []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/queue" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "synthetic-api-key" {
			t.Errorf("API key header missing")
		}
		q := r.URL.Query()
		if q.Get("pageSize") != "100" || q.Get("includeUnknownSeriesItems") != "true" || q.Get("includeUnknownMovieItems") != "" {
			t.Errorf("unexpected query: %v", q)
		}
		page, _ := url.QueryUnescape(q.Get("page"))
		var pageNumber int
		_, _ = fmt.Sscan(page, &pageNumber)
		pages = append(pages, pageNumber)
		w.Header().Set("Content-Type", "application/json")
		if pageNumber == 1 {
			var records []any
			for i := 0; i < 100; i++ {
				records = append(records, map[string]any{"id": i + 1, "downloadId": fmt.Sprintf("Item-%03d", i), "status": "arbitrary-status"})
			}
			_, _ = w.Write(queueJSON(1, 100, 101, records))
			return
		}
		_, _ = w.Write(page2)
	}))
	defer server.Close()

	ids, err := ReadQueue(context.Background(), Endpoint{URL: server.URL}, "synthetic-api-key", "sonarr")
	if err != nil {
		t.Fatalf("ReadQueue: %v", err)
	}
	if len(ids) != 101 || !ids["page-two-hash"] || !ids["item-000"] || !ids["item-099"] {
		t.Fatalf("incomplete or noncanonical IDs: count=%d page2=%v", len(ids), ids["page-two-hash"])
	}
	if !reflect.DeepEqual(pages, []int{1, 2}) {
		t.Fatalf("requested pages = %v", pages)
	}
}

func TestReadQueueAcceptsExplicitEmptyIDsAndEmptyArrays(t *testing.T) {
	for name, records := range map[string][]any{
		"null and empty IDs": {map[string]any{"id": 1, "downloadId": nil}, map[string]any{"id": 2, "downloadId": ""}},
		"empty array":        {},
	} {
		t.Run(name, func(t *testing.T) {
			server := queueTestServer(func(_ *http.Request) []byte {
				return queueJSON(1, 100, len(records), records)
			})
			defer server.Close()
			ids, err := ReadQueue(context.Background(), Endpoint{URL: server.URL}, "key", "radarr")
			if err != nil || len(ids) != 0 {
				t.Fatalf("empty IDs should not protect anything: %v %v", ids, err)
			}
		})
	}
}

func TestReadQueueRejectsMalformedPaginationAndRecords(t *testing.T) {
	for name, body := range map[string][]byte{
		"missing records":    []byte(`{"page":1,"pageSize":100,"sortKey":"timeleft","sortDirection":"descending","totalRecords":0}`),
		"null records":       []byte(`{"page":1,"pageSize":100,"sortKey":"timeleft","sortDirection":"descending","totalRecords":0,"records":null}`),
		"wrong page":         queueJSON(2, 100, 0, []any{}),
		"over count":         queueJSON(1, 100, 0, []any{map[string]any{"id": 1, "downloadId": "x"}}),
		"missing downloadId": queueJSON(1, 100, 1, []any{map[string]any{"id": 1, "status": "downloading"}}),
		"missing record ID":  queueJSON(1, 100, 1, []any{map[string]any{"downloadId": "x"}}),
		"wrong ID type":      queueJSON(1, 100, 1, []any{map[string]any{"id": 1, "downloadId": 12}}),
	} {
		t.Run(name, func(t *testing.T) {
			server := queueTestServer(func(_ *http.Request) []byte { return body })
			defer server.Close()
			if _, err := ReadQueue(context.Background(), Endpoint{URL: server.URL}, "key", "radarr"); err == nil {
				t.Fatal("invalid queue response accepted")
			}
		})
	}
}

func TestReadQueueRejectsDuplicateIDsAndChangedOrShortPages(t *testing.T) {
	for name, pageFn := range map[string]func(*http.Request) []byte{
		"duplicate record IDs even for empty downloads": func(_ *http.Request) []byte {
			return queueJSON(1, 100, 2, []any{map[string]any{"id": 8, "downloadId": nil}, map[string]any{"id": 8, "downloadId": ""}})
		},
		"duplicate record ID across pages despite null downloadId": func(r *http.Request) []byte {
			if r.URL.Query().Get("page") == "1" {
				records := make([]any, 100)
				for i := range records {
					records[i] = map[string]any{"id": i + 1, "downloadId": fmt.Sprintf("id-%d", i)}
				}
				return queueJSON(1, 100, 101, records)
			}
			return queueJSON(2, 100, 101, []any{map[string]any{"id": 1, "downloadId": nil}})
		},
		"short page": func(_ *http.Request) []byte {
			return queueJSON(1, 100, 101, []any{map[string]any{"id": 1, "downloadId": "ABC"}})
		},
		"too many total": func(_ *http.Request) []byte {
			return queueJSON(1, 100, 100001, []any{})
		},
		"changed page metadata": func(r *http.Request) []byte {
			if r.URL.Query().Get("page") == "1" {
				records := make([]any, 100)
				for i := range records {
					records[i] = map[string]any{"id": i + 1, "downloadId": fmt.Sprintf("id-%d", i)}
				}
				return queueJSON(1, 100, 101, records)
			}
			return queueJSON(2, 100, 102, []any{map[string]any{"id": 101, "downloadId": "page2"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := queueTestServer(pageFn)
			defer server.Close()
			if _, err := ReadQueue(context.Background(), Endpoint{URL: server.URL}, "key", "sonarr"); err == nil {
				t.Fatal("invalid queue pagination accepted")
			}
		})
	}
}

func TestReadQueueDeduplicatesDownloadIDsFromDistinctRecords(t *testing.T) {
	server := queueTestServer(func(_ *http.Request) []byte {
		return queueJSON(1, 100, 2, []any{
			map[string]any{"id": 1, "downloadId": "ABC"},
			map[string]any{"id": 2, "downloadId": "abc", "status": "downloading"},
		})
	})
	defer server.Close()
	ids, err := ReadQueue(context.Background(), Endpoint{URL: server.URL}, "key", "sonarr")
	if err != nil || len(ids) != 1 || !ids["abc"] {
		t.Fatalf("same download on distinct records should be deduplicated: %v %v", ids, err)
	}
}

func TestReadQueueHTTPGuardsAndBadArguments(t *testing.T) {
	t.Run("oversized response", func(t *testing.T) {
		server := queueTestServer(func(_ *http.Request) []byte { return bytes.Repeat([]byte("x"), maxResponseBytes+1) })
		defer server.Close()
		_, err := ReadQueue(context.Background(), Endpoint{URL: server.URL}, "key", "sonarr")
		if err == nil || strings.Contains(err.Error(), strings.Repeat("x", 20)) {
			t.Fatalf("oversized response was not safely rejected: %v", err)
		}
	})
	t.Run("redirect does not expose API key", func(t *testing.T) {
		var targetHits atomic.Int32
		key := "synthetic-secret-api-key"
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetHits.Add(1) }))
		defer target.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Api-Key") != key {
				t.Errorf("API key was not sent to configured endpoint")
			}
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer source.Close()
		_, err := ReadQueue(context.Background(), Endpoint{URL: source.URL}, key, "sonarr")
		if err == nil || strings.Contains(err.Error(), source.URL) || strings.Contains(err.Error(), key) || targetHits.Load() != 0 {
			t.Fatalf("unsafe redirect behavior: err=%v target hits=%d", err, targetHits.Load())
		}
	})
	t.Run("HTTP error body and endpoint are not returned", func(t *testing.T) {
		const canary = "synthetic-response-secret"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, canary)
		}))
		defer server.Close()
		_, err := ReadQueue(context.Background(), Endpoint{URL: server.URL}, "key", "sonarr")
		if err == nil || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), canary) {
			t.Fatalf("unsafe HTTP error: %v", err)
		}
	})
	if _, err := ReadQueue(context.Background(), Endpoint{URL: "http://127.0.0.1"}, "", "sonarr"); err == nil {
		t.Fatal("empty API key accepted")
	}
	if _, err := ReadQueue(context.Background(), Endpoint{URL: "http://127.0.0.1"}, "key", "transmission"); err == nil {
		t.Fatal("unknown service accepted")
	}
}

func transmissionResponseServer(t *testing.T, session, torrents []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "session_get" {
			_, _ = w.Write(session)
		} else {
			_, _ = w.Write(torrents)
		}
	}))
	return server
}

func queueTestServer(pageFn func(*http.Request) []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pageFn(r))
	}))
}

func queueJSON(page, pageSize, total int, records []any) []byte {
	b, _ := json.Marshal(map[string]any{
		"page": page, "pageSize": pageSize, "sortKey": "timeleft",
		"sortDirection": "descending", "totalRecords": total, "records": records,
	})
	return b
}

func mutateJSON(t *testing.T, input []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(input, &value); err != nil {
		t.Fatal(err)
	}
	edit(value)
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
