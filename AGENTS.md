# Arrtify

Arrtify is a one-shot Go cleanup tool for Transmission with Sonarr/Radarr queue
protection. [README.md](README.md) and `config.example.yml` describe the public
surface; [docs/development.md](docs/development.md) records test commands and
proof boundaries. Read them before changing policy, public configuration,
API compatibility or activation.

## Code boundaries

- Keep the CLI in `cmd/arrtify` and the deterministic policy plus explicit I/O
  boundaries in `internal/cleaner`. Prefer the standard library; YAML is the
  sole runtime dependency. Configuration types and `config.example.yml` define
  the implemented configuration; do not add a parallel set of env/flag thresholds.
- Category comes from the final DownloadDir component, never Transmission labels.
  Arr protects every nonempty downloadId in both complete queues, in every status.
  Missing or invalid decision data is an error, never an empty queue or a zero.
- Dry-run is the default. Only YAML `sync_tracker_label: true` enables metadata writes: manage
  `arrtify:ignored-tracker`, refresh before each single-hash write, preserve other
  labels, confirm readback, and stop on uncertain results without retry.
  Future apply must use a single hash, fresh torrent and both queue reads before
  each remove, and stop on errors or uncertain effects. Keep one policy and one
  decision model shared by reporting and execution.
- Do not log credentials, raw HTTP bodies/errors, or tracker URLs. Log safe
  service/operation context and structured decisions. HTTP has bounded reads,
  deadlines, no redirects, and no domain retries beyond Transmission's 409 handshake.

## Development and delivery

`just ci` is the local/CI gate; `just e2e` runs the built CLI against disposable
loopback services and writes `artifacts/e2e.json`. Tests have no production
access; run and fix them autonomously within the requested change.

Use Red/Green for new behavior and meaningful failure cases. Reassess Go fuzzing,
stateful/model-based testing, and formal methods before substantial public-contract,
state-transition, or concurrency changes; record adopt/pilot/skip and benefit/cost
in [docs/development.md](docs/development.md).

Local implementation does not activate production cleanup. Remote repository
creation, publication, GitOps changes, live apply, and retirement of an existing cleanup executor
need delivery scope from the operator. Keep synthetic API tests, real isolated
Transmission proof, live read-only compatibility, and production activation as
separate evidence. Before activation, complete the independent read-only review
of destructive RPC and partial-failure behavior.

## Local Go skills

Selected guidance lives in `.agents/skills/`:

- [golang-code-style](.agents/skills/golang-code-style/SKILL.md): Go clarity and focused refactoring.
- [golang-error-handling](.agents/skills/golang-error-handling/SKILL.md): error propagation, safe context and logging.
- [golang-context](.agents/skills/golang-context/SKILL.md): deadlines, cancellation and context ownership.
- [golang-testing](.agents/skills/golang-testing/SKILL.md): behavior tests, fixtures and fuzzing.

Apply the relevant skill when its task arises. Project contracts take precedence
over community defaults: retain meaningful missing/null distinctions in API
types, keep hermetic HTTP/E2E tests in the default suite, and preserve error
redaction. Use the existing standard-library tests; tools such as `gotests`,
new libraries, and companion skills are not implicit dependencies.

See [docs/development.md](docs/development.md#local-go-skills) for source and update policy.
