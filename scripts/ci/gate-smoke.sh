#!/usr/bin/env bash
set -euo pipefail
image=${1:?expected image tag}
tmp=$(mktemp -d)
container=
cleanup() {
    if [[ -n $container ]]; then docker rm -f "$container" >/dev/null; fi
    rm -rf -- "$tmp" 2>/dev/null || true
}
trap cleanup EXIT
read_token=demo-reader
printf '%s\n' "$read_token" > "$tmp/read"
printf '%s\n' demo-admin > "$tmp/admin"
# Directory is private to the job; bind-mounted files must be readable by 10008.
chmod 0444 "$tmp/read" "$tmp/admin"
# The installation mounts the data directory as a host bind (RW), never a named volume:
# the governed module release reuses the bind mounts of the running predecessor.
mkdir "$tmp/data"
chmod 0777 "$tmp/data"
gate='OUF_APPLICATION_GATE=0; trap "OUF_APPLICATION_GATE=1" USR1; while [ "$OUF_APPLICATION_GATE" = 0 ]; do sleep 1 & wait "$!"; done; exec "$@"'
container=$(docker run -d --read-only --cap-drop ALL --security-opt no-new-privileges \
    -p 127.0.0.1::8080 --mount "type=bind,src=$tmp/data,dst=/var/lib/ouf-vertical-mock" \
    --mount "type=bind,src=$tmp/read,dst=/run/mock-read,readonly" \
    --mount "type=bind,src=$tmp/admin,dst=/run/mock-admin,readonly" \
    -e OUF_MOCK_DATA_DIR=/var/lib/ouf-vertical-mock \
    -e OUF_MOCK_DOMAIN_FILE=/usr/share/ouf-vertical-mock/examples/domain-lab.json \
    -e OUF_MOCK_READ_TOKEN_FILE=/run/mock-read -e OUF_MOCK_ADMIN_TOKEN_FILE=/run/mock-admin \
    --entrypoint /bin/sh "$image" -c "$gate" ouf-gated-application /usr/local/bin/ouf-vertical-mock serve)
base="http://$(docker port "$container" 8080/tcp)"
for _ in 1 2 3 4; do
    if curl -s --max-time 1 -o /dev/null "$base/health/ready"; then
        echo "HTTP answered before USR1" >&2
        exit 1
    fi
    sleep 1
done
echo "Gate held for at least three seconds"
docker kill --signal USR1 "$container" >/dev/null
deadline=$((SECONDS + 30))
while [[ $(curl -s --max-time 1 -o /dev/null -w '%{http_code}' "$base/health/ready" || true) != 204 ]]; do
    if (( SECONDS >= deadline )); then docker logs "$container"; exit 1; fi
    sleep 0.2
done
(( SECONDS <= deadline ))
echo "Readiness returned 204 after USR1"
[[ $(curl -sS --max-time 5 -o /dev/null -D "$tmp/headers" -H "Authorization: Bearer $read_token" -w '%{http_code}' "$base/collections") == 200 ]]
grep -Eiq '^X-Schema-Version: [a-f0-9]{64}[[:space:]]*$' "$tmp/headers"
[[ $(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "$base/collections") == 401 ]]
docker stop --time 10 "$container" >/dev/null
[[ $(docker inspect -f '{{.State.ExitCode}}' "$container") == 0 ]]
echo "Catalog authentication, schema header and graceful shutdown passed"
