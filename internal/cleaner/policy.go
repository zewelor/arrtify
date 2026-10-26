package cleaner

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"time"
)

type Torrent struct {
	Hash, Name, DownloadDir string
	Ratio                   float64
	SecondsSeeding          int64
	Status                  int
	IsFinished              bool
	LeftUntilDone           int64
	MetadataComplete        float64
	Trackers                []string
	Labels                  []string
}
type Decision struct {
	TrackerLabel    *TrackerLabelDecision `json:"tracker_label,omitempty"`
	Hash            string                `json:"torrent_hash"`
	Name            string                `json:"torrent_name"`
	Category        string                `json:"category"`
	Rule            string                `json:"rule,omitempty"`
	Reason          string                `json:"reason"`
	Outcome         string                `json:"outcome"`
	UploadRatio     float64               `json:"upload_ratio"`
	SecondsSeeding  int64                 `json:"seed_time_seconds"`
	SeedTime        string                `json:"seed_time"`
	UploadRatioKind string                `json:"upload_ratio_kind"`
	Criteria        *RuleCriteria         `json:"criteria,omitempty"`
}

// RuleCriteria records the exact thresholds and predicate results used by the
// decision. Nil criteria mean a guard stopped evaluation before a rule was used.
type RuleCriteria struct {
	MinUploadRatio    float64 `json:"min_upload_ratio"`
	MinSeedTime       string  `json:"min_seed_time"`
	MaxSeedTime       string  `json:"max_seed_time,omitempty"`
	MinUploadRatioMet bool    `json:"min_upload_ratio_met"`
	MinSeedTimeMet    bool    `json:"min_seed_time_met"`
	MaxSeedTimeMet    *bool   `json:"max_seed_time_met,omitempty"`
}

// decision projects a validated torrent into the shared report/log model.
func (t Torrent) decision() Decision {
	hash, _ := canonicalHash(t.Hash)
	category, _ := Category(t.DownloadDir)
	ratioKind := "finite"
	switch t.Ratio {
	case -1:
		ratioKind = "not_available"
	case -2:
		ratioKind = "infinite"
	}
	return Decision{
		UploadRatioKind: ratioKind,
		Hash:            hash, Name: t.Name, Category: category, Outcome: "skipped",
		UploadRatio: t.Ratio, SecondsSeeding: t.SecondsSeeding,
		// Avoid multiplying an int64 counter into time.Duration: large valid RPC
		// counters would overflow even though displaying their hours is safe.
		SeedTime: fmt.Sprintf("%dh%dm%ds", t.SecondsSeeding/3600, t.SecondsSeeding%3600/60, t.SecondsSeeding%60),
	}
}

func Category(path string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) {
		return "", errors.New("invalid absolute DownloadDir")
	}
	for _, s := range strings.Split(path, "/") {
		if s == "." || s == ".." {
			return "", errors.New("DownloadDir contains dot segments")
		}
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "", errors.New("DownloadDir has no category")
	}
	return strings.ToLower(path[strings.LastIndex(path, "/")+1:]), nil
}

func Hostname(s string) (string, error) {
	h := strings.ToLower(strings.TrimSuffix(s, "."))
	if net.ParseIP(h) != nil {
		return h, nil
	}
	if h == "" || len(h) > 253 {
		return "", errors.New("invalid hostname")
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("invalid hostname")
			}
		}
	}
	return h, nil
}

func canonicalHash(s string) (string, error) {
	if len(s) != 40 {
		return "", errors.New("invalid torrent hash")
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", errors.New("invalid torrent hash")
	}
	return strings.ToLower(s), nil
}

func trackerHost(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", errors.New("invalid tracker announce URL")
	}
	return Hostname(u.Hostname())
}

func (t Torrent) validate() error {
	if _, err := canonicalHash(t.Hash); err != nil {
		return err
	}
	if t.Name == "" {
		return errors.New("missing torrent name")
	}
	if _, err := Category(t.DownloadDir); err != nil {
		return err
	}
	if math.IsNaN(t.Ratio) || math.IsInf(t.Ratio, 0) || (t.Ratio < 0 && t.Ratio != -1 && t.Ratio != -2) {
		return errors.New("invalid upload_ratio")
	}
	if t.SecondsSeeding < 0 || t.LeftUntilDone < 0 || t.Status < 0 || t.Status > 6 || math.IsNaN(t.MetadataComplete) || t.MetadataComplete < 0 || t.MetadataComplete > 1 {
		return errors.New("invalid torrent counters or state")
	}
	if t.Trackers == nil {
		return errors.New("missing trackers")
	}
	for _, s := range t.Trackers {
		if _, err := trackerHost(s); err != nil {
			return err
		}
	}
	return nil
}

func Evaluate(c Config, t Torrent, protected bool) (Decision, error) {
	if err := t.validate(); err != nil {
		return Decision{}, fmt.Errorf("invalid torrent data: %w", err)
	}
	d := t.decision()
	if !(t.Status == 5 || t.Status == 6 || t.Status == 0 && t.IsFinished) || t.LeftUntilDone != 0 || t.MetadataComplete != 1 {
		d.Reason = "ineligible_state"
		return d, nil
	}
	if protected {
		d.Reason = "arr_queue"
		return d, nil
	}
	ignored, err := matchesIgnoredTracker(t.Trackers, c.IgnoreTrackers)
	if err != nil {
		return Decision{}, err
	}
	if ignored {
		d.Reason = "ignored_tracker"
		return d, nil
	}
	for _, rule := range c.Rules {
		matches := false
		for _, s := range rule.Categories {
			if d.Category == s {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		d.Rule = rule.Name
		d.Reason = "rule_not_satisfied"
		d.Criteria = &RuleCriteria{
			MinUploadRatio:    *rule.Ratio,
			MinSeedTime:       time.Duration(*rule.MinSeedTime).String(),
			MinUploadRatioMet: t.Ratio >= 0 && t.Ratio >= *rule.Ratio,
			MinSeedTimeMet:    seedTimeReached(t.SecondsSeeding, *rule.MinSeedTime),
		}
		maxMet := false
		if rule.MaxSeedTime != nil {
			d.Criteria.MaxSeedTime = time.Duration(*rule.MaxSeedTime).String()
			maxMet = seedTimeReached(t.SecondsSeeding, *rule.MaxSeedTime)
			d.Criteria.MaxSeedTimeMet = &maxMet
		}
		if d.Criteria.MinUploadRatioMet && d.Criteria.MinSeedTimeMet || maxMet {
			d.Outcome = "would_delete"
			d.Reason = "rule_satisfied"
		}
		return d, nil
	}
	d.Reason = "no_rule"
	return d, nil
}

// Compare integral RPC seconds without rounding a configured subsecond boundary.
func seedTimeReached(seconds int64, threshold Duration) bool {
	duration := time.Duration(threshold)
	wholeSeconds := int64(duration / time.Second)
	return seconds > wholeSeconds || seconds == wholeSeconds && duration%time.Second == 0
}
