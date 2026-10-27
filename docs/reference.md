# Configuration and operation reference

For installation and a first run, see the [README](../README.md#quick-start).
The complete YAML example is [config.example.yml](../config.example.yml).

## CLI and configuration

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
ignoring case and trailing slashes. Transmission labels do not select cleanup rules. The
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

## Cleanup execution

```sh
ARRTIFY_DRY_RUN=false bin/arrtify --config config.yml
```

Apply first builds a complete plan, optionally synchronizes tracker labels, then
processes the initial cleanup candidates sequentially. A label synchronization
error prevents every removal. Before each remove, it refreshes that torrent and
both complete Arr queues and reevaluates the same policy. A missing torrent,
changed category, or newly failing guard is skipped. New candidates discovered
during these reads wait for the next run.

Each remove targets one stable hash and requests `delete_local_data: true`.
`removed` requires a positive response and a subsequent read confirming the hash
is absent. This confirms the Transmission entry, not the filesystem; Arrtify
never accesses media paths directly. Local hardlink behavior is verified in the
[manual lab](transmission-lab.md).

Read/validation failures and explicit pre-dispatch RPC rejections are `failed`.
An uncertain remove response, cancellation during remove, failed confirmation or
still-present hash is `unknown`. Either result stops further mutations without
retry; remaining candidates are `not_attempted`. Earlier confirmed removals and
label changes stay in the report. Errors after confirmed or possible writes give
`partial` and exit 1; an error before any such effect gives `failed` and exit 1.
`would_delete` occurs only in dry-run. Final outcome counts account for every
initially scanned torrent, while `planned` retains the original candidate count.

Fresh reads narrow the race window; the three services do not offer one atomic
transaction. Run one cleanup owner and avoid concurrent category/path changes.
When replacing another cleanup tool, durably disable and stop it before
enabling Arrtify writes. Publishing an image does not activate cleanup.

## Import reports

Enable optional, read-only reporting with the post-import category configured
in each Arr application's Transmission client:

```yaml
sonarr:
  url: http://sonarr.example.invalid:8989
  imported_label: sonarr-imported
radarr:
  url: http://radarr.example.invalid:7878
  imported_label: radarr-imported
```

The label names above are examples. Each service's `imported_label` is independent;
omitting it or setting it to an empty string disables that service's report.
Labels match case-insensitively. Values must be strings without surrounding
whitespace or control characters. Configured names appear as
`sonarr_imported_label` / `radarr_imported_label` in run reports and logs.
Transmission's download-directory category still selects cleanup rules.

At startup, including offline `--check`, an `INFO` event named `import_reporting`
states whether reporting is enabled for each service. `enabled`, `label`, and
`setting` identify the configured state and the relevant YAML setting. A disabled
report explains that `imported_label` is not configured; this is informational,
not a warning or an execution-mode change. Existing cleanup rules still apply.

Only torrents carrying the configured label receive a `sonarr_import` or
`radarr_import` decision object in JSON and logs. If none match, no extra API
requests are made for that service. Arrtify matches `downloadFolderImported`
history events by exact torrent hash, ignoring hash case. It never guesses from
titles, filenames or paths.

Radarr compares the imported `data.fileId` with the movie's current `movieFileId`.
Sonarr reads current episodes for each referenced series once and compares the
imported file IDs with `episodeFileId`. Identical import references are deduplicated;
multiple episodes sharing one file are supported.

| State | Radarr | Sonarr |
| --- | --- | --- |
| `current` | The movie still references the imported file ID. | At least one known imported episode still references its imported file ID. |
| `replaced` | The movie references a different, nonzero file ID. | Every known imported episode references a different, nonzero file ID. |
| `unknown` | History or the current movie/file is absent, or import references conflict. | History or required episode/file references are absent, or one episode has conflicting import references. |

The `reason` field explains the result: both services use `no_import_history` and
`ambiguous_import_history`; Radarr uses `current_file`, `different_current_file`,
`movie_missing`, and `movie_file_missing`. Sonarr uses `current_episode_file`,
`different_current_files`, `episode_missing`, and `episode_file_missing`.
Conflicting imports take precedence over other states. Otherwise, a current
Sonarr reference keeps `current` even when another known episode is missing;
without one, a missing episode/file yields `unknown` rather than `replaced`.

An unambiguous Radarr import includes its `movie_id` and `file_id`. Sonarr includes
`imported_episodes`, the number of distinct episodes in the matching history.
That count does not prove the complete contents of a season pack were imported
or remain represented in retained history. Deleting a movie/series also removes
its history; an imported label can legitimately have no matching history.
Neither report proves physical hardlink state or tracks an independent copy.

These fields describe initial planning reads and do not change cleanup
eligibility or thresholds. `unknown` does not protect a torrent from an existing
category rule, and `current` does not promise retention. Cleanup `outcome` and
its `unknown` counter remain separate. Failed or malformed enabled API reads
fail the run before writes; ordinary absence or ambiguity yields informational
`unknown`. Dry-run blocks every write, and `--check` stays offline.

## Ignored-tracker label

Dry-run includes a `tracker_label` plan for every torrent. Select the feature
in YAML:

```yaml
sync_tracker_label: true
```

The global execution mode is controlled **only** by `ARRTIFY_DRY_RUN`, defaulting
to `true` when unset. In dry-run mode, Arrtify reads APIs and reports what would
happen without changing torrents, labels, categories, or files. The environment accepts exactly
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
| `false` | `true` | Synchronize the managed label, then execute cleanup rules. |
| `false` | `false` or omitted | Execute cleanup rules without label synchronization. |

Reports and start/finish logs expose the effective `dry_run` boolean and the
feature setting. The run mode is always `dry-run` or `apply`, controlled solely
by the environment; label synchronization is a feature, not a separate run
mode. `--check` remains offline regardless of these settings.

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
Avoid concurrent label changes by operators or other tools. The managed label
is informational; protection comes from the configured tracker matcher.

## Build and publication

`just ci` checks formatting, vet, race tests (including binary E2E), current Go
vulnerabilities, short fuzzing pilots, static build and the container smoke.
`just e2e` reruns binary scenarios and writes
`artifacts/e2e.json`. `just fuzz` runs short parser,
normalization and remove-target fuzzing pilots. See [development.md](development.md) for
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
