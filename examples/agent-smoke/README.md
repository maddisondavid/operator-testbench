# Reusable CronJob smoke

Run setup and `scripts/agent-smoke.py` from the repository root as described in the root README. Supply your reachable host via DYNAMO_HOST_ADDRESS or --host-address. Saved YAML is reused unchanged; each invocation copies input-only files, substitutes effective endpoints, builds the pinned unchanged operator, executes once and validates its fresh report. Generated operator binary and runtime evidence are ignored. This reference is not an integration into an unspecified customer repository.
