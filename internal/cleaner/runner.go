package cleaner

import (
	"context"
	"errors"
	"log/slog"
)

// Run plans all decisions before writes, then applies labels and cleanup in order.
func Run(ctx context.Context, c Config, logger *slog.Logger) RunReport {
	report := RunReport{
		Mode:                c.RunMode(),
		DryRun:              c.IsDryRun(),
		SyncTrackerLabel:    bool(c.SyncTrackerLabel),
		RadarrImportedLabel: string(c.Radarr.ImportedLabel),
		SonarrImportedLabel: string(c.Sonarr.ImportedLabel),
		Status:              "complete",
		Decisions:           []Decision{},
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
	sonarr, err := ReadQueue(ctx, c.Sonarr.Endpoint, c.Credentials.SonarrKey, "sonarr")
	if err != nil {
		return fail("sonarr", err)
	}
	radarr, err := ReadQueue(ctx, c.Radarr.Endpoint, c.Credentials.RadarrKey, "radarr")
	if err != nil {
		return fail("radarr", err)
	}
	radarrImports, err := readRadarrImports(ctx, c, torrents)
	if err != nil {
		return fail("radarr", err)
	}
	sonarrImports, err := readSonarrImports(ctx, c, torrents)
	if err != nil {
		return fail("sonarr", err)
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
		decision.RadarrImport = radarrImports[torrent.Hash]
		decision.SonarrImport = sonarrImports[torrent.Hash]
		decisions = append(decisions, decision)
	}
	if err := ctx.Err(); err != nil {
		return fail("run", err)
	}
	report.Decisions = decisions
	for i := range report.Decisions {
		d := &report.Decisions[i]
		if d.Outcome == "would_delete" {
			report.Planned++
			if !c.IsDryRun() {
				d.Outcome, d.Reason = "not_attempted", "aborted"
			}
		}
		if d.TrackerLabel.Action != "none" {
			report.TrackerLabelsPlanned++
		}
	}
	if c.ShouldSyncTrackerLabel() {
		syncTrackerLabels(ctx, rpc, c.IgnoreTrackers, &report, logger)
	}
	if !c.IsDryRun() && report.Errors == 0 {
		applyCleanup(ctx, rpc, c, &report, logger)
	}
	for _, d := range report.Decisions {
		switch d.Outcome {
		case "would_delete":
			report.WouldDelete++
		case "skipped":
			report.Skipped++
		case "removed":
			report.Removed++
		case "failed":
			report.Failed++
		case "unknown":
			report.Unknown++
		case "not_attempted":
			report.NotAttempted++
		}
		logDecision(logger, d)
	}
	return report
}

func applyCleanup(ctx context.Context, rpc *transmissionRPC, c Config, report *RunReport, logger *slog.Logger) {
	for i := range report.Decisions {
		d := &report.Decisions[i]
		if d.Outcome != "not_attempted" {
			continue
		}
		d.Outcome, d.Reason = "failed", "revalidation_failed"
		fresh, err := rpc.torrent(ctx, d.Hash)
		if err != nil {
			cleanupFailure(report, d, logger, err)
			return
		}
		if fresh == nil {
			d.Outcome, d.Reason = "skipped", "torrent_missing"
			continue
		}
		sonarr, err := ReadQueue(ctx, c.Sonarr.Endpoint, c.Credentials.SonarrKey, "sonarr")
		if err != nil {
			cleanupFailure(report, d, logger, err)
			return
		}
		radarr, err := ReadQueue(ctx, c.Radarr.Endpoint, c.Credentials.RadarrKey, "radarr")
		if err != nil {
			cleanupFailure(report, d, logger, err)
			return
		}
		decision, err := Evaluate(c, *fresh, sonarr[d.Hash] || radarr[d.Hash])
		if err != nil {
			cleanupFailure(report, d, logger, err)
			return
		}
		if decision.Category != d.Category && decision.Outcome == "would_delete" {
			decision.Outcome, decision.Reason = "skipped", "category_changed"
		}
		decision.TrackerLabel = d.TrackerLabel
		decision.RadarrImport = d.RadarrImport
		decision.SonarrImport = d.SonarrImport
		*d = decision
		if d.Outcome != "would_delete" {
			continue
		}
		if err := ctx.Err(); err != nil {
			d.Outcome, d.Reason = "failed", "cancelled"
			cleanupFailure(report, d, logger, err)
			return
		}
		logger.Info("torrent removal requested", "event", "delete_requested", "torrent_hash", d.Hash, "rule", d.Rule)
		report.Attempted++
		if err := rpc.remove(ctx, d.Hash); err != nil {
			d.Outcome, d.Reason = "unknown", "operation_unknown"
			if errors.Is(err, errRPCRejected) {
				d.Outcome, d.Reason = "failed", "remove_rejected"
			}
			cleanupFailure(report, d, logger, err)
			return
		}
		confirmed, err := rpc.torrent(ctx, d.Hash)
		if err == nil && confirmed != nil {
			err = errors.New("transmission: removed torrent is still present")
		}
		if err != nil {
			d.Outcome, d.Reason = "unknown", "operation_unknown"
			cleanupFailure(report, d, logger, err)
			return
		}
		d.Outcome = "removed"
		logger.Info("torrent removed", "event", "torrent_removed", "torrent_hash", d.Hash, "rule", d.Rule)
	}
}

func cleanupFailure(report *RunReport, d *Decision, logger *slog.Logger, err error) {
	report.Errors++
	report.Status = "failed"
	if report.TrackerLabelsChanged > 0 {
		report.Status = "partial"
	}
	for _, decision := range report.Decisions {
		if decision.Outcome == "removed" || decision.Outcome == "unknown" {
			report.Status = "partial"
			break
		}
	}
	event := "cleanup_failed"
	if d.Outcome == "unknown" {
		event = "operation_unknown"
	}
	logger.Error("torrent cleanup failed", "event", event, "torrent_hash", d.Hash, "outcome", d.Outcome, "error", err)
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
	if d.RadarrImport != nil {
		attrs = append(attrs, "radarr_import", d.RadarrImport)
	}
	if d.SonarrImport != nil {
		attrs = append(attrs, "sonarr_import", d.SonarrImport)
	}
	logger.Info("torrent decision", attrs...)
}
