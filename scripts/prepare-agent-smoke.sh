#!/bin/sh
# One-time reference prerequisites; no operator or Dynamo application is executed.
set -eu
cd "$(dirname "$0")/.."
revision=cd90bd82a2d692fbf63ba0231699e2e3dc0b6a08
expected=294516d657d3c00d9be8f83561cc4c06142af3b4de1badd37da21b1fed56d2ae
prefix=${DYNAMO_SMOKE_CACHE_PREFIX:-dynamo-go}
case "$prefix" in ''|*[!a-zA-Z0-9_.-]*) echo 'Invalid cache prefix' >&2; exit 2;; esac
python3 -c 'import importlib.util; s=importlib.util.spec_from_file_location("smoke","scripts/agent-smoke.py"); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); m.validate_profile()'
evidence=${DYNAMO_SMOKE_SETUP_EVIDENCE:-$PWD/evidence/smoke-setup}
mkdir -p "$evidence"
setup=$(mktemp -d "$evidence/setup-XXXXXX")
start=$(date +%s)
clean=
if [ "${DYNAMO_SMOKE_CLEAN_BUILD:-0}" = 1 ]; then clean=--no-cache; fi
container=
helper="dynamo-smoke-setup-$(basename "$setup")"
cleanup() { if [ -n "$container" ]; then docker rm -f "$container" >/dev/null 2>&1 || true; fi; docker rm -f "$helper" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
curl --max-time 120 -fsSL "https://api.github.com/repos/kubernetes-sigs/kubebuilder/tarball/$revision" -o "$setup/upstream.tar.gz"
mkdir "$setup/upstream"
tar -xzf "$setup/upstream.tar.gz" -C "$setup/upstream" --strip-components=1
project="$setup/upstream/docs/book/src/cronjob-tutorial/testdata/project"
printf '%s\n' "$revision" > "$setup/revision.txt"
sha256sum "$setup/upstream.tar.gz" > "$setup/archive.sha256"
cmp "$project/config/crd/bases/batch.tutorial.kubebuilder.io_cronjobs.yaml" examples/agent-smoke/resources/cronjob.yaml
(cd "$project" && find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$setup/source.sha256"
timeout 600 docker build $clean --secret id=proxy_ca,src=/etc/ssl/certs/ca-certificates.crt -f test/fixture/Dockerfile -t dynamo-fixture:agent-smoke "$project" > "$setup/build.log" 2>&1
image=$(docker image inspect dynamo-fixture:agent-smoke --format '{{.Id}}')
printf '%s\n' "$image" > "$setup/image.txt"
container=$(docker create "$image")
staged=$(mktemp "$PWD/examples/agent-smoke/operator-src/.operator-XXXXXX")
docker cp "$container:/operator" "$staged"
actual=$(sha256sum "$staged" | cut -d ' ' -f1)
[ "$actual" = "$expected" ] || { rm -f "$staged"; echo 'Reference binary differs; investigate before using it' >&2; exit 3; }
chmod 755 "$staged"
mv "$staged" examples/agent-smoke/operator-src/operator
printf '%s\n' "$actual" > "$setup/operator.sha256"
timeout 600 docker build $clean --secret id=proxy_ca,src=/etc/ssl/certs/ca-certificates.crt -t operator-dynamo:agent-smoke . > "$setup/dynamo-build.log" 2>&1
timeout 300 docker run --rm --name "$helper" -v "$PWD:/src:ro" -w /src -v "$prefix-modules:/go/pkg/mod" -v "$prefix-cache:/tmp/gocache" -v /etc/ssl/certs/ca-certificates.crt:/run/ca.pem:ro -e SSL_CERT_FILE=/run/ca.pem golang:1.26.5-bookworm sh -ec 'go mod download; GOCACHE=/tmp/gocache/agent-smoke go build -buildvcs=false -o /tmp/validator ./test/agent-smoke; chmod -R a+rX /go/pkg/mod; chmod -R a+rwX /tmp/gocache'
docker image inspect golang:1.26.5-bookworm --format '{{.Id}} {{json .RepoDigests}}' > "$setup/tool-image.txt"
printf '%s\n' "$prefix" > "$setup/cache-prefix.txt"
printf '%s\n' "$(( $(date +%s) - start ))" > "$setup/duration-seconds.txt"
printf 'Prerequisites ready; source/build evidence: %s\n' "$setup"
