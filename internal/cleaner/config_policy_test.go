package cleaner

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func testCredentialsLookup(name string) (string, bool) {
	switch name {
	case "TRANSMISSION_USER", "TRANSMISSION_PASSWORD", "SONARR_API_KEY", "RADARR_API_KEY":
		return "test-secret", true
	default:
		return "", false
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	b, err := os.ReadFile("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(strings.NewReader(string(b)), testCredentialsLookup)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigStrict(t *testing.T) {
	b, err := os.ReadFile("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	valid := string(b)
	for name, change := range map[string]func(string) string{
		"unknown":             func(s string) string { return s + "unexpected: token-canary\n" },
		"root imported label": func(s string) string { return s + "radarr_imported_label: custom\n" },
		"Transmission imported label": func(s string) string {
			return strings.Replace(s, "transmission:\n", "transmission:\n  imported_label: custom\n", 1)
		},
		"duplicate":       func(s string) string { return s + "rules: []\n" },
		"second document": func(s string) string { return s + "---\n{}\n" },
		"missing ratio":   func(s string) string { return strings.ReplaceAll(s, "    ratio: 2\n", "") },
		"null minimum":    func(s string) string { return strings.ReplaceAll(s, "24h", "null") },
		"unitless zero":   func(s string) string { return strings.ReplaceAll(s, "24h", "\"0\"") },
		"null maximum":    func(s string) string { return strings.ReplaceAll(s, "72h", "null") },
		"missing trackers": func(s string) string {
			return strings.Replace(s, "ignore_trackers:\n  - tracker.example.invalid\n", "", 1)
		},
		"duration number":   func(s string) string { return strings.ReplaceAll(s, "24h", "24") },
		"negative duration": func(s string) string { return strings.ReplaceAll(s, "24h", "-1h") },
		"zero max":          func(s string) string { return strings.ReplaceAll(s, "72h", "0h") },
		"nan":               func(s string) string { return strings.ReplaceAll(s, "ratio: 2", "ratio: .nan") },
		"inf":               func(s string) string { return strings.ReplaceAll(s, "ratio: 2", "ratio: .inf") },
		"negative ratio":    func(s string) string { return strings.ReplaceAll(s, "ratio: 2", "ratio: -1") },
		"bad port": func(s string) string {
			return strings.Replace(s, "http://sonarr.example.invalid:8989", "http://sonarr.example.invalid:99999", 1)
		},
		"userinfo": func(s string) string {
			return strings.ReplaceAll(s, "http://sonarr.", "http://user:token-canary@sonarr.")
		},
		"query": func(s string) string {
			return strings.Replace(s, "http://sonarr.example.invalid:8989", "http://sonarr.example.invalid:8989?apikey=token-canary", 1)
		},
		"bad transmission path": func(s string) string { return strings.Replace(s, "/transmission/rpc", "/wrong", 1) },
		"no rules":              func(s string) string { return s[:strings.Index(s, "rules:")] + "rules: []\n" },
		"duplicate rule":        func(s string) string { return strings.Replace(s, "name: radarr", "name: sonarr", 1) },
		"empty category":        func(s string) string { return strings.Replace(s, "[radarr, radarr-imported]", "[]", 1) },
		"bad tracker": func(s string) string {
			return strings.Replace(s, "tracker.example.invalid", "https://tracker.example.invalid/path", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig(strings.NewReader(change(valid)), testCredentialsLookup)
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			if strings.Contains(err.Error(), "token-canary") || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("secret leaked: %v", err)
			}
		})
	}
	c := testConfig(t)
	if len(c.Rules) != 3 {
		t.Fatalf("rules: %d", len(c.Rules))
	}
	_, err = ParseConfig(strings.NewReader(valid), func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("missing credentials accepted")
	}
	_, err = ParseConfig(strings.NewReader(strings.ReplaceAll(strings.ReplaceAll(valid, "ratio: 2", "ratio: 0"), "24h", "0h")), testCredentialsLookup)
	if err != nil {
		t.Fatalf("explicit zero: %v", err)
	}
}

func TestConfigTrackerLabelSync(t *testing.T) {
	b, err := os.ReadFile("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value   string
		want, invalid bool
	}{
		{"omitted", "", false, false},
		{"disabled", "false", false, false},
		{"enabled", "true", true, false},
		{"null", "null", false, true},
		{"quoted boolean", `"true"`, false, true},
		{"legacy yes", "yes", false, true},
		{"legacy on", "on", false, true},
		{"number", "1", false, true},
		{"array", "[]", false, true},
		{"merged boolean", "merge:true", true, false},
		{"merged legacy yes", "merge:yes", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := strings.Replace(string(b), "sync_tracker_label: false\n", "", 1)
			if strings.HasPrefix(tc.value, "merge:") {
				s += "\n<<: {sync_tracker_label: " + strings.TrimPrefix(tc.value, "merge:") + "}\n"
			} else if tc.value != "" {
				s += "\nsync_tracker_label: " + tc.value + "\n"
			}
			c, err := ParseConfig(strings.NewReader(s), testCredentialsLookup)
			if (err != nil) != tc.invalid {
				t.Fatalf("error = %v, want invalid %v", err, tc.invalid)
			}
			if err == nil && bool(c.SyncTrackerLabel) != tc.want {
				t.Fatalf("sync = %v, want %v", c.SyncTrackerLabel, tc.want)
			}
		})
	}
}

func TestConfigArrImportedLabels(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"sonarr", "radarr"} {
		for _, tc := range []struct {
			value   string
			invalid bool
		}{
			{value: ""}, {value: `""`}, {value: "media-finished"}, {value: `"My Imported Media"`},
			{value: "true", invalid: true}, {value: "123", invalid: true}, {value: "null", invalid: true},
			{value: "[]", invalid: true}, {value: `" media-finished"`, invalid: true},
			{value: `"media-finished "`, invalid: true}, {value: `"media\nfinished"`, invalid: true},
		} {
			t.Run(service+"/"+tc.value, func(t *testing.T) {
				text := string(data)
				if tc.value != "" {
					text = strings.Replace(text, service+":\n", service+":\n  imported_label: "+tc.value+"\n", 1)
				}
				_, err := ParseConfig(strings.NewReader(text), testCredentialsLookup)
				if (err != nil) != tc.invalid {
					t.Fatalf("error=%v, want invalid=%t", err, tc.invalid)
				}
			})
		}
	}
}

func TestConfigDryRunDefaultsToReadOnly(t *testing.T) {
	if !(Config{}).IsDryRun() {
		t.Fatal("zero configuration enables writes")
	}
	b, err := os.ReadFile("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value        string
		set, want, invalid bool
	}{
		{"unset", "", false, true, false}, {"true", "true", true, true, false}, {"false", "false", true, false, false},
		{"empty", "", true, true, true}, {"quoted", `"false"`, true, true, true}, {"legacy", "off", true, true, true},
		{"uppercase", "FALSE", true, true, true}, {"number", "0", true, true, true}, {"whitespace", "false ", true, true, true},
		{"redacted", "config-secret-canary", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				if name == "ARRTIFY_DRY_RUN" {
					return tc.value, tc.set
				}
				return testCredentialsLookup(name)
			}
			c, err := ParseConfig(strings.NewReader(string(b)), lookup)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v, invalid=%v", err, tc.invalid)
			}
			if err != nil && strings.Contains(err.Error(), "config-secret-canary") {
				t.Fatal("environment value leaked")
			}
			if err == nil && c.IsDryRun() != tc.want {
				t.Fatalf("dry-run=%v, want %v", c.IsDryRun(), tc.want)
			}
		})
	}
	for _, value := range []string{"true", "false"} {
		if _, err := ParseConfig(strings.NewReader(string(b)+"\ndry_run: "+value+"\n"), testCredentialsLookup); err == nil {
			t.Fatalf("YAML dry_run=%s accepted", value)
		}
	}
}

func TestRunModeDependsOnlyOnGlobalDryRun(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		dryRun *bool
		mode   string
	}{
		{nil, "dry-run"}, {&on, "dry-run"}, {&off, "apply"},
	} {
		for _, syncEnabled := range []yamlBool{false, true} {
			c := Config{dryRun: tc.dryRun, SyncTrackerLabel: syncEnabled}
			if got := c.RunMode(); got != tc.mode {
				t.Fatalf("sync=%v, mode=%s, want %s", syncEnabled, got, tc.mode)
			}
		}
	}
}

func sampleTorrent() Torrent {
	return Torrent{Hash: strings.Repeat("a", 40), Name: "Synthetic", DownloadDir: "/downloads/sonarr", Ratio: 2, SecondsSeeding: 24 * 3600, Status: 6, LeftUntilDone: 0, MetadataComplete: 1, Trackers: []string{}}
}

func TestPolicy(t *testing.T) {
	c := testConfig(t)
	for _, tc := range []struct {
		name            string
		change          func(*Torrent)
		protected       bool
		outcome, reason string
	}{
		{"equal", func(*Torrent) {}, false, "would_delete", "rule_satisfied"},
		{"below ratio", func(v *Torrent) { v.Ratio = 1.999 }, false, "skipped", "rule_not_satisfied"},
		{"below minimum", func(v *Torrent) { v.SecondsSeeding-- }, false, "skipped", "rule_not_satisfied"},
		{"above", func(v *Torrent) { v.Ratio = 3; v.SecondsSeeding++ }, false, "would_delete", "rule_satisfied"},
		{"queued", func(v *Torrent) { v.Status = 5 }, false, "would_delete", "rule_satisfied"},
		{"stopped finished", func(v *Torrent) { v.Status = 0; v.IsFinished = true }, false, "would_delete", "rule_satisfied"},
		{"stopped unfinished", func(v *Torrent) { v.Status = 0 }, false, "skipped", "ineligible_state"},
		{"downloading", func(v *Torrent) { v.Status = 4 }, false, "skipped", "ineligible_state"},
		{"verifying", func(v *Torrent) { v.Status = 2 }, false, "skipped", "ineligible_state"},
		{"incomplete", func(v *Torrent) { v.LeftUntilDone = 1 }, false, "skipped", "ineligible_state"},
		{"metadata", func(v *Torrent) { v.MetadataComplete = 0.5 }, false, "skipped", "ineligible_state"},
		{"case and trailing slash", func(v *Torrent) { v.DownloadDir = "/downloads/SONARR-IMPORTED///" }, false, "would_delete", "rule_satisfied"},
		{"unknown category", func(v *Torrent) { v.DownloadDir = "/downloads/unrelated" }, false, "skipped", "no_rule"},
		{"protected", func(*Torrent) {}, true, "skipped", "arr_queue"},
		{"tracker", func(v *Torrent) {
			v.Trackers = []string{"udp://ANNOUNCE.TRACKER.EXAMPLE.INVALID.:80/announce?passkey=secret"}
		}, false, "skipped", "ignored_tracker"},
		{"tracker not suffix", func(v *Torrent) { v.Trackers = []string{"https://eviltracker.example.invalid/announce"} }, false, "would_delete", "rule_satisfied"},
		{"tracker not substring", func(v *Torrent) { v.Trackers = []string{"https://tracker.example.invalid.other.invalid/announce"} }, false, "would_delete", "rule_satisfied"},
		{"ratio NA", func(v *Torrent) { v.Ratio = -1 }, false, "skipped", "rule_not_satisfied"},
		{"ratio INF", func(v *Torrent) { v.Ratio = -2 }, false, "skipped", "rule_not_satisfied"},
		{"max equal", func(v *Torrent) { v.DownloadDir = "/downloads/radarr"; v.Ratio = -2; v.SecondsSeeding = 168 * 3600 }, false, "would_delete", "rule_satisfied"},
		{"max below", func(v *Torrent) {
			v.DownloadDir = "/downloads/unlinked"
			v.Ratio = -1
			v.SecondsSeeding = 72*3600 - 1
		}, false, "skipped", "rule_not_satisfied"},
		{"max above", func(v *Torrent) {
			v.DownloadDir = "/downloads/unlinked"
			v.Ratio = -1
			v.SecondsSeeding = 72*3600 + 1
		}, false, "would_delete", "rule_satisfied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := sampleTorrent()
			tc.change(&v)
			d, err := Evaluate(c, v, tc.protected)
			if err != nil {
				t.Fatal(err)
			}
			if d.Outcome != tc.outcome || d.Reason != tc.reason {
				t.Fatalf("got %+v", d)
			}
		})
	}
	// The first matching rule is final, even when a later rule would qualify.
	c.Rules = append([]Rule{c.Rules[0]}, c.Rules...)
	high := 100.0
	c.Rules[0].Ratio = &high
	d, err := Evaluate(c, sampleTorrent(), false)
	if err != nil || d.Outcome != "skipped" {
		t.Fatalf("priority: %+v %v", d, err)
	}
	// Maximum does not depend on the minimum branch.
	c = testConfig(t)
	maximum := Duration(time.Hour)
	c.Rules[0].MaxSeedTime = &maximum
	v := sampleTorrent()
	v.Ratio = 0
	v.SecondsSeeding = 3600
	d, err = Evaluate(c, v, false)
	if err != nil || d.Outcome != "would_delete" {
		t.Fatalf("max OR: %+v %v", d, err)
	}
}

func TestInvalidTorrent(t *testing.T) {
	c := testConfig(t)
	for name, change := range map[string]func(*Torrent){
		"root": func(v *Torrent) { v.DownloadDir = "///" }, "relative": func(v *Torrent) { v.DownloadDir = "sonarr" }, "dot": func(v *Torrent) { v.DownloadDir = "/downloads/../sonarr" }, "nul": func(v *Torrent) { v.DownloadDir = "/x/sonarr\x00" },
		"ratio": func(v *Torrent) { v.Ratio = -3 }, "nan": func(v *Torrent) { v.Ratio = math.NaN() }, "time": func(v *Torrent) { v.SecondsSeeding = -1 }, "hash": func(v *Torrent) { v.Hash = "" }, "trackers nil": func(v *Torrent) { v.Trackers = nil }, "tracker invalid": func(v *Torrent) { v.Trackers = []string{"not-a-url"} }, "state": func(v *Torrent) { v.Status = 7 },
	} {
		t.Run(name, func(t *testing.T) {
			v := sampleTorrent()
			change(&v)
			if _, err := Evaluate(c, v, true); err == nil {
				t.Fatal("invalid data hidden by guard")
			}
		})
	}
}

func FuzzConfig(f *testing.F) {
	f.Add("rules: []\n")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ParseConfig(strings.NewReader(s), testCredentialsLookup)
	})
}
func FuzzCategory(f *testing.F) {
	for _, s := range []string{"/a/SONARR///", "/../x", "/", "x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := Category(s)
		if err == nil {
			again, err := Category("/" + c)
			if err != nil || again != c || c == "" {
				t.Fatalf("noncanonical %q", c)
			}
		}
	})
}
func FuzzHostname(f *testing.F) {
	f.Add("TRACKER.EXAMPLE.INVALID.")
	f.Fuzz(func(t *testing.T, s string) {
		h, err := Hostname(s)
		if err == nil {
			again, err := Hostname(h)
			if err != nil || again != h {
				t.Fatalf("noncanonical %q", h)
			}
		}
	})
}

func TestConfigServicePaths(t *testing.T) {
	b, err := os.ReadFile("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"http://example.invalid/prefix", "http://127.0.0.1:8080/prefix", "https://[::1]:443/prefix/"} {
		s := strings.ReplaceAll(string(b), "http://sonarr.example.invalid:8989", base)
		if _, err := ParseConfig(strings.NewReader(s), testCredentialsLookup); err != nil {
			t.Errorf("valid base %s: %v", base, err)
		}
	}
}

func TestPolicyKeepsSubsecondBoundaryAtLongSeedTimes(t *testing.T) {
	c := testConfig(t)
	minimum := Duration(2_000_000_000*time.Second + time.Nanosecond)
	c.Rules[0].MinSeedTime = &minimum
	torrent := sampleTorrent()
	torrent.SecondsSeeding = 2_000_000_000
	d, err := Evaluate(c, torrent, false)
	if err != nil || d.Outcome != "skipped" {
		t.Fatalf("seed time below minimum: %+v %v", d, err)
	}
	torrent.SecondsSeeding++
	d, err = Evaluate(c, torrent, false)
	if err != nil || d.Outcome != "would_delete" {
		t.Fatalf("seed time above minimum: %+v %v", d, err)
	}
}
