package cleaner

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
)

const ignoredTrackerLabel = "arrtify:ignored-tracker"

type TrackerLabelDecision struct {
	Ignored bool   `json:"ignored"`
	Action  string `json:"action"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

func matchesIgnoredTracker(trackers, ignoredHosts []string) (bool, error) {
	matched := false
	for _, tracker := range trackers {
		host, err := trackerHost(tracker)
		if err != nil {
			return false, err
		}
		for _, ignored := range ignoredHosts {
			if host == ignored || strings.HasSuffix(host, "."+ignored) {
				matched = true
			}
		}
	}
	return matched, nil
}

func planTrackerLabel(t Torrent, ignoredHosts []string) (TrackerLabelDecision, []string, error) {
	ignored, err := matchesIgnoredTracker(t.Trackers, ignoredHosts)
	if err != nil {
		return TrackerLabelDecision{}, nil, err
	}
	d := TrackerLabelDecision{Ignored: ignored, Action: "none", Outcome: "unchanged"}
	labels := append([]string{}, t.Labels...)
	present := slices.Contains(labels, ignoredTrackerLabel)
	if ignored && !present {
		d.Action, d.Outcome = "add", "would_add"
		labels = append(labels, ignoredTrackerLabel)
	} else if !ignored && present {
		d.Action, d.Outcome = "remove", "would_remove"
		labels = slices.DeleteFunc(labels, func(label string) bool { return label == ignoredTrackerLabel })
	}
	return d, labels, nil
}

// Transmission replaces the complete label list and has no compare-and-swap.
// Fresh reads narrow the race with other writers; readback detects divergence.
// An ambiguous write is never retried automatically.
func syncTrackerLabels(ctx context.Context, rpc *transmissionRPC, ignored []string, report *RunReport, logger *slog.Logger) {
	for i := range report.Decisions {
		if report.Decisions[i].TrackerLabel.Action != "none" {
			report.Decisions[i].TrackerLabel.Outcome = "not_attempted"
		}
	}
	for i := range report.Decisions {
		d := &report.Decisions[i]
		if d.TrackerLabel.Action == "none" {
			continue
		}
		fresh, err := rpc.torrent(ctx, d.Hash)
		if err != nil {
			labelSyncFailure(report, d, logger, err, false)
			return
		}
		if fresh == nil {
			d.TrackerLabel.Outcome, d.TrackerLabel.Reason = "skipped", "torrent_missing"
			continue
		}
		plan, labels, err := planTrackerLabel(*fresh, ignored)
		if err != nil {
			labelSyncFailure(report, d, logger, err, false)
			return
		}
		d.TrackerLabel = &plan
		if plan.Action == "none" {
			continue
		}
		if err := ctx.Err(); err != nil {
			labelSyncFailure(report, d, logger, err, false)
			return
		}
		if err := rpc.setLabels(ctx, d.Hash, labels); err != nil {
			labelSyncFailure(report, d, logger, err, true)
			return
		}
		confirmed, err := rpc.torrent(ctx, d.Hash)
		if err == nil && (confirmed == nil || !sameLabels(confirmed.Labels, labels)) {
			err = errors.New("transmission: label update could not be confirmed")
		}
		if err != nil {
			labelSyncFailure(report, d, logger, err, true)
			return
		}
		if plan.Action == "add" {
			plan.Outcome = "added"
		} else {
			plan.Outcome = "removed"
		}
		report.TrackerLabelsChanged++
		logger.Info("tracker label synchronized", "event", "tracker_label_synced", "torrent_hash", d.Hash, "torrent_name", d.Name, "label", ignoredTrackerLabel, "action", plan.Action, "outcome", plan.Outcome)
	}
}

func sameLabels(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func labelSyncFailure(report *RunReport, d *Decision, logger *slog.Logger, err error, uncertain bool) {
	report.Status = "failed"
	d.TrackerLabel.Outcome = "failed"
	d.TrackerLabel.Reason = "label_sync_failed"
	if uncertain || report.TrackerLabelsChanged > 0 {
		report.Status = "partial"
	}
	if uncertain {
		d.TrackerLabel.Outcome = "unknown"
	}
	report.Errors++
	logger.Error("tracker label synchronization failed", "event", "tracker_label_failed", "torrent_hash", d.Hash, "torrent_name", d.Name, "action", d.TrackerLabel.Action, "outcome", d.TrackerLabel.Outcome, "error", err)
}
