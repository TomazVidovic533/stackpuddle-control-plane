# ADR-0001: Compose renderer

- Status: Proposed
- Date: 2026-10-29
- Realization: Planned. The renderer is written after this record and is covered by the two-instance
  and same-second-scope tests.

## Context

The engine runs the workloads a project declares and the workloads module instances contribute, on
Docker Compose, and must keep a later Kubernetes adapter additive. If the Compose adapter knew
module names, every new runtime would need the same product branches.

## Decision

Module instances contribute generic workloads and resources. One renderer translates every workload
to Compose JSON under instance-scoped names:

- The Compose project is `<project>-<environment>-<suffix>`, where the suffix is six lowercase
  characters from the random part of the scope uid's ULID, never its `sc-` or time prefix.
- Service names are `<instance>--<workload>`, volume names `<service>--<volume>` and secret names
  `<service>--<name>`. Keys never contain `--`, so two instances of one module never collide.
- Every string taken from the model is written with `$` doubled, so Compose interpolation cannot
  copy a value from the child's environment, such as a secret, into a plain variable. No `${VAR}`
  or `${VAR:-}` fallback is ever rendered.
- Secrets arrive only as top-level Compose secrets sourced from `SP_SECRET_n` variables that exist
  in the child process's environment for that one call. No host secret file and no bind mount.
- Data volumes are created by the engine with Compose's own project and volume labels and are
  adopted by `up`, so `down -v` removes them with the project.
- Ports are published only when declared, on `127.0.0.1`.
- The renderer has no module-specific branch and emits only fields it models.

## Problem signal

Two instances of one module in one project; two state roots on one Docker engine.

## Rejected alternatives

- A Compose file per module: a product switch inside the core that every new runtime would repeat.
- `external: true` volumes: `down -v` would not remove them, which breaks the disposable-project
  contract.

## Consequences

- Names are long and not guessable; `inspect` shows them.
- Compose, not the engine, decides container creation order within one `up` call, so the engine
  starts services one at a time with `--no-deps`.

## Verification evidence

The integration tests render and start two instances of one project-local definition without a name
collision and create two scopes in the same second with different Compose project names. Unit tests
cover the `$` escaping table.
