#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
lab="$PWD/artifacts/transmission-lab"
container=arrtify-transmission-lab
image=ghcr.io/home-operations/transmission:4.1.3@sha256:236ef4efe46905314ad9471ffb2e102c7b30150bb3e39f258b0a49f912c3359c
config=/var/lib/transmission-daemon/.config/transmission-daemon
downloads=/var/lib/transmission-daemon/downloads
action=${1:-}
if (($#)); then shift; fi

remote() {
    docker exec --workdir / "$container" transmission-remote 127.0.0.1:9091 "$@"
}

case "$action" in
    up)
        # Namespace UID 0 maps to the host user; bind mounts stay user-owned.
        [[ $(docker info --format '{{json .SecurityOptions}}') == *name=rootless* ]] || {
            echo 'This lab requires rootless Docker.' >&2; exit 1;
        }
        endpoint=${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}
        [[ $endpoint == unix://* ]] || { echo 'Use local Docker.' >&2; exit 1; }
        if docker container inspect "$container" >/dev/null 2>&1; then
            echo 'Lab container already exists; use transmission-down first.' >&2
            exit 1
        fi
        mkdir -p "$lab/config" "$lab/downloads/sonarr" "$lab/library"
        # Match the host distribution's AppArmor paths without changing its profile.
        cat > "$lab/config/settings.json" <<EOF
{
  "download_dir": "$downloads/sonarr",
  "rpc_enabled": true,
  "rpc_bind_address": "0.0.0.0",
  "rpc_authentication_required": false,
  "rpc_whitelist_enabled": false,
  "rpc_host_whitelist_enabled": false,
  "dht_enabled": false,
  "pex_enabled": false,
  "lpd_enabled": false,
  "port_forwarding_enabled": false,
  "ratio_limit_enabled": false,
  "cache_size_mib": 0
}
EOF
        docker run --detach --init --name "$container" --label io.arrtify.lab=true \
            --network bridge --publish 127.0.0.1:19091:9091 \
            --user 0:0 --workdir / --read-only --cap-drop ALL \
            --security-opt no-new-privileges \
            --mount "type=bind,src=$lab/config,dst=$config" \
            --mount "type=bind,src=$lab/downloads,dst=$downloads" \
            --entrypoint /usr/bin/transmission-daemon "$image" \
            --foreground --config-dir "$config" --log-level error
        for ((attempt=0; attempt<30; attempt++)); do
            if remote --session-info >/dev/null 2>&1; then
                echo "RPC: http://127.0.0.1:19091/transmission/rpc"
                echo "Data: $lab"
                exit 0
            fi
            running=$(docker inspect --format '{{.State.Running}}' "$container")
            if [[ $running != true ]]; then
                echo 'Transmission exited before RPC became ready.' >&2
                docker logs "$container" >&2
                exit 1
            fi
            sleep 1
        done
        echo 'Transmission did not become ready; inspect docker logs and run transmission-down.' >&2
        exit 1
        ;;
    remote|down)
        [[ $(docker inspect --format '{{index .Config.Labels "io.arrtify.lab"}}' "$container") == true ]] || {
            echo 'Container is not this lab.' >&2; exit 1;
        }
        if [[ $action == remote ]]; then
            remote "$@"
        else
            if [[ $(docker inspect --format '{{.State.Running}}' "$container") == true ]]; then
                if remote --exit; then
                    timeout 55s docker wait "$container" >/dev/null
                else
                    echo 'RPC shutdown failed; stopping the lab container through Docker.' >&2
                    docker stop --timeout 45 "$container"
                fi
            fi
            docker rm --volumes "$container"
            echo "Lab stopped; data retained in $lab"
        fi
        ;;
    *)
        echo "Usage: $0 up|down|remote [transmission-remote arguments]" >&2
        exit 2
        ;;
esac
