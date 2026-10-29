# ADR-0003: Server certificates and binding delivery

- Status: Proposed
- Date: 2026-10-29
- Realization: Built. `internal/tls`, the Bind method of the driver port, `internal/plan/delivery.go`, the file rendering in `internal/runtime/compose` and the `tls.issue` and `module.verify` steps in `internal/operation`.
- Extends: [ADR-0002](0002-module-driver-protocol.md), which is not edited.

## Context

A consumer needs the address of a provider, the values it exports and the certificate authority it
must trust. A provider that serves encrypted connections needs a certificate and a private key. Both
must reach a container without a host file, a bind mount or an environment key that `docker inspect`
shows, and a secret must stay in the sealed store.

## Decision

**One certificate authority per environment.** `internal/tls` issues an ECDSA P-256 CA valid for ten
years and one server certificate per workload valid for one year. Every certificate starts one hour
before the issue time. A server certificate carries the instance-scoped service name, plus `localhost`
and `127.0.0.1` only when the environment exposes the workload on the host. Keys come from
`crypto/rand`, never from the engine's `Random` seam. A server certificate is reissued when its names
change, when the CA changes or when fewer than thirty days remain. Nothing rotates the CA in m01.

| Material | Where | Data class |
| --- | --- | --- |
| CA private key `tls/ca-key`, server key `tls/server/<workload>` | the sealed secret store, through `Store.CreateValueIfAbsent` | secret |
| CA and server certificates | `<environment>/tls/<name>.crt`, written through the environment's `os.Root` | public |

The `tls.issue` step runs after the secret steps and before any workload. It stores the key before it
writes the certificate, and a repeat reuses the stored key, so a process killed between the two writes
leaves nothing to repair.

**The driver port gains Bind.** `Bind(BindRequest) (BindResponse, error)` returns the values one Export
of an instance delivers. It runs no probe. Each `BoundValue` is exactly one of `Text`, `SecretRef` or
`Material`; the only material is `tls-ca`. `Descriptor.ServerTLS` names the workloads that need a
certificate and the owner of the files. The engine refuses a response with a missing, extra or
doubly-set value, an unknown material, a secret outside the instance's namespace, or a kind that does
not match the Export's declared sensitivity. Binding parameters are untrusted input: a driver error
is `binding.parameters_invalid`, raised at plan time, before any effect.

**One delivery path.** `host` and `port` come from the resolved binding. A declared value of a module
comes from its driver; a declared value of a plain workload has no source and is refused as
`delivery.value_unavailable`. Every file becomes a Compose secret sourced from a numbered child
variable with `uid`, `gid` and `mode`, so Compose copies it in. The topology stores the source and its
reference, never a value. An environment key carries public text only: a secret mapped to one is
`delivery.secret_in_environment`, and a key or path that two paths set is `delivery.conflict`. The
process environment is never read, and every `$` of a model string stays doubled in the rendered file.
The label `io.stackpuddle.files` holds a digest of the version of each delivered source (a secret
version ID, a certificate fingerprint), so a reissued certificate or a rotated secret changes the spec
hash and restarts the workload; an unchanged one restarts nothing.

**Fixed targets.** A server's certificate and key arrive at `/run/stackpuddle/tls/server.crt` (0444)
and `/run/stackpuddle/tls/server.key` (0600), owned by `ServerTLS.Owner`. Module content refers to
these paths.

**Order of an up.** Secrets and certificates, the provider workloads, the module access
steps, the prerequisite jobs, `module.verify` of every instance a consumer binds, then the consumers. A
failed step ends the operation, so a failed migration blocks the rollout. An up that changes nothing
adds no `module.verify` step.

## Problem signal

A module that needs the CA in its consumers as a file, or a consumer that needs a different
credential per binding, cannot be served by environment keys alone.

## Rejected alternatives

- Bind mounts of certificate files from the host: a host file, and the owner and mode are the host's.
- A driver that writes the certificate through a probe: the key would pass through the driver.
- `DATABASE_URL` with the password: it shows in `docker inspect`. A consumer that wants it cannot have it in m01.
- Starting the consumers before the access steps: a consumer could connect to a role that does not exist yet.

## Consequences

- Every driver, including the test driver, implements Bind.
- Copy delivery on Linux is proven here; macOS Docker Desktop is a named verification gap.
- A host-context endpoint is never delivered; the engine delivers into workloads only.

## Verification evidence

`tests/unit/tls`, `tests/unit/plan/delivery_test.go`, `tests/unit/compose/files_test.go`,
`tests/unit/operation/{delivery,tls,jobs}_test.go`, `tests/unit/postgres/bind_test.go`, the runtime
conformance scenario for files and jobs, and the Docker tests in `tests/integration/tls` and
`tests/integration/delivery`.
