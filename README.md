# Arrtify

Declarative cleanup for your Arr stack.

Arrtify is a one-shot Go program that reads Transmission and the complete Sonarr
and Radarr queues, evaluates ordered cleanup rules, and emits a JSON dry-run
report.

This first implementation is **read-only**. It supports `--check` and dry-run;
`--apply` is explicitly rejected with exit 2. This version reports decisions only.

## Run

Build with Go 1.27.3 and `just build` (or `go build -buildvcs=false -o bin/arrtify ./cmd/arrtify`).
Copy [config.example.yml](config.example.yml) to `config.yml` and configure the
service URLs. Supply all four credentials through the environment:

- `TRANSMISSION_USER`
- `TRANSMISSION_PASSWORD`
- `SONARR_API_KEY`
- `RADARR_API_KEY`

```sh
bin/arrtify --config config.yml --check
bin/arrtify --config config.yml
```

`--check` validates YAML and required environment without network access. Ordinary
execution reads the configured APIs. stdout contains one final JSON report;
stderr contains JSON events with the same `run_id`, mode, and build version.
Exit 0 means complete; 2 means invalid CLI/configuration/environment; 1 means a
source, execution, cancellation, or report-output error. Forced termination can
prevent the final report.

## Policy

Category is the final component of Transmission's absolute `download_dir`,
ignoring case and trailing slashes. Transmission labels have no effect. The
first rule matching the category wins. Its condition is:

```text
(ratio >= ratio_limit AND seconds_seeding >= minimum) OR seconds_seeding >= maximum
```

The maximum branch exists only when configured. Ratio sentinels `-1` and `-2`
do not meet the ratio branch; a maximum may still qualify. Only fully downloaded
torrents with complete metadata in queued-seeding, seeding, or stopped/finished
states qualify. A hash in either Arr queue or an ignored tracker hostname
prevents qualification. Invalid or incomplete source data fails the run.

Decision fields distinguish observations, configured limits and comparison results:

| Field | Meaning |
| --- | --- |
| `upload_ratio` | Value reported by Transmission. |
| `upload_ratio_kind` | `finite`, `not_available` (-1), or `infinite` (-2); the latter two are protocol sentinels, not ordinary numbers used for the ratio comparison. |
| `seed_time_seconds` / `seed_time` | Measured seeding counter as seconds and a readable duration. |
| `criteria.min_upload_ratio` | Minimum upload ratio from the applied rule (`ratio` in YAML). |
| `criteria.min_seed_time` / `criteria.max_seed_time` | Configured minimum and optional maximum seeding durations. |
| `criteria.*_met` | Result of comparing the observation with the named limit. |
| `app_version` | Arrtify's build version, distinct from external service versions. |

A rule is satisfied when `(min_upload_ratio_met AND min_seed_time_met) OR
max_seed_time_met`. The reason is `rule_satisfied` or `rule_not_satisfied`,
describing the whole expression. Maximum fields are omitted when unconfigured;
`rule` and `criteria` are omitted when no rule was evaluated. Both sentinel ratio
kinds keep `min_upload_ratio_met=false`; the maximum can still qualify. Logs and
final reports use the same decision data.

`status` describes the run; `outcome` describes one torrent. `scanned` counts unique
torrents, `planned` counts initial candidates and `attempted` counts started remove
operations. The last two are auxiliary counters, not additional outcomes.
`errors` counts execution errors, while `failed` counts torrents with a failed
outcome. Final summary logs contain the same outcome counters as the report.

The example uses placeholder service addresses, an `.invalid` tracker hostname,
and illustrative category rules. Replace them with your own service URLs and policy.
Tests use only synthetic data and disposable local APIs.

## Development

`just ci` checks formatting, vet, race tests (including binary E2E), current Go
vulnerabilities, short fuzzing pilots, static build and the container smoke.
`just e2e` reruns binary scenarios and writes
[artifacts/e2e.json](artifacts/e2e.json). `just fuzz` runs short parser and
normalization fuzzing pilots. See [docs/development.md](docs/development.md) for
API contracts, limits, test decisions, and boundaries of the evidence.

GitHub Actions runs the same checks with read-only repository permissions. After
they pass on `main`, a separate job publishes `ghcr.io/zewelor/arrtify:latest` for
linux/amd64 and linux/arm64, stamps `app_version` with the source commit, and
reports its digest. Only that job receives package write permission. GitOps pins
the published digest. Renovate uses its recommended preset and pinned Action
updates; automerge is disabled.

## Container and cluster operation

`just image-build` builds the local `arrtify:test` image; `just container-test` builds
and exercises it against synthetic APIs. `just ci` includes these container checks
and requires a local Docker daemon. Ordinary `just test` remains Docker-free.
Container evidence goes to `artifacts/container-e2e.json`, separate from the binary
E2E report.

The runtime image is `scratch`, containing a static Go binary and trusted CA
certificates. It runs as UID/GID 65532 and needs no writable directory, shell,
service, database or media mount. For an offline configuration check with the
four credential environment variables already exported:

```sh
docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges --env TRANSMISSION_USER --env TRANSMISSION_PASSWORD --env SONARR_API_KEY --env RADARR_API_KEY --mount type=bind,src="$PWD/config.yml",dst=/config.yml,readonly arrtify:test --config /config.yml --check
```

Make the mounted configuration readable by the container user. Normal dry-run
needs network access to the configured APIs; the offline check does not.
For scheduled operation, run one invocation per Job or CronJob, mount configuration
read-only, and supply credentials through Secret references. Collect stdout and
stderr for reports and structured decisions; monitor both failed and missing runs.
Scheduling and deployment configuration belong to the operator.
