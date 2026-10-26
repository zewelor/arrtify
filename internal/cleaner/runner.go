package cleaner

import (
	"context"
	"log/slog"
)

// Run reads all three sources and plans every decision before optional label writes.
func Run(ctx context.Context, c Config, logger *slog.Logger) RunReport {
	report := RunReport{
		Mode:             c.RunMode(),
		DryRun:           c.IsDryRun(),
		SyncTrackerLabel: bool(c.SyncTrackerLabel),
		Status:           "complete",
		Decisions:        []Decision{},
	}
	rpc, err := newTransmissionRPC(ctx, c)
	var torrents []Torrent
	if err == nil {
		torrents, err = rpc.readTorrents(ctx, nil)
	}
	if err != nil {
		report.Status = "failed"
		report.Errors = 1
		logger.Error("source read failed", "event", "source_read_failed", "service", "transmission", "error", err)
		return report
	}
	report.Scanned = len(torrents)
	fail := func(service string, err error) RunReport {
		report.Status = "failed"
		report.Errors = 1
		report.Planned = 0
		report.NotAttempted = len(torrents)
		for _, torrent := range torrents {
			d := torrent.decision()
			d.Outcome = "not_attempted"
			d.Reason = "source_read_failed"
			report.Decisions = append(report.Decisions, d)
			logDecision(logger, d)
		}
		logger.Error("source read failed", "event", "source_read_failed", "service", service, "error", err)
		return report
	}
	sonarr, err := ReadQueue(ctx, c.Sonarr, c.Credentials.SonarrKey, "sonarr")
	if err != nil {
		return fail("sonarr", err)
	}
	radarr, err := ReadQueue(ctx, c.Radarr, c.Credentials.RadarrKey, "radarr")
	if err != nil {
		return fail("radarr", err)
	}
	decisions := make([]Decision, 0, len(torrents))
	for _, torrent := range torrents {
		decision, err := Evaluate(c, torrent, sonarr[torrent.Hash] || radarr[torrent.Hash])
		if err != nil {
			return fail("policy", err)
		}
		label, _, err := planTrackerLabel(torrent, c.IgnoreTrackers)
		if err != nil {
			return fail("policy", err)
		}
		decision.TrackerLabel = &label
		decisions = append(decisions, decision)
	}
	if err := ctx.Err(); err != nil {
		return fail("run", err)
	}
	report.Decisions = decisions
	for _, d := range decisions {
		if d.TrackerLabel.Action != "none" {
			report.TrackerLabelsPlanned++
		}
	}
	if c.ShouldSyncTrackerLabel() {
		syncTrackerLabels(ctx, rpc, c.IgnoreTrackers, &report, logger)
	}
	for _, d := range decisions {
		switch d.Outcome {
		case "would_delete":
			report.Planned++
			report.WouldDelete++
		case "skipped":
			report.Skipped++
		}
		logDecision(logger, d)
	}
	return report
}

func logDecision(logger *slog.Logger, d Decision) {
	attrs := []any{
		"event", "torrent_decision",
		"torrent_hash", d.Hash,
		"torrent_name", d.Name,
		"category", d.Category,
		"reason", d.Reason,
		"outcome", d.Outcome,
		"upload_ratio", d.UploadRatio,
		"seed_time_seconds", d.SecondsSeeding,
		"upload_ratio_kind", d.UploadRatioKind,
		"seed_time", d.SeedTime,
	}
	if d.Rule != "" {
		attrs = append(attrs, "rule", d.Rule)
	}
	if d.Criteria != nil {
		attrs = append(attrs, "criteria", d.Criteria)
	}
	if d.TrackerLabel != nil {
		attrs = append(attrs, "tracker_label", d.TrackerLabel)
	}
	logger.Info("torrent decision", attrs...)
}
