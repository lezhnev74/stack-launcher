# 0001. Host and isolation

Status: Accepted.

## Context

The team needs to run its own application stacks for automated tests, manual
testing, and demos. Multiple versions of the same application must be able to
run side by side.

## Decision

- Run rootless Podman on one dedicated Linux host. Application containers
  share the host kernel.
- Give each stack its own application containers, network, and data volumes.
  Different stacks can reuse service names and internal ports.
- Treat teammates and authorized CI jobs as one trust group. Anyone allowed
  to launch can access and operate all stacks. Run trusted code with test
  data and sandbox credentials.
- The team arranges access to the host separately.

## Consequences

The stack boundary prevents routine collisions between applications. A
requirement to run untrusted code or keep stacks private from other launcher
users would require revisiting the isolation model.
