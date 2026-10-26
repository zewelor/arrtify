# Arrtify

Declarative cleanup for your Arr stack.

Arrtify is a one-shot Go program that reads Transmission and the complete Sonarr
and Radarr queues, evaluates ordered cleanup rules, and emits a JSON dry-run
report.

Cleanup execution is not implemented: torrent/file removal remains **read-only**.
The CLI has no `--apply` flag; execution is controlled by YAML. Dry-run is the
default. Environment `ARRTIFY_DRY_RUN=true` blocks every state-changing API call,
including labels. Actual label writes require both `ARRTIFY_DRY_RUN=false` and
`sync_tracker_label: true`; torrent and file removal is not implemented.

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

The configuration file is selected in this order: explicit `--config`,
`ARRTIFY_CONFIG_FILE`, then `config.yml` in the working directory. For example:

```sh
ARRTIFY_CONFIG_FILE=/config/config.yml bin/arrtify --check
```

An empty, missing or invalid selected file is an error; it does not fall back to
another file. An explicit flag takes precedence even when the environment value
is invalid. Relative paths are resolved from the working directory.

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

## Ignored-tracker label

Dry-run includes a `tracker_label` plan for every torrent. Select the feature
in YAML:

```yaml
sync_tracker_label: true
```

The global execution mode is controlled **only** by `ARRTIFY_DRY_RUN`, defaulting
to `true` when unset. It reads APIs and reports what would happen, but never
changes torrents, labels, categories, or files. The environment accepts exactly
`true` or `false`; empty or invalid values fail before network access. A YAML
`dry_run` key is rejected, so there is no second mode source or precedence rule.

`sync_tracker_label` independently selects the feature in YAML and defaults to
`false`. To actually synchronize labels:

```sh
ARRTIFY_DRY_RUN=false bin/arrtify --config config.yml
```

| `ARRTIFY_DRY_RUN` | YAML `sync_tracker_label` | Effect |
| --- | --- | --- |
| `true` or unset | `true` | Plan label changes, zero writes. |
| `true` or unset | `false` or omitted | Zero writes. |
| `false` | `true` | Synchronize the managed label; cleanup is still report-only. |
| `false` | `false` or omitted | No write feature is enabled in this version. |

Reports and start/finish logs expose the effective `dry_run` boolean and the
feature setting. The run mode is always `dry-run` or `apply`, controlled solely
by the environment; label synchronization is a feature, not a separate run
mode. In this version `apply` can write only labels; cleanup execution remains
unimplemented. `--check` remains offline regardless of these settings.

The label is present when any tracker matches `ignore_trackers`, using the same
hostname/subdomain matcher as cleanup policy. It is removed when no tracker
matches, including after the ignore list changes. This applies to all torrents,
regardless of queue protection, category, or download state. Other labels retain
their spelling and order. Labels never determine cleanup categories or rules.
`--check` validates the configuration offline, including when synchronization
is enabled, and never reads APIs or writes labels.

The run reads all three sources before any write. Each planned change refreshes
the torrent, recomputes the label, writes only that hash, and verifies the full
label list. Already correct labels cause no write. Missing or malformed tracker
or label data stops the run; a torrent deleted before its write is skipped.
`tracker_labels_planned` counts initial changes and `tracker_labels_changed`
counts confirmed changes. Each decision reports its label action and outcome
separately from the cleanup outcome. A failed write or readback is `unknown`,
sets the run to `partial`, and stops further writes without automatic retry.

Transmission replaces the entire label list and offers no atomic conditional
update. Fresh reads preserve current foreign labels and readback detects visible
divergence, but another label writer can still race between read and write.
Cleanuparr can coexist while Arrtify only manages this informational label;
check whether Cleanuparr's enabled workflows also write labels, and avoid
concurrent label changes. This label does not configure Cleanuparr exclusions.

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
