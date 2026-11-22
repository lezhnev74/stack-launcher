# Compose manifest

Save this as `compose.yaml` or `compose.yml`. Replace the image and port for
your application.

```yaml
services:
  web:
    image: docker.io/library/nginx:1-alpine
x-launcher:
  endpoint:
    service: web
    port: 80
```

Declare one `x-launcher.endpoint`. Set `service` to a name under `services` and
`port` to its HTTP port inside the container. HTTPS is the default. Set
`https: false` under `endpoint` for HTTP-only access.

## Choose an image source

- **Registry image.** Set `image` to a registry reference. A run-level image
  override wins over the manifest.
- **Git build.** Pair `build` with `x-launcher.git`. This makes the launcher
  ignore any `image` on that service. A run can override the Git ref.
- **Local directory.** Build and push an image from that directory, then use
  its registry reference in `image`.

To build from Git, replace the `web` service above and set `endpoint.port` to
your app's HTTP port.

```yaml
web:
  build: .
  x-launcher:
    git:
      repository: your-org/your-app
      ref: main
```

`build: .` uses the root of the Git checkout. Build paths are relative and must
stay inside that checkout. The launcher needs SSH read access on GitHub.
Set `ref` to a branch, tag, or full commit SHA.

Set `prebuilt_repository` under `x-launcher.git` to a registry repository
such as `registry.example.com/team/web`. The launcher tries its
`git-<full-commit-sha>` image first. A confirmed miss builds that same commit.
Other lookup or pull errors fail the launch.

## Launch

1. Open `/ui` on a [running launcher](README.md#quick-start). Choose
   **Templates → New template → Import YAML**. If the YAML changed, select
   **Update services**. Name the template and select **Save template**.
2. In **Runs**, choose the template, select **Create run**, and **Launch run**.
   Open the URL under **Access** when ready.

Repository-relative mounts, configs, and secrets require a
[GitHub manifest reference](app/README.md#manifest-sources) via `POST /stacks`.
See the [Compose reference](app/README.md#compose-preparation) for supported
fields, ports, environment, and launch commands.
