package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zewelor/arrtify/internal/cleaner"
)

var version = "dev"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	started := time.Now()
	flags := flag.NewFlagSet("arrtify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	defaultConfigPath := "config.yml"
	if path, set := os.LookupEnv("ARRTIFY_CONFIG_FILE"); set {
		defaultConfigPath = path
	}
	configPath := flags.String("config", defaultConfigPath, "path to YAML configuration (ARRTIFY_CONFIG_FILE)")
	check := flags.Bool("check", false, "validate configuration and required environment without network access")
	showHelp := func() {
		fmt.Fprintln(stdout, "Usage: arrtify --config config.yml [--check]")
		flags.SetOutput(stdout)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}
	// FlagSet invokes Usage for parse failures too; stdout must remain JSON.
	flags.Usage = func() {}
	parseErr := flags.Parse(args)
	if parseErr == flag.ErrHelp {
		showHelp()
		return 0
	}
	mode := "dry-run"
	if *check {
		mode = "check"
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		fmt.Fprintln(stderr, "cannot initialize run identity")
		return 1
	}
	runID := hex.EncodeToString(id)
	baseLogger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			a.Value = slog.TimeValue(a.Value.Time().UTC())
		}
		return a
	}})).With("run_id", runID, "app_version", version)
	logger := baseLogger.With("mode", mode)
	report := cleaner.RunReport{DryRun: true, Status: "complete", Decisions: []cleaner.Decision{}}
	finish := func(code int) int {
		report.RunID = runID
		report.Mode = mode
		report.Version = version
		report.DurationSeconds = time.Since(started).Seconds()
		logger.Info(
			"run finished",
			"event",
			"run_finished",
			"status",
			report.Status,
			"scanned",
			report.Scanned,
			"planned",
			report.Planned,
			"attempted",
			report.Attempted,
			"skipped",
			report.Skipped,
			"would_delete",
			report.WouldDelete,
			"removed",
			report.Removed,
			"failed",
			report.Failed,
			"unknown",
			report.Unknown,
			"not_attempted",
			report.NotAttempted,
			"errors",
			report.Errors,
			"dry_run", report.DryRun,
			"sync_tracker_label", report.SyncTrackerLabel,
			"radarr_imported_label", report.RadarrImportedLabel,
			"sonarr_imported_label", report.SonarrImportedLabel,
			"tracker_labels_planned", report.TrackerLabelsPlanned,
			"tracker_labels_changed", report.TrackerLabelsChanged,
			"duration_seconds",
			report.DurationSeconds,
		)
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			logger.Error("cannot write final report", "event", "report_write_failed")
			return 1
		}
		return code
	}
	invalid := func(message string) int {
		logger.Info("run started", "event", "run_started")
		report.Status = "failed"
		report.Errors = 1
		logger.Error(message, "event", "configuration_failed")
		return finish(2)
	}
	if parseErr != nil || flags.NArg() != 0 {
		return invalid("invalid command-line arguments; use --help")
	}
	file, err := os.Open(*configPath)
	if err != nil {
		return invalid("cannot open configuration file")
	}
	c, err := cleaner.ParseConfig(file, os.LookupEnv)
	closeErr := file.Close()
	if err != nil {
		return invalid(err.Error())
	}
	if closeErr != nil {
		return invalid("cannot close configuration file")
	}
	report.DryRun = c.IsDryRun()
	report.SyncTrackerLabel = bool(c.SyncTrackerLabel)
	report.RadarrImportedLabel = string(c.Radarr.ImportedLabel)
	report.SonarrImportedLabel = string(c.Sonarr.ImportedLabel)
	if !*check {
		mode = c.RunMode()
		logger = baseLogger.With("mode", mode)
	}
	logger.Info("run started", "event", "run_started", "dry_run", report.DryRun,
		"sync_tracker_label", report.SyncTrackerLabel,
		"radarr_imported_label", report.RadarrImportedLabel, "sonarr_imported_label", report.SonarrImportedLabel)
	for _, service := range []struct{ name, label string }{
		{name: "sonarr", label: report.SonarrImportedLabel},
		{name: "radarr", label: report.RadarrImportedLabel},
	} {
		enabled := service.label != ""
		message := "import reporting enabled"
		if !enabled {
			message = "import reporting disabled: imported_label is not configured"
		}
		logger.Info(
			message,
			"event", "import_reporting",
			"service", service.name,
			"enabled", enabled,
			"setting", service.name+".imported_label",
			"label", service.label,
		)
	}
	if *check {
		return finish(0)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 270*time.Second)
	defer cancel()
	report = cleaner.Run(ctx, c, logger)
	if report.Errors != 0 {
		return finish(1)
	}
	return finish(0)
}
