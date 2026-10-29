# ADR-0002: Module driver protocol

- Status: Proposed
- Date: 2026-10-29
- Realization: Partial. The port exists in `internal/module`; the PostgreSQL driver in `internal/modules/postgres` is the first built-in driver.

## Context

Built-in drivers are the first implementations of the contract a staged container runner implements
later. A driver that held a runtime handle would be a privileged shortcut no remote runner could copy.

## Decision

A driver has five methods: Describe, Observe, Plan, Apply and Verify. They exchange plain
JSON-serialisable data only. A driver asks for runtime work as probes (exec in a scope container, or a
one-shot container on the scope network) and never holds a Docker handle; the engine runs each probe
and calls the driver again with the results, at most eight rounds. Delete is a capability that
Describe reports, not a sixth method. A driver's actions and state kinds are namespaced by its module.

Two fields serve the first driver that provisions credentials. `Instance.Scope` carries the uid of the
scope the instance runs in, so the ownership markers a driver writes name their scope. A
`SecretBinding` has a `Form`: empty binds the secret's own bytes, and `scram-sha-256` binds the
RFC 5802 verifier the engine computes from the secret with a random salt, so a driver can set a
password without holding the plaintext. The plaintext and the verifier exist only in the probe
child's environment. Any other form is refused as out of scope before the probe runs.

## Problem signal

An out-of-process runner must speak the same contract.

## Rejected alternatives

- Passing a runtime interface to drivers: a handle no remote runner can receive.

## Consequences

- A request and probe loop bounded at eight rounds; more is an effect failure.
- A driver never sees a secret value it did not create; the engine resolves probe secrets into the
  child's environment only.

## Verification evidence

A JSON round-trip test over every exported type of the port, a test driver that completes Observe
through an exec probe, the RFC 7677 vector for the verifier, and a test that an unknown form is
refused before any probe runs.
