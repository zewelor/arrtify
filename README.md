# Arrtify

[![CI](https://github.com/zewelor/arrtify/actions/workflows/ci.yml/badge.svg)](https://github.com/zewelor/arrtify/actions/workflows/ci.yml)

**Declarative cleanup for your Arr stack.**

Arrtify removes completed torrents and their data from Transmission using
category, ratio and seeding-time rules. Torrents in either Sonarr or Radarr
queue are protected.

- Dry-run by default, with every write blocked.
- Tracker exclusions and optional `arrtify:ignored-tracker` label synchronization.
- JSON reports and structured logs for each run.
- A single process per run, ready for cron or Kubernetes CronJobs. No database or media mounts.

Requires Transmission 4.1.x; uses the Sonarr and Radarr v3 queue APIs.

## Quick start

Copy [config.example.yml](config.example.yml) and edit the service URLs and rules:

```sh
cp config.example.yml config.yml
```

Export the required credentials in your environment:

| Variable | Purpose |
| --- | --- |
| `TRANSMISSION_USER` | Transmission RPC username |
| `TRANSMISSION_PASSWORD` | Transmission RPC password |
| `SONARR_API_KEY` | Sonarr API key |
| `RADARR_API_KEY` | Radarr API key |

Run a preview with the published image, available for linux/amd64 and linux/arm64:

```sh
docker run --rm --read-only \
  --env ARRTIFY_DRY_RUN=true \
  --env TRANSMISSION_USER --env TRANSMISSION_PASSWORD \
  --env SONARR_API_KEY --env RADARR_API_KEY \
  --mount type=bind,src="$PWD/config.yml",dst=/config.yml,readonly \
  ghcr.io/zewelor/arrtify:latest --config /config.yml
```

The configuration must be readable by container user `65532`.
Add `--check` for offline validation of YAML and required environment variables.

To execute cleanup, change `ARRTIFY_DRY_RUN=true` to `ARRTIFY_DRY_RUN=false`.
**This removes qualifying torrents and their download data.** The environment is
the only mode switch; there is no `--apply` flag or YAML `dry_run` setting.

## Rules

The first rule matching the download-directory category wins:

```text
(ratio >= limit AND seeding time >= minimum) OR seeding time >= maximum
```

The maximum is optional. Categories come from the final component of
Transmission's `download_dir`, never from labels. Queue protection and tracker
exclusions take precedence over cleanup rules.

## Safety

Before each removal, Arrtify refreshes the torrent and both complete Arr queues.
It removes one hash at a time and confirms that the torrent is absent afterward.
Invalid reads, failed operations and uncertain results stop further writes;
confirmed earlier changes remain in the report.

Run a single cleanup owner and avoid concurrent category or label changes.
Arrtify relies on Transmission for file deletion and does not inspect the media filesystem.

## Documentation and development

- [Configuration, execution and report reference](docs/reference.md)
- [Development, compatibility and verification](docs/development.md)
- [Isolated Transmission lab](docs/transmission-lab.md)

```sh
just build    # Build bin/arrtify
just ci       # Run the full local and CI checks
```

Development uses the Go toolchain declared in [go.mod](go.mod), plus Just.
The full CI gate also requires Docker.
