# Dedicated host and CI quick start

This walkthrough connects a trusted CI runner to a dedicated launcher host. The
launcher API has no authentication: it can run workloads as the launcher account
and read files accessible to it. Keep its HTTP listener on a private network
reachable only by trusted runners and administrators, with firewall rules. Do
not expose it to untrusted jobs or the internet.

## Prepare the launcher host

On Linux, install rootless Podman, podman-compose, Git, OpenSSH, and Caddy. Set
up the launcher OS account's subordinate UID/GID mappings, user session and
rootless networking as required by Podman. Run and validate Podman as that
account. `podman info --format '{{.Host.Security.Rootless}}'` should print
`true`. Use trusted parent directories and create the state directory with
mode `0700`. The launcher and Caddy need access to the Caddy admin Unix socket. Set
up the [Caddy baseline](../README.md#caddy-routes), wildcard certificate and
socket permissions before starting the launcher. Reserve the entire base domain
for launcher routes. The stack ID is the leftmost hostname label, for example
`quietly-otter-20261121` for a new stack.

Follow the [development build instructions](../README.md#run-the-checks) to
run `./dev setup` and `./dev build` from `app/`. Install the resulting
`build/stack-launcher` from the printed report directory into the launcher
account's `PATH` on the dedicated host.

Run the following as the launcher account, which owns `/srv/stack-launcher`.

```sh
mkdir -m 700 /srv/stack-launcher/state
cat > /srv/stack-launcher/config.yml <<'YAML'
listen: 10.0.0.10:8080
state_dir: /srv/stack-launcher/state
compose_provider: podman-compose
registry_auth_file: /srv/stack-launcher/auth.json
base_domain: stacks.example.com
caddy_admin_socket: /run/stack-launcher/caddy-admin.sock
YAML
chmod 600 /srv/stack-launcher/config.yml
stack-launcher -c /srv/stack-launcher/config.yml
```

Replace the private IP, domain and paths for your deployment. Persist the
launcher and Caddy as services under their intended accounts; preserve the state
directory and Caddy configuration across restarts. See the
[rootless Podman settings](../README.md#rootless-podman-launch) and
[Caddy setup](../README.md#caddy-routes).

Give the launcher account read access to the fixture repository over SSH. A
read-only deploy key scoped to that repository is sufficient. Install GitHub's
verified host key for the launcher account. As that account, outside a Git
checkout, clear any existing
`GIT_*` environment overrides and use the same SSH agent environment as the
service. Verify access with:

```sh
GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_ALLOW_PROTOCOL=ssh \
GIT_SSH_COMMAND='ssh -o BatchMode=yes' \
git ls-remote ssh://git@github.com/TEAM/FIXTURE.git HEAD
```

Expect a commit and `HEAD`. Configure a registry credential with pull-only
access to the fixture image. An administrator should verify its scope in the
registry, then as the launcher account authenticate and prove the pull works:

```sh
podman login --authfile /srv/stack-launcher/auth.json registry.example.com
chmod 600 /srv/stack-launcher/auth.json
podman pull --authfile /srv/stack-launcher/auth.json registry.example.com/team/smoke:1
```

Use a private image containing a POSIX shell, Nginx, `wget`, and `grep`. The
image should require authentication to pull. Commit this example as
`compose.yaml` in the fixture repository, substituting that registry image.
The container user needs permission to serve port 80 and write the document root.

```yaml
services:
  web:
    image: registry.example.com/team/smoke:1
    command: [sh, -c, 'printf "stack-launcher-smoke-ok\n" > /usr/share/nginx/html/index.html; exec nginx -g "daemon off;"']
    healthcheck:
      test: [CMD-SHELL, 'wget -qO- http://127.0.0.1/ | grep -qx stack-launcher-smoke-ok']
      interval: 1s
      timeout: 2s
      retries: 10
x-launcher:
  endpoint: {service: web, port: 80, https: true}
```

## Configure the trusted runner

Install a self-hosted Linux runner with Bash, curl, jq, and network access to the
private API and Caddy. Restrict the `stack-launcher-smoke` runner group to this
repository and the selected workflow
`.github/workflows/dedicated-host-smoke.yml` at `refs/heads/master` using
[runner group access controls](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access).
Create the `stack-launcher-smoke` environment and restrict its deployment
branches to `master` using [environment rules](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments).
Protect master and the workflow file from untrusted changes. Never run untrusted
pull request code on this runner: the API grants workload execution as the
launcher account.

In the repository's **Settings → Environments → stack-launcher-smoke**, add
these configuration variables under **Environment variables**.

| Variable | Example |
| --- | --- |
| `STACK_LAUNCHER_URL` | Private API origin `http://10.0.0.10:8080` |
| `STACK_LAUNCHER_FIXTURE_REFERENCE` | GitHub blob permalink `https://github.com/TEAM/FIXTURE/blob/0123456789abcdef0123456789abcdef01234567/compose.yaml` |

The [dedicated-host smoke workflow](../../.github/workflows/dedicated-host-smoke.yml)
runs by manual dispatch on `master` and probes the fixed fixture marker over
verified HTTPS. Use a permalink with a full 40-character commit.

From the runner, check private connectivity and wildcard DNS and TLS before
running the workflow. Substitute an unused hostname covered by the certificate:

```sh
curl --disable --max-time 15 -i http://10.0.0.10:8080/healthz
curl --disable --max-time 15 -i https://unused.stacks.example.com/
```

Expect 204 for health and a certificate-verified 404 for the unused hostname.
DNS must resolve wildcard stack names to Caddy, and the runner must trust the
certificate chain. Dispatch **Dedicated host smoke** on `master`. A passing run
logs the accepted stack ID, the ready HTTPS URL, and successful removal after
the fixture returns HTTP 200 and `stack-launcher-smoke-ok`.

## Wrap an external CI test

The helper reads ordinary launch JSON on stdin, waits for readiness, exports
`STACK_URL`, runs the given command and removes the stack on exit. For example:

```sh
export STACK_LAUNCHER_URL=http://10.0.0.10:8080
jq -n --arg reference 'https://github.com/TEAM/FIXTURE/blob/0123456789abcdef0123456789abcdef01234567/compose.yaml' \
  '{reference: $reference, duration: "30m"}' |
  app/scripts/with-stack.sh ./run-existing-e2e-tests
```

The test should use `STACK_URL`. The helper requires curl and jq. It polls for
up to five minutes with a two-second delay between polls. Launch and status
requests time out after 30 seconds. Cleanup gets a separate five-minute window
and waits two seconds between retries. A DELETE can use the remaining cleanup
time so the launcher can finish synchronous teardown. Set positive integer
seconds with `STACK_REQUEST_TIMEOUT`, `STACK_POLL_TIMEOUT`, `STACK_POLL_INTERVAL`,
`STACK_CLEANUP_TIMEOUT`, and `STACK_CLEANUP_INTERVAL`. Allow enough CI job time
for cleanup after the test, including request timeouts and retry intervals.
The helper preserves a failing test's status; a cleanup failure also fails an
otherwise successful test. It logs launch failures and a copyable retry command.

DELETE permanently removes this stack's containers, networks, named volumes,
exclusive checkout and ledger record. Cleanup retries the same ID after 409,
5xx or an ambiguous transport failure; 204 and 404 both mean it is gone. If
cleanup fails, repair the reported issue and repeat the logged DELETE for that
ID. Expiry stops a ready stack and preserves data; it is not deletion.

A POST is never retried automatically: a timeout or lost response may have
created a stack even without a confirmed ID. Look for `Launch accepted` and its
`run_id` in the launcher logs around the request time, then check the run in
`/ui` before resubmitting. A hard kill or runner loss can cut cleanup
short. Use the logged ID to inspect status and retry DELETE. If the
launcher or external Compose process was hard-killed, verify that process has
stopped before teardown. See the [lifecycle and removal details](../README.md#remove-a-stack-and-its-data).
