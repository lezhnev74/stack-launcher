# 0006. Manifest sources and external testing

Status: Accepted.

## Compose source and Git access

A fresh API launch supplies one manifest reference, inline Compose YAML, or a
saved template name. A browser run starts with a saved copy of a template's
manifest and per-service environment overrides. The draft records the source
template's name and can change its manifest, environment overrides, and service
refs before launch. The launcher reuses the saved manifest when a stopped stack
starts.

Canonical HTTPS GitHub blob URLs and `github://` references select a regular
Git blob. The launcher resolves the ref, fetches it over SSH, and records the
repository, commit, manifest path, and pinned checkout. Compose-relative runtime
files resolve from the manifest directory in that checkout. Service build
contexts resolve from the service repository root.

Other HTTP(S) URLs use the bytes of a successful HTTP response. Local paths
and `file://` URLs read a regular file with the launcher account's filesystem
permissions. Host file references are for trusted callers. Web URLs, local
files, inline YAML, and saved templates provide manifest bytes without a checkout.
Saved templates also supply per-service environment overrides. Compose
preparation rejects runtime file references it cannot resolve. Named admin
`env_file` entries follow [ADR 0004](0004-runtime-environment-files.md) for
every source.

Connect to `git@github.com` using the OpenSSH configuration, host trust, and
credentials of the OS account running the launcher. GitHub controls repository
read access through that identity for both the Compose source and the service
build refs in [ADR 0002](0002-compose-and-image-sources.md). Run SSH in batch
mode and make the account's SSH agent available when required. Keep credentials
out of launch requests, application containers, and build contexts.

## Fresh-launch commands

A service can declare an optional command and its arguments under
`x-launcher.launch_command`.

```yaml
services:
  web:
    x-launcher:
      launch_command: [bash, ./scripts/seed.sh]
```

Compose starts the containers using their normal entrypoints and commands.
Once the services pass their startup checks, run each launch command in
dependency order inside its running service container. It uses that
service's runtime environment, including operator overrides. Startup health
checks that gate these commands must pass independently of their effects.

Run each command on a fresh launch and on retries of failed launches.
Starting a stopped stack preserves its
data and skips launch commands. Publish the Caddy route and report the stack
ready after the commands succeed. A failed or timed-out command fails the
launch, identifies the service, and leaves its route unpublished.

## External testing and demos

The launch API returns the stack ID when it accepts the launch. The
[CI helper](../app/docs/quick-start.md#wrap-an-external-ci-test) polls by ID,
provides the Caddy URL as `STACK_URL` to the caller's test command, and attempts
to remove the stack on exit. The repository's
[manual smoke workflow](../.github/workflows/dedicated-host-smoke.yml) uses the
helper to check an HTTPS fixture. External CI supplies its own test command
and diagnostics. Manual testers open the same URL from the web UI. A demo can
stop and start a prepared stack while retaining its data and hostname.

[Compose startup order](https://docs.docker.com/compose/how-tos/startup-order/) ·
[Compose exec](https://docs.docker.com/reference/cli/docker/compose/exec/)
