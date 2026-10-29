# StackPuddle Control Plane

The execution engine of [StackPuddle](https://github.com/TomazVidovic533/stackpuddle). Today it is a Go library that the
command-line interface runs in-process; there is no listener, login, version handshake or container image yet. The
authenticated operation API, work that continues after the command-line interface exits, and the packaged image are
planned for a later milestone and are not built.

The command-line interface lives in [StackPuddle CLI](https://github.com/TomazVidovic533/stackpuddle-cli). Shared schemas
and the project model live in the main StackPuddle repository.

## What is built

One public package, [`engine`](engine), with five calls:

| Call | What it does |
| --- | --- |
| `Submit` | Brings one environment of a loaded project up, stops it, or destroys it, under a lock and a durable journal. `DryRun` plans without a lock and writes nothing. |
| `Run` | Invokes one job and nothing else: the provider and access steps the job binds, then the job, whose native exit code is recorded on its step. A non-zero code is `job.exited_nonzero`, apart from an engine failure. |
| `Inspect` | Reads an environment or one operation without the lock and without the secrets key. |
| `Cancel` | Writes a cancel request that the running operation notices before its next step. |
| `Advertise` | Lists the capabilities the engine runs: the Docker Compose runtime, the local secret provider and the module drivers. |

Everything else is under `internal/`:

```
engine/                  the only public package; api.txt is its committed surface
internal/
  config/                the one reader of the environment
  durable/               write a file so a crash leaves the old content or the new
  lock/                  flock on env.flock, an owner record, refusal of network filesystems
  state/                 scope.json, state.json, archived operations, cancel requests; strict load
  secrets/ secrets/local the provider port and the sealed file per scope
  module/                the driver port: plain data only
  modules/               the explicit driver list
  tls/                   the environment's CA and server certificates (standard library only)
  plan/                  topology, merge of a selection into revision N+1, diff against the runtime
  operation/             the order of a Submit, recovery, cancel, inspect
  runtime/compose/       the only code that runs Docker; the renderer
```

## PostgreSQL module options

The PostgreSQL driver (`internal/modules/postgres`) runs the server the definition's workload fixes. The engine applies
no memory or CPU limit to it: `resources.memoryMiB` and `resources.cpus` are accepted only at their defaults (1024 and
1), and the container runs unbounded. `settings.maxConnections`, `settings.sharedBuffersMiB`, `hostAccess.enabled` and a
`startupTimeoutSeconds` above the probe budget are refused at plan time. Wiring limits into the workload model is a
separate decision.

## Certificates, binding delivery and jobs

A module that serves encrypted connections asks for a server certificate in its driver's descriptor. The engine issues one
CA per environment and one ECDSA P-256 server certificate per such workload. The private keys live only in the sealed
store; the public certificates are written to `<environment>/tls/`. Consumers get the CA and a per-binding credential as
files copied into the container with the declared owner and mode, never from a host file, and an environment key only
where the binding declares one. A secret mapped to an environment key is refused. See
[ADR-0003](docs/architecture/decisions/0003-tls-and-binding-delivery.md).

An up runs in this order: secrets and certificates, the provider workloads, the module access steps, the prerequisite
jobs, the final access check of every module instance a consumer binds, then the consumers. A failed step ends the
operation, so a failed migration blocks the rollout, and an ordinary restart does not run a job again. See
[ADR-0004](docs/architecture/decisions/0004-job-invocation.md).

## State

A per-user root, `$XDG_STATE_HOME/stackpuddle` or `~/.local/state/stackpuddle` on Linux and
`~/Library/Application Support/stackpuddle` on macOS. Folders are 0700 and files 0600.

```
<root>/environments/<project>.<environment>/
  scope.json        written once: scope uid, Docker engine ID, Compose project, key fingerprint
  env.flock         the lock file; never deleted
  state.json        generation, accepted revision, realizations, the open operation (the journal)
  operations/       one file per finished operation
  cancel/           one file per cancel request
  tls/              public CA and server certificates; private keys are never written here
<root>/secrets/<scope uid>.sealed       AES-256-GCM, one file per scope
```

The files are internal under `stateVersion` 1. An unknown version, field or enum value is refused and the bytes stay as
they were.

## Settings

| Setting | Meaning |
| --- | --- |
| `STACKPUDDLE_STATE_DIR` | Absolute path that replaces the state root. |
| `STACKPUDDLE_SECRETS_DIR` | Absolute path that replaces `<state root>/secrets`. |
| `STACKPUDDLE_SECRETS_KEY` | 32 random bytes in standard base64. StackPuddle never writes it, never regenerates a missing value and has no plaintext fallback. |
| `HOME` | Platform input: the home folder the default state root derives from. |
| `XDG_STATE_HOME` | Platform input: parent of the default state root on Linux when absolute. |
| `PATH` | Searched once for the `docker` binary. |

Docker children receive only an allowlisted environment, so `DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_TLS_VERIFY`,
`DOCKER_CERT_PATH` and `COMPOSE_*` from the caller never change the target. Every call uses the Docker context the
environment declares.

## Checks

```bash
GOWORK=off task check                  # needs no Docker
GOWORK=off task test:integration       # needs a Docker daemon and Compose; disposable projects only
GOWORK=off task check:supply-chain     # govulncheck; needs network access to https://vuln.go.dev
```

Run with Go 1.27.0 or later, Task 3 and a C compiler (the race detector needs cgo). Until the StackPuddle dependency in
`go.mod` is re-pinned to a pushed commit that holds the model package, run the checks against a workspace file outside
the repository with `GOWORK=<file> task check`; `check:tidy` then reports that it did not run.

Linux is the tested platform. macOS paths, `F_FULLFSYNC` and Docker Desktop's file ownership are not verified, and there
is no Windows lock adapter.

## Decisions

- [ADR-0001: Compose renderer](docs/architecture/decisions/0001-compose-renderer.md)
- [ADR-0002: Module driver protocol](docs/architecture/decisions/0002-module-driver-protocol.md)
- [ADR-0003: Server certificates and binding delivery](docs/architecture/decisions/0003-tls-and-binding-delivery.md)
- [ADR-0004: Job invocation](docs/architecture/decisions/0004-job-invocation.md)

## License

Apache-2.0.
