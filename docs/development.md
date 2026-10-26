# Development and proof boundaries

These notes describe cleanup reporting, optional tracker-label synchronization,
verification and proof boundaries.
The module uses the installed Go 1.27.3 toolchain, standard
library boundaries, and `go.yaml.in/yaml/v3` v3.0.5. `govulncheck` is pinned through
Go's module tool mechanism. There are no other runtime dependencies.

## Verification

Blocking local checks are `just ci`: formatting, `go vet ./...`,
`go test -race ./...`, bounded fuzzing pilots, `go tool govulncheck ./...`, static CLI build, and the actual scratch-image smoke. The race
suite includes the hermetic E2E package, which builds and exercises the actual
binary and writes `artifacts/e2e.json` with exit codes, counters and reports. Scenario cleanup records failures too:
`passed: false` remains in the artifact even after a fatal assertion; `exit: -1`
means no process exit result was collected. A regression check runs a deliberately
failing CLI scenario in a disposable source copy and inspects its artifact.
Loopback sockets and access to the Go vulnerability database are needed for
these checks; a sandbox denying either is a verification limitation, not a pass.

E2E verifies the explicit observation/limit names, threshold explanations and their consistency between JSON logs
and reports, including an unmet ratio after minimum seed time. It also covers both full Arr queues (including an uppercase hash on page 2), ignored
trackers, the maximum branch with a sentinel ratio, invalid source data, required
credentials, configuration-file precedence (`--config`, `ARRTIFY_CONFIG_FILE`,
default working-directory file), invalid/empty path handling without fallback,
offline `--check`, rejected apply/conflicting flags, redaction, and
SIGTERM during I/O. Unit/HTTP tests cover strict config, threshold boundaries,
first-rule priority, missing fields, pagination errors, protocol handshakes,
redirect rejection, bounded reads and cancellation. Test servers and files are
disposable and contain no production data. API fixtures describe documented
shapes; they do not prove compatibility with the operator's live services.

Testing decisions:

- Adopt table-driven policy/config tests and real `httptest` boundaries for the
  decision and incomplete-read risks. No framework or source-text assertions.
- Pilot built-in Go fuzzing for strict YAML, category and hostname normalization
  with `just fuzz`. Seeds run in ordinary tests; the timed campaign is separate.
  Add guard-invariant fuzzing when the mutation path is implemented.
- Adopt deterministic HTTP/CLI state-sequence tests for label synchronization:
  stale label/trackers, missing torrents, preserved foreign labels, idempotence,
  uncertain writes and readback, and stopping subsequent writes. The same suite
  proves that unset/true global `ARRTIFY_DRY_RUN` blocks every write even with label
  sync enabled; the RPC boundary rejects all non-read methods in this mode.
  `--check` remains offline even when write features are configured. No additional stateful framework is
  needed; extend these scenarios for later destructive apply.
- Skip a formal/TLA+ model for this bounded sequential metadata operation.
  Transmission has no compare-and-swap: no client-side model can eliminate the
  concurrent-writer race. Use fresh reads, readback, explicit uncertainty and
  deterministic failure tests; reassess for destructive apply or coordination.

## Compatibility

Transmission uses JSON-RPC 2.0 `session_get` and `torrent_get`, named params and
snake_case fields from the [4.1.3 RPC specification](https://github.com/transmission/transmission/blob/4.1.3/docs/rpc-spec.md).
It requires an explicitly supported 4.1.x/RPC 6.0.x version and refuses unknown
versions. It requests every decision field, validates presence and values, and
allows unused additional API fields. A 409 handshake is the sole protocol retry.
Only environment `ARRTIFY_DRY_RUN=false` together with YAML `sync_tracker_label: true` permits
synchronization, using single-hash `torrent_set` with
only `ids` and `labels`. It never calls `torrent_remove`. `labels` is a required
array on every torrent read, including targeted refreshes and confirmation.
Unknown write outcomes stop execution rather than trigger a blind retry. The
[Transmission 4.1.3 implementation](https://github.com/transmission/transmission/blob/4.1.3/libtransmission/rpcimpl.cc)
replaces the full label list, so read/write races with another label writer
remain possible and must not be described as atomic foreign-label preservation.

Sonarr/Radarr use `GET /api/v3/queue` with explicit unknown-item flags, all statuses
and full pagination. Empty/null downloadId is allowed; missing downloadId is an
error. Distinct records may carry the same hash; duplicate record IDs and changed
pagination metadata are errors. Complete pagination is not an atomic snapshot.
Sources: [Sonarr v3 queue](https://github.com/Sonarr/Sonarr/blob/v5-develop/src/Sonarr.Api.V3/Queue/QueueController.cs),
[Radarr v3 queue](https://github.com/Radarr/Radarr/blob/develop/src/Radarr.Api.V3/Queue/QueueController.cs).

Service URL paths may include a reverse-proxy prefix. Transmission URLs end in
`/transmission/rpc`; Arr URLs are service base URLs. Credentials, queries and
fragments are prohibited in configured URLs. YAML accepts exactly one document,
rejects unknown/duplicate fields, and caps input at 1 MiB. Durations require units;
ratio and minimum must be explicit, finite/nonnegative; a maximum is positive.

HTTP requests have a 10-second timeout, the process a 270-second deadline. Responses
are bounded at 16 MiB; queue reads at 100,000 records with page size 100. Exceeding
a limit is an error. TLS uses normal verification; redirects and domain retries
are disabled. Logs omit raw HTTP responses, URLs and transport errors that could
contain credentials.

## Remaining proof

The synthetic APIs prove policy and wire handling. Compatibility with deployed
services, image publication, log ingestion, Job monitoring and resource budgets
must be verified separately in the target environment. Do not publish captured
service responses or operational reports as test fixtures.

Before implementing cleanup, prove real Transmission remove+delete-data and
hardlink preservation using isolated services and synthetic files. Review
destructive RPC and partial-failure behavior independently before activation.

Source builds use `-buildvcs=false` and identify themselves as `dev`. Published
container builds stamp their source commit through the Dockerfile VERSION
argument. A separate trusted Actions job publishes only after the complete
local/CI gate. Native tests and an image cross-build do not prove target-cluster
execution.

## Local Go skills

The project includes four copied skills from
[samber/cc-skills-golang](https://github.com/samber/cc-skills-golang): code style
for readable Go, error handling for HTTP/CLI failures and redaction, context for
bounded I/O and cancellation, and testing for the existing policy/API/E2E suite.
They are project-scoped under `.agents/skills`, installed using `--agent codex
--copy`; compatible agents may discover that shared directory too.
`skills-lock.json` records their source paths and content hashes.

These are authoring instructions, not application or CI dependencies. Keep the
existing standard-library-first contract and default hermetic tests when community
recommendations differ. Do not install referenced tools or companion skills merely
because a skill mentions them. The copied guidance becomes available to Codex on
the next turn; its selection and reference integrity were checked statically.

Before updating a skill, inspect local divergence, then use
`npx skills update <skill-name> -p -y`. Review the changed instructions and
`skills-lock.json`, resolve project-contract conflicts, and verify discovery with
`npx skills list --json`. Keep changes to the skill's own content in its source
repository; avoid an untracked local fork.

## Container verification

The native image smoke reuses the binary E2E fixture and output assertions. It
compiles a static fixture helper and starts it on a disposable internal Docker
network, so rootless Docker does not need access to host loopback servers. The
application container runs non-root with read-only root, all capabilities dropped,
no-new-privileges and a read-only config mount. Scenarios cover offline `--check`
with networking disabled, a nontrivial dry-run with both queue guards and
`would_delete` candidates, and a failed Arr queue. Networked container scenarios
select the configuration through `ARRTIFY_CONFIG_FILE` without a CLI override.
The fixture records actual request/mutation counts; all scenarios must have zero
mutations. Containers and
the network are removed after testing. No Compose stack or production credentials
are involved.

The runtime includes the builder's CA bundle. Container DNS and API requests are
exercised by the synthetic dry-run; the synthetic fixture uses HTTP. An
HTTPS handshake using the image's system trust has not been separately exercised.
The Dockerfile cross-compiles the second architecture without QEMU; a successful
cross-build does not imply native execution on that architecture.
