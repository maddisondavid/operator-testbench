# Operator Dynamo

This repository builds one Go executable and runs unchanged operators in Docker against a deliberately limited Kubernetes-style API. It includes a reusable CronJob smoke example that proves actual Job creation and cache-derived status. It does not run Kubernetes workloads or claim general operator compatibility.

## Supported setup and smoke

Prerequisites: Linux/amd64, Python3, shell/coreutils (including timeout), curl/tar/SHA256 tools, local Docker Engine on its default Unix socket, and the system CA bundle at `/etc/ssl/certs/ca-certificates.crt`. Docker Desktop, remote Docker and other platforms are not verified. Setup downloads pinned public sources/modules. No private image or credentials are required.

Clone your repository URL into a fresh directory, then run from its root:

```sh
./scripts/prepare-agent-smoke.sh
# Set this yourself to an IPv4 address assigned to this host and reachable from operator containers.
export DYNAMO_HOST_ADDRESS="YOUR_OPERATOR_REACHABLE_HOST_IPV4"
./scripts/agent-smoke.py
```

The placeholder is intentionally invalid. Missing/invalid configuration fails before launch; no host interface is guessed. Alternatively pass `--host-address "$DYNAMO_HOST_ADDRESS"`. Optional `--http-port`/`--api-port` select distinct unprivileged host ports; the API port cannot collide with the application's internal HTTP listener. The host must actually own the address and containers must reach it. Local Engine source mounts must be visible to the daemon.

The recurring command copies saved YAML, substitutes only endpoints in the fresh configuration, builds the unchanged pinned operator through the production build path, executes once and validates fresh report/exit/input provenance. Expected exit0 means the real Job/status assertions and cleanup passed. It prints the report/evidence location. Nonzero exits remain failures; cancellation cannot pass. Ctrl-C stops owned work. Evidence directories remain for inspection; no shared image/cache pruning. Saved example YAML never changes during a run; its unresolved endpoint is not directly runnable without preparation.

## Interactive Docker deployment

After setup, the example can also be launched as a normal interactive session:

```sh
./scripts/deploy-source.py --workspace examples/agent-smoke --ui-write
```

It prints its URL and owned container name (default `dynamo-shared-editor`). It uses a fresh copied workspace and never replaces an existing container. Explicit endpoints may also be supplied on this command. The user HTTP listener binds all container interfaces; Docker publishes only the configured host address. `--ui-write` enables the trusted-development editor for supported non-Secret resources. Without it the UI is read-only. No authentication or credential isolation is provided.

Stop/remove this deployment, retaining its printed workspace/evidence:

```sh
docker stop --signal SIGINT --time 6 dynamo-shared-editor
docker rm dynamo-shared-editor
```

## Build and checks

All demonstrated execution is Docker-based; native host execution is not verified.

```sh
./scripts/check.sh
python3 -m unittest discover -s test/publication -p '*_test.py'
python3 -m unittest discover -s test/agent-smoke -p '*_test.py'
docker build --secret id=proxy_ca,src=/etc/ssl/certs/ca-certificates.crt -t operator-dynamo:agent-smoke .
```

The report/event fixtures at their normal test paths are synthetic schema and invocation-validation inputs, not historical execution evidence. The real smoke run proves framework behavior separately. No exporter or fixture replacement is needed in a clean checkout. Ordinary Go tests and embedded assets are included; five historical integration-tag tests remain local-only because their investigation harnesses and evidence are deliberately excluded.

## Git publication boundary

Before staging changes, run:

```sh
python3 scripts/audit-git.py --output evidence/git-audit
```

The audit uses a temporary Git index to model normal `git add -A`; it does not modify your index, create a commit, configure a remote or push. It scans **all** tracked and staging-eligible files, not a fixed source allowlist. It also checks current index objects and available history. It writes the exact candidate manifest and a clean `checkout/` under the output directory. Use a fresh output directory for each audit. Review changes and the manifest, run checks and real smoke from that checkout, then stage and inspect normally:

```sh
git add -A
git diff --cached --stat
git diff --cached --check
# Review the staged contents before choosing your own commit and remote.
```

No commits or remote are created by these instructions. Re-audit after any change; an earlier result is not evidence for new bytes. Ignore rules exclude local evidence, generated binaries, manual workspaces, caches, agent files and historical specs/docs/helpers. Already tracked files are still inspected even if later ignored. Local originals were preserved privately before synthetic fixture replacement; these private histories are not required to build or run the committed source. The supported tree includes cmd/internal, ordinary tests, the report schema, the saved smoke example and the five operational Python/shell entry points plus shared scanner.

The audit bounds selected text, paths, source reads and Git objects, rejects special files/symlinks/hardlinks and checks plaintext plus one-level base64 recognized credential/address patterns. Exact context exceptions are recorded in `test/publication/scan-exceptions.json`. Reserved documentation addresses are only non-network test inputs; wildcard/loopback strings describe local listener/test syntax. No development-machine address is an executable default. Unknown binary/archive payloads are rejected, not certified.

Limits: this is a bounded pattern/content review, not proof that arbitrary secrets, new encodings or malicious code cannot exist. No public upload, credential rotation, license choice or third-party license grant occurs. Add and review licensing separately before distribution. Execution evidence is Docker/Linux/amd64, not native host or multi-machine portability.

Upstream example: unchanged Kubebuilder commit `cd90bd82a2d692fbf63ba0231699e2e3dc0b6a08`, CronJob tutorial project. Setup records source/image/tool identities and validates the expected reference binary. See its public repository for upstream licensing and provenance.
