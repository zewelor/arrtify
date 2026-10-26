# Development and proof boundaries

These notes describe cleanup execution, optional tracker-label synchronization,
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

Apply E2E exercises multiple candidates with fresh queues, confirmed absence,
an uncertain second removal and an unattempted third candidate. Final logs,
outcomes, counters and exit codes must agree. It also verifies clean apply,
a newly protected hash, initial Arr failure in apply mode, and label readback
failure blocking every remove. HTTP runner tests cover fresh state/category/
tracker changes, missing torrents, bad reads, explicit pre-dispatch rejections,
uncertain responses, failed/still-present confirmations and cancellation.

Testing decisions:

- Adopt table-driven policy/config tests and real `httptest` boundaries for the
  decision and incomplete-read risks. No framework or source-text assertions.
- Pilot built-in Go fuzzing for strict YAML, category and hostname normalization
  with `just fuzz`. Seeds run in ordinary tests; the timed campaign is separate.
  Pilot the single-hash remove boundary with `FuzzRemoveTarget`: arbitrary empty,
  malformed or noncanonical hashes must never reach HTTP; valid hashes must be
  sent unchanged, alone, with delete-local-data enabled. Seeds run in normal tests
  and `just fuzz` adds a five-second campaign. This tests the new mutation guard
  without changing the existing pure policy or introducing a framework.
- Adopt deterministic HTTP/CLI state-sequence tests for label synchronization:
  stale label/trackers, missing torrents, preserved foreign labels, idempotence,
  uncertain writes and readback, and stopping subsequent writes. The same suite
  proves that unset/true global `ARRTIFY_DRY_RUN` blocks every write even with label
  sync enabled; the RPC boundary rejects all non-read methods in this mode.
  `--check` remains offline even when write features are configured. No additional stateful framework is
  needed. Extend the same HTTP/CLI approach to destructive apply: an earlier
  confirmed removal, then unknown, must stop all later writes; label failures
  must block cleanup. This covers changing external state at the real I/O boundary.
- Reassessed and skip a formal/TLA+ model for this sequential executor: it has no
  custom coordination protocol or persistent state. A model cannot make the three
  services atomic or eliminate external-writer races. Deterministic HTTP/CLI
  sequences provide the required observable failure evidence at lower cost;
  reassess if coordination or concurrency is introduced.

## Compatibility

Transmission uses JSON-RPC 2.0 `session_get` and `torrent_get`, named params and
snake_case fields from the [4.1.3 RPC specification](https://github.com/transmission/transmission/blob/4.1.3/docs/rpc-spec.md).
It requires an explicitly supported 4.1.x/RPC 6.0.x version and refuses unknown
versions. It requests every decision field, validates presence and values, and
allows unused additional API fields. A 409 handshake is the sole protocol retry.
Only environment `ARRTIFY_DRY_RUN=false` together with YAML `sync_tracker_label: true` permits
synchronization, using single-hash `torrent_set` with
only `ids` and `labels`. Apply also uses `torrent_remove` with exactly one canonical
hash and `delete_local_data: true`, even when label synchronization is disabled.
Only initially planned candidates are considered; each refreshes its torrent and
both complete queues and reuses `Evaluate`. A changed category is skipped even
if another rule could qualify it. Positive remove plus confirmed absence is
`removed`; this does not audit filesystem deletion. `labels` is a required
array on every torrent read, including targeted refreshes and confirmation.
Unknown write outcomes stop execution rather than trigger a blind retry. The
[Transmission 4.1.3 implementation](https://github.com/transmission/transmission/blob/4.1.3/libtransmission/rpcimpl.cc)
replaces the full label list, so read/write races with another label writer
remain possible and must not be described as atomic foreign-label preservation.

Only valid, matching-ID JSON-RPC error envelopes with standard pre-dispatch codes
`-32600`, `-32601` or `-32602` classify a remove as definitely rejected/failed.
HTTP failures, malformed/conflicting responses, internal/application RPC errors,
and interrupted or failed confirmations are unknown. This conservative distinction
does not infer safety from a later absence read or silently retry a remove.

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

The isolated Transmission RPC/hardlink proof is recorded below. Verify the actual
Arrtify executor in the same lab and review destructive RPC and partial-failure
behavior independently before activation. Publishing an image does not activate
cleanup in the operator's environment.

## Isolated Transmission proof

[The manual lab](transmission-lab.md) uses a small Bash script and a pinned real
Transmission container. `just transmission-up` starts it; `just transmission-down`
stops it. The guide covers a legal public download, the current CLI with existing
synthetic Arr fixtures, and remove+delete-data with hardlink verification. Policy
and failure-sequence coverage stays in the existing Go unit/HTTP/E2E tests.

The one-off proof on 2026-10-26 downloaded Sintel (129,302,391 bytes, 11 files),
removed its download paths and verified unchanged SHA-256 for all library
hardlinks. The report remains in `artifacts/transmission-public-proof.json`.
The exploratory Go harness was removed; it is not a maintained test suite.
The current executor's acceptance and review are separate from that exploratory
proof. Local hardlinks do not prove NAS/Storage Box behavior or authorize
production activation.

Executor acceptance on 2026-10-26: the current CLI, using the same lab YAML,
first reported one dry-run candidate with zero writes, then with
`ARRTIFY_DRY_RUN=false` and label synchronization disabled confirmed one removal
and zero errors. All 11 download paths disappeared and all 11 library hardlinks
retained their SHA-256. Local evidence is `artifacts/executor-transmission-proof.json`.
An independent read-only review of destructive RPC and partial failures found no
actionable defects. A combined label-success/cleanup-unknown CLI regression case
also verifies retained label reporting and no later removal. Production activation
and live NAS/Storage Box behavior remain separate proof boundaries.

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
