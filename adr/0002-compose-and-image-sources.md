# 0002. Compose and image sources

Status: Accepted.

## Decision

Use a Compose manifest for services, startup dependencies, and health checks.
Run the manifest through `podman compose` with one configured Compose provider.
[ADR 0006](0006-git-sourced-launch-and-external-testing.md) defines how to
supply the manifest and how the launcher accesses Git.

Choose how to prepare each application service's image:

- **Registry by default.** Pull the image declared in Compose or supplied by
  the launch caller. A caller override can supply a service's only image. CI
  builds and pushes images and supplies their fully qualified references,
  preferably by digest. Configure a persistent auth file with a pull-only
  registry credential for the rootless Podman account. When
  `registry_auth_file` is omitted, Podman uses its normal account credential
  lookup. CI holds its own push credential. The default `pull_policy: always`
  requires a successful registry pull. A service can opt into
  `pull_policy: newer` to use Podman's registry-first behavior with fallback to
  the same reference in local storage when a pull fails.
- **Build when a Git ref is supplied.** Declare the service repository and ref
  under `x-launcher.git` together with Compose `build`. Resolve the ref using
  the launcher's existing GitHub SSH access. Check out the resolved commit
  into a temporary launcher-owned directory and build on the host using the
  service's Compose `build` settings. Use the same rootless Podman image
  storage and keep layer caching enabled across launches. Cache reuse depends
  on the build inputs and the layers still present on the host.
- **Prefer a CI image for a Git build.** Set `x-launcher.git.prebuilt_repository`
  to the fully qualified registry repository for that build recipe. CI publishes
  immutable `git-<full-commit-sha>` tags there. Resolve the Git ref once, look up
  that commit's tag, and pull by the returned manifest digest. On a confirmed
  registry miss (`MANIFEST_UNKNOWN` or `NAME_UNKNOWN`), build the same commit
  locally. Report other lookup failures and pull failures. Keep the local
  build output separate from the CI tags.

A stack can mix these choices. Prepare the selected images before starting
the stack. If image preparation fails, report the service and the failed
operation. Record the immutable image ID per service. For a Git-backed service,
also record the repository and resolved commit, and save the digest reference
when a pull supplies the image. Start reuses the saved image ID.

Declare exactly one HTTP endpoint for Caddy in the source Compose file:

```yaml
x-launcher:
  endpoint:
    service: web
    port: 8080
```

The launcher validates the service name and reads the port as the HTTP port
inside that service's container. Each stack gets its own subdomain. The
connection between Caddy and the endpoint is defined in
[ADR 0003](0003-caddy-routing-and-host-ports.md).

[Podman Compose](https://docs.podman.io/en/latest/markdown/podman-compose.1.html) ·
[Registry login](https://docs.podman.io/en/latest/markdown/podman-login.1.html) ·
[Podman build layers](https://docs.podman.io/en/latest/markdown/podman-build.1.html#layers) ·
[Compose extensions](https://docs.docker.com/reference/compose-file/extension/)
