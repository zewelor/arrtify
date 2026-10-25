package cleaner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSourceFailuresNeverReturnPartialData(t *testing.T) {
	for _, source := range []string{"transmission", "sonarr", "radarr"} {
		for _, tc := range []struct {
			name      string
			status    int
			body      string
			truncated bool
		}{
			{"unauthorized", 401, "secret-canary", false},
			{"forbidden", 403, "secret-canary", false},
			{"server error", 500, "secret-canary", false},
			{"malformed JSON", 200, `{"secret-canary":`, false},
			{"truncated response", 200, `{}`, true},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.truncated {
						w.Header().Set("Content-Length", "10000")
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer server.Close()
				var err error
				if source == "transmission" {
					var items []Torrent
					items, err = ReadTransmission(context.Background(), apiConfig(server.URL))
					if items != nil {
						t.Fatal("partial torrent data returned")
					}
				} else {
					var ids map[string]bool
					ids, err = ReadQueue(context.Background(), Endpoint{URL: server.URL}, "secret-canary", source)
					if ids != nil {
						t.Fatal("partial queue data returned")
					}
				}
				if err == nil || strings.Contains(err.Error(), "secret-canary") || strings.Contains(err.Error(), server.URL) {
					t.Fatalf("missing/unsafe failure: %v", err)
				}
			})
		}
	}
}

func TestQueueDeadlineCancelsRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	ids, err := ReadQueue(ctx, Endpoint{URL: server.URL}, "synthetic-key", "sonarr")
	if ids != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v %v", ids, err)
	}
}

func TestTransmissionRejectsInvalidRPCEnvelopes(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":99,"result":{"version":"4.1.3","rpc_version_semver":"6.0.1"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"secret-canary"}}`,
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		`{"jsonrpc":"1.0","id":1,"result":{}}`,
	} {
		server := transmissionResponseServer(t, []byte(body), nil)
		items, err := ReadTransmission(context.Background(), apiConfig(server.URL))
		server.Close()
		if err == nil || items != nil || strings.Contains(err.Error(), "secret-canary") {
			t.Fatalf("RPC failure: %v %v", items, err)
		}
	}
}
