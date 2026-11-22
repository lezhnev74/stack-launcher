# 0004. Runtime environment files

Status: Accepted.

## Decision

Server admins maintain one private directory of named runtime environment
files on the launcher host. Each Compose service names the file it needs
using the standard `env_file` field as a filename or ordered list of filenames.

```yaml
services:
  web:
    env_file: checkout-dev.env
```

The launcher resolves each filename against `env/` inside the configured `state_dir` and
substitutes the resulting path in the launch copy of the Compose file.
The source manifest contains only filenames. The server supplies the
directory automatically. Filenames must resolve to files within that
directory. If a file is missing or unreadable, launch fails with the service
and filename.

Compose passes the values to the declared service as process environment
variables at container creation. Services can reference the same file or
different files. Keep the values out of source, build inputs, launcher records,
and launcher logs.

## Operator overrides

A launch accepts an optional map of environment values per service, empty by
default. Store these operator-provided values in plaintext in the SQLite
ledger from [ADR 0005](0005-stack-ledger-and-lifecycle.md). Operators can edit
overrides while a run is a draft and can view the saved overrides after launch
begins.

An explicitly supplied override takes precedence over the matching key from
Compose or an admin env file for that stack. Omitting or removing an override
restores the default. Each container uses the saved overrides for its service
when created. For environment configuration, the UI and API
expose only override values. Server admins can read the default files
directly from the host shell.

[Compose service environment files](https://docs.docker.com/reference/compose-file/services/#env_file)
