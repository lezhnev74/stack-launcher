# Development guide

`/healthz` currently returns an empty HTTP 204 response for GET and HEAD. The
[accepted launcher decisions](../README.md#architecture-decisions) govern feature work.

For deployment and the opt-in CI smoke test, see the [dedicated host quick start](docs/quick-start.md).

## Go layout

`app/` is a single Go module. `cmd/app` parses the CLI flag and wires
dependencies. `internal/config` parses and validates YAML configuration.
`internal/httpserver` owns routing and translates between HTTP and application
behavior. `internal/ledger` persists stack records, `internal/manifest` resolves
and saves Compose sources, and `internal/caddy` manages ingress routes. Add
packages by capability as features arrive.
Define small interfaces in the packages that use them when a
dependency needs to be substituted.
[Go module layout](https://go.dev/doc/modules/layout#server-project) ·
[Go interface guidance](https://go.dev/wiki/CodeReviewComments#interfaces)

## Run the checks

Use rootless Podman on Linux with Bash, GNU tar and coreutils. Go 1.27.4 and
the pinned QA tools run in the tools image. Setup and the vulnerability scan
need network access.

```sh
cd app
./dev setup
./dev check
```

| Command | Checks |
| --- | --- |
| `./dev lint` | gofmt, ShellCheck, golangci-lint config validation and linters (including vet, Staticcheck, gosec, and Cyclop), and `go mod tidy -diff`. |
| `./dev test-go` | Uncached, shuffled Go tests and fuzz seeds with the race detector and module-wide coverage instrumentation. Internal statement coverage must be at least 90%. |
| `./dev fuzz` | Explore HTTP method and path inputs for five seconds with two workers. |
| `./dev build` | Compile `stack-launcher` with `-trimpath` and verify that the binary initializes `stacks.sqlite3` in `state_dir` and serves `GET /healthz` on loopback. |
| `./dev security` | Scan the Go module and standard library for reachable vulnerabilities with govulncheck. |
| `./dev format` | Apply gofmt to `cmd/` and `internal/` in the checkout. |

`./dev check` runs lint first. After lint passes, the tests, fuzzing, build and
vulnerability scan run in parallel. Each check invocation starts from a read-only
snapshot of the working tree, including untracked source and excluding `.env`
files and prior artifacts. The container runs the binary smoke check on its own
loopback without publishing a port. Fuzzing uses a writable copy of the snapshot
so Go can save failing inputs.
The first run fills the checkout's `.cache/go-build` directory. Later runs can
reuse compiled packages, including race-instrumented packages. Tests still
execute on every run, and the vulnerability scan still queries its database.
The command prints the `coverage/dev-*` report directory. Each gate records
`output.log` and `status.txt` there. The test gate retains its coverage profile
and function report, and the build gate retains the binary and server log.
The [GitHub Actions workflow](../.github/workflows/qa.yml) runs the same check on
pull requests and pushes to `master` and uploads the reports.

To run the built binary, substitute the report directory printed by `./dev build`
or `./dev check`.

Pass one YAML configuration file with `-c`. It accepts `state_dir`, `listen`,
and the [Podman](#rootless-podman-launch) and [Caddy](#caddy-routes) settings.
If you omit `listen`, the launcher uses `127.0.0.1:8080`. Create the
state directory under trusted parent directories before running the launcher.
It must have mode `0700` and be usable by the launcher. Use an absolute
`state_dir` path or a path relative to the config file's directory. At startup,
the launcher opens or creates `stacks.sqlite3` as a regular file in that
directory and applies any pending migrations before serving HTTP. A migration
failure rolls back the upgrade and stops startup. The launcher also stops if
the ledger's schema version is newer than the binary supports.

```sh
launcher_dir=$(mktemp -d "$HOME/stack-launcher.XXXXXXXX")
mkdir -m 700 "$launcher_dir/state"
cat > "$launcher_dir/config.yml" <<'EOF'
listen: 127.0.0.1:8080
state_dir: state
EOF
./coverage/dev-XXXXXXXX/build/stack-launcher -c "$launcher_dir/config.yml"
```

From another terminal, run `curl -i http://127.0.0.1:8080/healthz` and expect
HTTP 204.

## Browser controls

Open `/ui` on the launcher listener (by default `http://127.0.0.1:8080/ui`).
In **Templates**, create a template from the starter manifest or import a
Compose YAML file up to 16 MiB. The list shows each template's service names.
Give the template a unique name. You can rename
it, edit its Compose manifest in the YAML code editor, and set environment
overrides for each service. Use **Update services** after editing YAML to refresh
the list of services, their published and internal ports, and the HTTP route.

In **Runs**, choose a template and select **Create run**. The draft keeps its
own copy of the manifest and overrides along with the template's name. Keep the
manifest's default for each service or enter a registry image reference or an
exact Git branch, tag, or commit, depending on the service's source. You can
save the draft and return later. **Launch run** uses the current configuration.
The **Launch log** shows timestamped progress through image preparation,
container startup, readiness checks, launch commands, and route publication.
It remains available after startup. **Original manifest** opens the run's saved
Compose YAML. Access the
loopback bindings from the launcher host or through SSH forwarding.

The environment editor shows overrides for the selected service. Enter one
`KEY=value` per line. A line beginning with `#` is a comment. `KEY=` saves an
empty string. Values are literal, including `$`. Use JSON-style double quotes
for escapes such as `\n`, or single quotes for a literal quoted value. Remove
a line to restore its default.

In both code editors, Tab indents and Shift+Tab outdents. Press Escape, then
Tab to move focus out of the editor.

The run page also shows failures and the available lifecycle controls. Stopping
retains data and removal deletes it. Starting a stopped run reuses its pinned
images. After correcting a launch failure, use **Retry launch** on the failed
run's page. The run's saved environment overrides become read-only when launch
begins.

The launcher rejects cross-origin browser mutations. Use the controls on the
launcher listener or use a command-line API client. Protect access to the listener.

The browser editor is served from a checked-in bundle embedded in the Go binary.
After changing editor sources, run `npm ci`, `npm test`, and `npm run build` from
`app/` and include the regenerated bundle in the change.

## Launch, status, stop, start, and remove API

The API is for trusted callers. A reference can read a local file or fetch a URL
from the launcher host, and Compose launches workloads under the launcher account.
Protect access to the listener. Configure both `base_domain` and
`caddy_admin_socket` to accept launches. The configured socket must reach the
same Caddy instance while the ledger has starting, ready, or stopping stacks or
stacks with pending route withdrawals. Update the config if that instance's
admin socket moves.
`POST /stacks` returns 503 when ingress is omitted or when the configured base
domain differs from those records. Health and status remain available in both
cases and when Caddy is offline.

```sh
curl -i -X POST http://127.0.0.1:8080/stacks \
  -H 'Content-Type: application/json' \
  -d '{"reference":"https://example.com/compose.yaml","duration":"168h","images":{"web":"registry.example.com/web:v1"}}'
curl http://127.0.0.1:8080/stacks/STACK_ID
```

Supply exactly one of `reference`, `inline_yaml`, or `manifest_template` as a JSON
string. `manifest_template` takes the template's current name and returns 404
if it is absent. Explicit environment values override template values with the same
service and variable name. An explicitly empty inline string is acquired and
then fails Compose validation.
Send `Content-Type: application/json`. The request body limit is 17 MiB.
`duration` is an optional positive Go duration and defaults to `168h`. `images`
maps registry-backed service names to image references and can supply a service's
only image. An override for a Git-built service fails startup validation. POST
returns 202, an `id`, and a `Location` header after the ledger row is committed.
Each accepted POST creates a new ID such as `quietly-otter-20261121`, using the
UTC launch date. The launcher can add a numeric suffix if candidate names collide.
Poll the `Location` URL while `state` is
`launching`. GET returns
200 for a known ID and 404 for an unknown ID. Launch failures include
`failure_phase` and `failure`. Ready status includes `url`, service/port
`bindings`, `ready_at`, and `expires_at`. Readiness is recorded after Podman
startup checks, any fresh-launch commands, and successful Caddy publication.
With matching ingress settings, expired ready stacks are stopped automatically,
including stacks whose deadlines passed while the launcher was down. The
launcher checks at startup and on a five-second timer. Expiry uses the same
route withdrawal and volume-preserving teardown as Stop.

On restart, interrupted API launches become failed. The launcher allows up to
two seconds to withdraw incomplete publications before serving HTTP, then retries
pending withdrawals from failed fresh launches while running. A failed launch
with `publication_pending` still requires confirmation from Caddy. Recovery uses
the admin socket from the launcher config. If ingress is omitted or the base domain differs, those
withdrawals stay pending until the launcher restarts with matching settings.
Recovery removes unowned checkout and private Compose scratch directories while
retaining recorded checkouts. A state-directory lock
prevents another launcher from recovering the same ledger concurrently. Recovery
retains Podman resources and upstream ports for later lifecycle operations. The
startup expiry check then handles overdue ready stacks. Podman attempts cleanup
after a startup or readiness failure. Legacy internal launches remain eligible
for their existing retries.

To stop a ready stack, send a POST without a body.

```sh
curl -i -X POST http://127.0.0.1:8080/stacks/STACK_ID/stop
curl http://127.0.0.1:8080/stacks/STACK_ID
```

The launcher records `stopping` and removes the live URL, bindings, and expiry
from status. The request waits for Caddy to confirm route withdrawal and Podman
to remove the stack's containers and networks. Allow time for the services'
Compose stop grace periods. The ledger entry, saved launch inputs, and named
volumes are retained. A successful stop returns 204, and GET reports
`stopped`. Repeating the POST for a stopped stack returns 204.

Stop returns 404 for an unknown ID and 409 for draft, launching, starting, failed, removing,
or legacy stacks, or while another lifecycle operation for that ID is active. Missing or
mismatched ingress configuration returns 503. Restore the stack's ingress
settings before retrying. A Caddy withdrawal failure returns 502 and leaves the
containers and ports in place. A Podman or ledger failure returns 500. Retry
the same POST after a 502 or 500, or if GET still reports `stopping` after a
client timeout or launcher restart. Look up the stack ID in the launcher logs
if a Caddy or Podman failure persists. If expiry leaves the stack in `stopping`,
use the same Stop retry. Teardown uses the configured Compose provider and saved
launch inputs, including any pinned checkout.

To start a stopped API stack, send a POST without a body.

```sh
curl -i -X POST http://127.0.0.1:8080/stacks/STACK_ID/start
curl http://127.0.0.1:8080/stacks/STACK_ID
```

Start returns 202 with the same ID and `Location`. Poll status while it reports
`starting`. When publication succeeds, the stack reports `ready` at the same
hostname and gets a fresh expiry based on its saved duration. Start recreates
the project from the saved manifest, pinned local image IDs, and named volumes.
The new containers use the current admin env files and saved overrides.
Start skips fresh-launch commands.

Start returns 404 for an unknown ID, 409 for a legacy stack, a state other than
`stopped`, or another lifecycle operation in progress, and 503 when ingress settings are
missing or mismatched. A startup or publication failure or an interrupted
Start, moves the record to `stopping` with a failure phase and message. If
Caddy could have applied the publication, the launcher attempts to withdraw
it. Stop retries any pending withdrawal before removing the project. Once
Stop returns 204, correct the failure and try Start again. Restore any missing
pinned checkout files before teardown and any missing pinned images before
starting. After a hard kill, verify that any external Compose process has
stopped before retrying teardown.

### Retry a failed launch

Send `POST /stacks/{id}/retry` without a body. A successful request returns 202
and the original ID. The run keeps its hostname. Poll status while it reports
`launching`.
Retry uses the saved manifest, service references, and environment overrides.
If acquisition failed, it tries the saved manifest source again. If the first
attempt pinned images, retry reuses them and cleans up any remaining containers
and networks while retaining named volumes. Fresh-launch commands run again and
may see data written by the previous attempt.

Retry requires the original ingress configuration. The launcher confirms any
pending route withdrawal before retrying. Older API records that failed before
pinning images may lack saved launch inputs and require a new launch request.

### Remove a stack and its data

Send a DELETE without a body to permanently remove an API stack and its data.
Remove accepts runs in `draft`, `ready`, `stopped`, `stopping`, `failed`, or `removing`.

```sh
curl -i -X DELETE http://127.0.0.1:8080/stacks/STACK_ID
```

The launcher records `removing`, clears the live URL, bindings, and expiry from
status, and confirms any required Caddy withdrawal before releasing Podman
ports. `publication_pending` on a `removing` stack means Caddy withdrawal still
needs confirmation. Remove then deletes the stack's project containers,
networks, named volumes, and any exclusively owned Git checkout. It uses
Podman's project labels, so saved Compose files and a Compose provider are
unnecessary for teardown. Shared images and the Podman build cache are kept.
The request returns 204 after deleting the ledger entry. Later GET and DELETE
requests for that ID return 404.

Remove returns 409 for a legacy stack, a launch or start still underway, or
another active lifecycle operation for that ID. Missing or mismatched ingress
settings return 503 for `ready`, `stopping`, and any stack with
`publication_pending`. A failed Caddy withdrawal returns 502 and leaves the
containers and ports in place. A Podman, checkout, or ledger failure returns
500. Look up the stack ID in the launcher logs if an error persists. Retry the
same DELETE after fixing the failure, or when status still shows `removing`
after a client timeout or launcher restart. Once withdrawal is confirmed and
recorded, Remove can resume with Caddy offline. After a hard kill, verify any
external Compose process has stopped before retrying.

### Runtime environment

Administrators can place private dotenv files in the real `env/` directory inside
`state_dir`. Compose services may declare `env_file: filename` or an ordered
list of filenames. Names must be literal basenames resolving to readable regular
files within that directory for every manifest source. The last file that defines
a key wins. Podman Compose parses the files as dotenv. Use `KEY=value` entries
because bare keys can inherit from the provider process environment.

Launch requests may include `"environment":{"web":{"KEY":"value"}}`.
These saved overrides take precedence over the service's `environment`, which
in turn takes precedence over the ordered env files. Empty strings are literal.
Unknown services are reported asynchronously as startup failures after the 202
response. `GET /stacks/{id}/environment` returns only saved overrides in the
same JSON shape.

Use PUT to edit a draft run's saved overrides before launch. Once launch begins,
PUT returns 409. To provide overrides on a fresh API launch, include them in
the POST request. Saving a nonempty map requires a saved manifest.

```sh
curl -i -X PUT http://127.0.0.1:8080/stacks/STACK_ID/environment \
  -H 'Content-Type: application/json' \
  -d '{"environment":{"web":{"KEY":"new value","EMPTY":""}}}'
```

PUT returns 204 after replacing the entire map. Omit a key or service to remove
its overrides, or send `{"environment":{}}` to clear them all. The launcher
applies the saved overrides each time it creates the run's containers.

## Manifest sources

The internal acquisition entry point accepts exactly one manifest reference
or inline YAML. An empty reference and a nil inline slice count as absent. A
non-nil empty inline slice counts as supplied. All sources save exact bytes.
The launcher acquires saved templates from their uploaded bytes, as with inline YAML.
Compose preparation validates the saved manifest as described below.

| Reference | Example | Resolution |
| --- | --- | --- |
| HTTP(S) URL | `https://example.com/compose/latest` | Follow standard HTTP redirects and read a successful response regardless of content type or filename. |
| Local path | `./fixtures/compose.yaml` | Read a regular file on the launcher host. Relative paths use the launcher's working directory. |
| Local file URL | `file:///srv/manifests/compose.yaml` | Read an absolute file path on the launcher host. The URL authority can be empty or `localhost`. |
| Canonical GitHub blob URL | `https://github.com/team/checkout/blob/main/deploy/compose.yaml` | Fetch the pinned Git checkout over SSH. |
| GitHub reference | `github://team/checkout/feature/demo/deploy/compose.yaml` | Fetch the pinned Git checkout over SSH. |

HTTP(S) responses and local files have a 16 MiB read limit. HTTP requests
have a 30-second timeout. Local file references use the launcher account's
filesystem access and are intended for trusted callers. Web URLs, local
files, and inline YAML save bytes alone. Pinned resolution for GitHub blob
URLs uses HTTPS and the `github.com` host without an explicit port. Other
web URLs, including `raw.githubusercontent.com` links, use HTTP fetching.

### GitHub source

The launcher runs `git` and `ssh` from `PATH` under its OS account. It connects
to `git@github.com` in noninteractive mode using the account's normal OpenSSH
configuration, host trust, keys, and agent. GitHub determines which
repositories the SSH identity can read. Load passphrase-protected keys into
the account's agent and make its socket available to the launcher.

Configure the `github.com` destination through OpenSSH. The launcher isolates
Git configuration and Git environment overrides when constructing the remote.
If you copied an earlier config example, remove its `github` block.

GitHub references can use a branch, tag, or full commit ID, including branch
and tag names with slashes. GitHub blob URLs also accept display queries or
line anchors. Use a commit permalink when
more than one ref matches a link. If a ref moves before Git can fetch the
resolved object, retry the request or use a commit permalink. The ledger stores
the exact manifest bytes and the pinned checkout stays under
`state_dir/checkouts`, where Compose can resolve files relative to the manifest.
Git initializes the checkout without submodules.

From `app/`, run
`go test ./internal/manifest -run '^TestAcquire'`. These tests use local Git and
SQLite to check SSH invocation, checkout persistence after a branch moves,
and byte-for-byte equality across sources. The tests use a local HTTP server
and temporary files to exercise web and file resolution, including retries.

## Compose preparation

`internal/compose.Prepare(stateDir, stackID, manifest)` returns an in-memory
launch YAML copy, a stable project name derived from the full stack ID, and the
endpoint service, container TCP port, and HTTPS setting. It does not write files,
modify the saved bytes or checkout, allocate ports, or start a provider. For example:

```yaml
services:
  web:
    image: example/web:latest
    ports: ["127.0.0.1:9000:9000/tcp"]
    environment: {MODE: test}
    volumes: ["data:/data"]
    x-launcher:
      launch_command: [sh, -c, 'printf "%s\n" "$$MODE" > /data/seed']
volumes: {data: {}}
x-launcher:
  endpoint: {service: web, port: 8080, https: true}
```

The endpoint is required once, names an existing service, and defaults HTTPS to
true. Use a registry image in Compose, a caller-supplied registry image override,
or a Git build for each service. The launch copy gives each distinct TCP
target a dynamic published port on `127.0.0.1`; requested fixed host ports are
replaced, and the endpoint is added if needed. The Podman runtime discovers the
actual ports after startup. Public addresses, UDP, ranges, and unsupported port
options are rejected.

The launcher accepts services with image or Git build settings, image pull policy, command, entrypoint, explicit
environment, admin env files, dependencies, health checks, ports, volumes,
networks, configs, secrets, restart, working directory, user, init, stop grace
period, expose, labels, and `x-launcher.launch_command`. Top-level named volumes
and networks use project-scoped defaults and cannot specify names, external
resources, or drivers. Anonymous volumes are unsupported. Repository
bind mounts must use `:ro` or `read_only: true`; named volumes hold mutable data.
Configs and secrets must name files in the pinned Git checkout, and service
references must match those declarations. Relative file
paths for runtime files resolve from the saved manifest directory, must exist
within that checkout after symlink resolution, and are rewritten in the copy.
Web, host file, and inline sources have no manifest checkout for runtime files.
The launcher rejects profiles, includes, external resources, and other
unsupported fields.

The launcher validates source Compose YAML as literal values and disables
implicit project `.env` loading. Assign an explicit value to every entry in
`environment`. Commands can escape dollar signs as `$$`. Repository file
paths must be literal and contain no dollar signs. The provider reads admin env
files at container creation. Run `go test ./internal/compose` from `app/` for
focused preparation tests.

### Git-built service images

Use `build` with `x-launcher.git` to build a service image from a GitHub
repository during fresh launch. This inline manifest mixes a Git-built web
service with a registry-backed database.

```yaml
services:
  web:
    build:
      context: app
      dockerfile: Dockerfile.production
      target: runtime
      args: {VERSION: "1"}
    x-launcher:
      git: {repository: owner/web, ref: feature/demo}
  db:
    image: docker.io/library/redis:8-alpine
x-launcher:
  endpoint: {service: web, port: 8080}
```

Set `repository` to `OWNER/REPO` and `ref` to an exact branch, tag, or full
commit SHA. Local builds fetch the selected commit using the launcher's SSH
identity. Use the same full commit SHA when multiple services
must build from the same revision. The build context defaults to `.` and
resolves from the service repository root regardless of the manifest source.
The `dockerfile` defaults
to `Dockerfile` and resolves from the build context. Both paths must resolve
inside the service checkout. An `.in` Dockerfile is unsupported. Use the short
form `build: .` or the mapping fields shown above. Supply build arguments as
explicit strings in an `args` mapping or a list of `KEY=value` strings.
Use `$$` for a literal dollar sign.

By default, the launcher builds a Git-backed service on the host. It ignores
the service's Compose `image` value and leaves registry tags untouched. The
ledger saves the repository, resolved commit, and selected image ID. Stop and
Start reuse the recorded image ID. The launcher removes temporary source
checkouts after each build and sweeps interrupted checkouts on restart. Podman
retains the image and its shared build cache. A hard kill can leave intermediate
Buildah containers.

### Prebuilt images for Git-backed services

To reuse CI images for a Git-backed service, set `prebuilt_repository` and keep
the local `build` settings in the manifest.

```yaml
services:
  web:
    build: .
    x-launcher:
      git:
        repository: owner/web
        ref: feature/demo
        prebuilt_repository: registry.example.com/team/web
x-launcher:
  endpoint: {service: web, port: 8080}
```

Use a fully qualified registry repository with no tag or digest. CI must
publish immutable `git-<full-commit-sha>` tags in that repository for this
service's build recipe, including its context, Dockerfile, target, and build
arguments. Publish images for the launcher host's platform. Use a separate
image repository for a different recipe at the same Git commit.

On fresh launch, the launcher resolves the Git ref and looks up the commit's
tag using the launcher account's registry credentials. It pulls a matching
image by digest and records that reference in the ledger. On a confirmed
registry miss (`MANIFEST_UNKNOWN` or `NAME_UNKNOWN`), it checks out and builds
the same commit locally. An unclassified 404 and other lookup or pull failures
appear in stack status. If lookup fails at every configured mirror and the
primary registry, the primary registry's response determines whether to build.
Start uses the saved image ID without checking Git or the registry again.

### Fresh-launch commands

Set `x-launcher.launch_command` under a service to a nonempty YAML list of
strings containing the executable and its arguments. The launcher passes
each argument directly to Podman. Use an explicit shell for variable expansion
or redirection, and write `$$` for a dollar sign passed to the container.
The command uses the running container's environment, including admin env
files and operator overrides.

On a fresh launch, all services must pass their startup checks before any
command runs. The launcher runs the commands in `depends_on` order, including
dependencies through services without commands. When a manifest declares a
launch command, dependency cycles and references to undeclared services fail
validation. After the commands succeed, the launcher checks readiness again and
publishes the Caddy route. Health checks must be able to pass before seeding.

Each command has up to two minutes. If one exits unsuccessfully or times out,
status reports `failure_phase: startup` and names the service and exit status
or timeout. Command output is discarded. The route stays unpublished and the
launcher attempts to remove the project's containers and networks. Named
volumes can retain partial writes. Retrying the same run can run its commands
against those volumes again.

### Lifecycle logs

The launcher writes timestamped launch, retry, start, and stop diagnostics to
stderr. Filter the service log by `run_id="STACK_ID"`, using the ID shown in
the browser or the API response. The log follows manifest acquisition, image
pulls and builds, Compose startup, readiness, fresh-launch commands, Caddy
publication and withdrawal, Podman cleanup, and ledger state changes. It
includes elapsed times for long-running steps and records readiness blockers
as they change.

Failure lines can contain diagnostic stderr from Git and Podman. Keep the
service log private. Fresh-launch command output is discarded, and runtime
Podman stderr during launch is suppressed when an admin `env_file` is used.

## Rootless Podman launch

The runtime uses the launcher account's local rootless Podman storage. Set
`compose_provider` to a `podman-compose` executable name on `PATH`, or give its
path. The default is `podman-compose`. The runtime resolves and pins that
executable when it launches a stack. An executable path such as
`./bin/podman-compose` and an auth file path resolve relative to the configuration
file when given as relative paths. The verified versions are Podman 5.8.9 and
podman-compose 1.6.0.

Set `registry_auth_file` to the persistent auth file created with
`podman login --authfile /path/to/auth.json registry.example.com`. Use a pull-only
credential. If you omit the setting, Podman uses its normal account credential
lookup. For predictable registry selection, use fully qualified image references.
Unqualified references use Docker Hub.

Registry-backed services default to `pull_policy: always`. Set
`pull_policy: newer` on a service to check the registry and pull when its image
digest differs. If the registry check or pull fails, Podman uses the image in the
launcher's local rootless storage for the same reference. Launch fails if neither
source can supply the image. Use `always` when a failed pull must fail the launch.
Git-backed services use the policy described above and cannot specify
`pull_policy`.

`internal/podman.Launch` takes a stack ID with a saved manifest and optional
per-service registry image references. It pulls or builds the selected images,
records their immutable IDs and provenance in SQLite, and starts the project
using those IDs. Readiness requires running services, passing configured health
checks, and verified loopback port bindings. A Git build has up to two minutes
for Git checkout and ten minutes for Podman build. Other Podman commands during
launch have up to two minutes each. The readiness check has up to 30 seconds
after Compose startup. The launch rejects `service_completed_successfully` dependencies
because the provider can start the dependent after a failed prerequisite. The
result includes the endpoint address and HTTPS preference, plus all published
bindings by service and container port.

The generated provider file pins the project name and suppresses implicit
`.env` loading. File-backed configs use read-only binds with shared SELinux
relabeling. They retain the host file's permissions and ownership. A pull,
Git checkout, or build failure identifies the service and operation in API
status under `failure_phase: startup`. After images are recorded, a startup
failure retains the image selection and attempts to remove the project's
containers and networks while preserving named volumes. After correcting the
cause, retry the failed run from the browser or API.

Run the real-provider check on a Linux host with rootless Podman, podman-compose,
Git, and registry access. It starts two projects with distinct networks, named
volumes, and loopback ports. It also checks that a volume in use blocks Remove,
that retry reclaims the selected project and a failed launch's volume, and that
the other project's data and the shared build cache are still available.

```sh
cd app
STACK_LAUNCHER_PODMAN_TEST=1 go test ./internal/podman -run '^TestRootlessPodman$' -count=1
```

The Git build checks use a local Git fixture via an SSH shim. They verify a
mixed launch, Start after the build ref moves, and a build failure reported in
stack status with no route published. Run both from `app/`.

```sh
STACK_LAUNCHER_PODMAN_TEST=1 go test ./internal/podman ./internal/httpserver -run '^TestGitBuild(RestartIntegration|FailureStatus)$' -count=1
```

## Caddy routes

Set `base_domain` and `caddy_admin_socket` together when configuring ingress.
Both can be omitted for health, status, and operations that require no Caddy
calls. A relative socket path resolves from the YAML file's directory.

```yaml
state_dir: /srv/stack-launcher/state
base_domain: stacks.example.com
caddy_admin_socket: /run/stack-launcher/caddy-admin.sock
```

Point `*.stacks.example.com` at the Caddy host and provide Caddy with a matching
wildcard certificate and key. Run Caddy on the same host as Podman. Give the
Caddy service account a directory for the admin socket and grant the launcher
account group access to connect. Keep the socket available only to those
accounts and the host administrator. The launcher uses the filesystem path from
its YAML config, while Caddy uses its Unix network address syntax.
[Caddy network addresses](https://caddyserver.com/docs/conventions#network-addresses)

Use this Caddy JSON configuration as the ingress baseline. The named server
exposes HTTP on port 80 and HTTPS on port 443. Set the certificate paths to the
files available to Caddy and leave `shutdown_delay` at its zero default.
Reserve hostnames below `base_domain` and route IDs starting with
`stack-launcher-` on this server for the launcher.

```json
{
  "admin": {"listen": "unix//run/stack-launcher/caddy-admin.sock|0660"},
  "apps": {
    "tls": {
      "certificates": {
        "load_files": [{"certificate": "/etc/caddy/wildcard.crt", "key": "/etc/caddy/wildcard.key"}]
      }
    },
    "http": {
      "servers": {
        "stack-launcher": {
          "listen": [":80", ":443"],
          "automatic_https": {"disable": true},
          "tls_connection_policies": [{}],
          "routes": [{"handle": [{"handler": "static_response", "status_code": 404}]}]
        }
      }
    }
  }
}
```

`internal/caddy` publishes or withdraws a hostname one label below the base
domain. When publishing, callers pass the verified `127.0.0.1` endpoint and
HTTPS preference from the Podman result. HTTPS stacks redirect HTTP with status
308 and preserve the path and query. HTTP-only stacks proxy over HTTP and return 404
over HTTPS. A route update changes both protocols together and preserves other
routes. Retry failed withdrawals before releasing the upstream port. Existing
in-flight requests can drain after a config change, and Caddy config reloads
close WebSocket sessions by default.
[Caddy config API](https://caddyserver.com/docs/api#concurrent-config-changes) ·
[Caddy WebSocket handling](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#streaming)

Load the baseline when first configuring Caddy. Give the Caddy account writable
configuration storage and check its logs for autosave errors before using
`caddy run --resume` to restore routes on restart. Reloading the baseline
replaces the active routes.
[Caddy autosave behavior](https://caddyserver.com/docs/api)

Run the ingress integration test with `caddy` on `PATH`. It starts Caddy with
a temporary wildcard certificate and two local backends, checks redirects and
proxying over both protocols, and verifies the second stack still serves after
the first route is withdrawn.

```sh
cd app
STACK_LAUNCHER_CADDY_TEST=1 go test ./internal/caddy -count=1
```

## Ledger migrations

Add SQL files to `internal/ledger/migrations/` using the next consecutive
`NNN_description.sql` name. Keep shipped files unchanged. The build embeds the
files, and the migration runner executes pending scripts in one transaction and
updates SQLite's `user_version`. Put schema and data changes in the scripts and
leave transaction control and version updates to the runner.

## Test scope

Cyclop limits production cyclomatic complexity to 7. The test gate also requires
CRAP ≤ 7 for each named production function, including command wiring. CRAP
combines cyclomatic complexity and statement coverage. Functions missing
coverage data are scored at 0% coverage. The coverage floor measures statements
under `internal/` and includes calls made from tests in other packages.
[CRAP formula](https://www.artima.com/weblogs/viewpost.jsp?thread=210575) ·
[Go coverage](https://go.dev/doc/build-cover)

Keep tests next to the packages they exercise. Use table-driven tests for rules
with clear expected results and `httptest` for HTTP contracts. Run focused
integration tests against real dependencies where their behavior matters.
Exercise the SQLite ledger and stack lifecycle as those features are built.
[ADR 0005](../adr/0005-stack-ledger-and-lifecycle.md)

Verify that the launch API returns a stack ID and reports readiness after
startup, fresh-launch commands, and route publication. The
[opt-in dedicated-host smoke workflow](docs/quick-start.md) probes the published
Caddy URL. External CI can wrap its existing end-to-end tests with the same helper.
[ADR 0006](../adr/0006-git-sourced-launch-and-external-testing.md)

## Fuzz testing

`FuzzHealthz` checks that the health route returns an empty 204 for GET and HEAD
and rejects other methods. For a longer run, use `FUZZ_TIME=1m ./dev fuzz`.
On failure, copy the new input from the report's `fuzz/corpus/` tree to the same
package-relative path in the module. Commit it under `testdata/fuzz/FuzzHealthz/`
so `./dev test-go` replays it. Add future fuzz targets to the fuzz gate with one
`go test -fuzz` invocation per target. [Go fuzzing guide](https://go.dev/doc/security/fuzz/)
