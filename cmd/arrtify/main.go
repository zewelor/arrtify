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
	configPath := flags.String("config", "config.yml", "path to YAML configuration")
	check := flags.Bool("check", false, "validate configuration and required environment without network access")
	apply := flags.Bool("apply", false, "reserved for the later apply stage (currently rejected)")
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
	if *apply {
		mode = "apply"
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		fmt.Fprintln(stderr, "cannot initialize run identity")
		return 1
	}
	runID := hex.EncodeToString(id)
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			a.Value = slog.TimeValue(a.Value.Time().UTC())
		}
		return a
	}})).With("run_id", runID, "mode", mode, "app_version", version)
	report := cleaner.RunReport{Status: "complete", Decisions: []cleaner.Decision{}}
	logger.Info("run started", "event", "run_started")
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
		report.Status = "failed"
		report.Errors = 1
		logger.Error(message, "event", "configuration_failed")
		return finish(2)
	}
	if parseErr != nil || flags.NArg() != 0 {
		return invalid("invalid command-line arguments; use --help")
	}
	if *check && *apply {
		return invalid("--check and --apply are mutually exclusive")
	}
	if *apply {
		return invalid("apply is not implemented in the reporting stage")
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
