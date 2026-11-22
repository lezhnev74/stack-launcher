# 0005. Stack ledger and lifecycle

Status: Accepted.

## Stack ledger

The HTTP API and the web UI at `/ui` use the same SQLite ledger. The launcher
records the stack ID before creating resources and tracks the hostname, state,
image identities, resolved commits when available, and the configured runtime
and expiry. Operators can manage running, stopped, and failed stacks by ID.

Configure a protected host directory as `state_dir`. The launcher stores
`stacks.sqlite3` and its journal files there, alongside the admin-managed
`env/` directory from [ADR 0004](0004-runtime-environment-files.md). Server
admins and the launcher have filesystem access to this directory. Application
stack containers receive environment variables through Compose.

## Lifecycle

- **Create run** stores a draft with a copy of a template's Compose manifest
  and per-service environment overrides. A draft has no deployed resources.
- **Launch** deploys a draft or creates a fresh API stack and publishes its
  Caddy route once the stack is ready. The default running time is seven days.
  API callers can specify a different duration. The launcher records the
  expiry when the stack becomes ready. [ADR 0006](0006-git-sourced-launch-and-external-testing.md)
- **Retry launch** attempts a failed launch with the same ID and saved inputs.
  It confirms any pending route withdrawal and preserves named volumes.
- **Stop** withdraws the route and removes the containers, releasing their
  ports and compute. Keep the stack's named volumes and ledger entry.
  Expiry performs the same stop operation. Check saved expiry times when the
  launcher starts and while it runs.
- **Start** recreates a stopped stack using the same hostname, images, and
  volumes. Reread the named environment files and apply the saved overrides.
  Start a fresh expiry window using the saved duration once the stack is ready.
- **Remove** explicitly deletes the stack's owned resources, volumes, checkout,
  and ledger entry. Confirm any necessary route withdrawal before releasing
  ports. Keep the ledger entry until cleanup succeeds so a failed removal can
  be retried. Keep Podman's shared build cache available for other launches.

[SQLite journal files](https://sqlite.org/tempfiles.html) ·
[Caddy routing and port release](0003-caddy-routing-and-host-ports.md)
