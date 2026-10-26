# Manual Transmission lab

Run from the repository root with local rootless Docker, Go, just and jq. The script uses a pinned
Transmission 4.1.3 image, bind-mounts only `artifacts/transmission-lab/{config,downloads}`
and exposes RPC at `127.0.0.1:19091`. Namespace UID 0 maps to the invoking host
user, so temporary files stay user-owned. AppArmor uses its existing allowed
paths. The container has internet access for public downloads; DHT, PEX, local
discovery and port forwarding are disabled. Library files stay outside its mounts.

## Start and download

```sh
just transmission-up
bash scripts/transmission-lab.sh remote --add https://webtorrent.io/torrents/sintel.torrent
bash scripts/transmission-lab.sh remote --list
```

[Sintel is a legal Creative Commons test torrent](https://webtorrent.io/free-torrents),
about 123 MiB across 11 files. Wait for 100% completion before the following steps.
Use the hash to select this torrent, including for later removal:

```sh
hash=08ada5a7a6183aae1e09d831df6748d566095a10
bash scripts/transmission-lab.sh remote --torrent "$hash" --info
```

## Run Arrtify against the real client

Reuse the existing E2E fixture for both Arr queues. In a separate terminal:

```sh
go test -c -o artifacts/transmission-lab/arr-fixture ./tests/e2e
ARRTIFY_E2E_FIXTURE_SERVER=1 artifacts/transmission-lab/arr-fixture
```

In the original terminal, create the lab configuration and run the real CLI:

```sh
cat > artifacts/transmission-lab/arrtify.yml <<'YAML'
transmission:
  url: http://127.0.0.1:19091/transmission/rpc
sonarr:
  url: http://127.0.0.1:18080/sonarr
radarr:
  url: http://127.0.0.1:18080/radarr
ignore_trackers: []
rules:
  - name: lab
    categories: [sonarr]
    ratio: 0
    min_seed_time: 0s
YAML
just build
ARRTIFY_DRY_RUN=true TRANSMISSION_USER=test TRANSMISSION_PASSWORD=test \
  SONARR_API_KEY=api-secret-canary RADARR_API_KEY=api-secret-canary \
  bin/arrtify --config artifacts/transmission-lab/arrtify.yml
```

The completed torrent should have outcome `would_delete`; data stays present.
The Arr services here are synthetic, and all credentials above are fixture values.
Protection, pagination, invalid reads and partial failures remain covered by
`just e2e` and the existing HTTP/unit tests. The next section enables the real
Arrtify executor against this lab client.

## Remove data and verify the library hardlink

```sh
(
    set -e
    lab="$PWD/artifacts/transmission-lab"
    source="$lab/downloads/sonarr/Sintel/Sintel.mp4"
    library="$lab/library/Sintel.mp4"
    ln "$source" "$library"
    stat -c '%d:%i %h %n' "$source" "$library"
    sha256sum "$library" > "$lab/library.sha256"
    ARRTIFY_DRY_RUN=false TRANSMISSION_USER=test TRANSMISSION_PASSWORD=test \
      SONARR_API_KEY=api-secret-canary RADARR_API_KEY=api-secret-canary \
      bin/arrtify --config "$lab/arrtify.yml" > "$lab/apply.json"
    test ! -e "$source" || {
        echo "Download still exists: $source" >&2
        exit 1
    }
    sha256sum --check "$lab/library.sha256"
    jq -e '.status == "complete" and .removed == 1 and .errors == 0' "$lab/apply.json"
)
```

The subshell stops on the first error without changing the interactive shell's
options or allowing a later checksum to hide an earlier failure.
Before removal both names must have the same device/inode; afterwards the download
path must be absent, the library checksum must pass, and the report must confirm
one removal. The YAML rules remain identical between dry-run and apply; only the
environment mode changes. These commands explicitly use the lab configuration
and synthetic credentials.
Use fresh test files for each case; an existing library link makes `ln` fail.
This proves local filesystem behavior, not NAS/Storage Box behavior.

## Stop

Stop the Arr fixture with Ctrl+C in its terminal, then run:

```sh
just transmission-down
```

The container uses Docker's `--init` so Docker can stop it independently of RPC.
Shutdown uses RPC first, then `docker stop` if RPC fails. A failed Docker stop
aborts cleanup and remains visible. The container is removed only after stopping.
Its lab data and checksums remain under `artifacts/` for inspection; the script
does not delete them.
