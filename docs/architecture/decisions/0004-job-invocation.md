# ADR-0004: Job invocation

- Status: Proposed
- Date: 2026-10-29
- Realization: Built. `engine.Run`, `internal/operation/run.go`, the `RunJob` result of the runtime port.

## Context

A deployment migration is a job. It must run after the module's access exists and before the
consumers start, its failure must stop the rollout, and an ordinary restart must not run it again. The
operator also needs to run a job on demand. In both cases the job's own failure is a different thing
from the engine failing to run it.

## Decision

**One path.** A job is a workload of type job. `RunJob` runs it as a one-shot container through the
runtime port. The adapter returns the job's native exit code in `Evidence.Job` whenever the job's
container ran to its end, zero or not. Only a job that could not be run is an error, and that is
`runtime.effect_failed`. A non-zero exit code is `job.exited_nonzero` (category conflict) and is
recorded on the step as `Job.ExitCode`.

**When a job runs.** An up plans a job only as a prerequisite of a service that is up, and only when its
spec hash changed. Naming a job in a selection or a group plans nothing. `engine.Run` runs the one job
it names even when the hash is unchanged. It records an operation with the command `run` holding the
provider and access steps the job binds and exactly one job step, and starts no consumer.

**Identity.** The invocation ID is `<operation id>/<step id>`: durable in the journal, and every
invocation is a new operation. There is one attempt per invocation, because the model has no retry
field; a retry is a new invocation.

**State.** `state.Step` gains the optional member `job`. `stateVersion` stays 1: no engine that reads
version-1 state has been released, so no reader can meet the member. It must be bumped for any later
change after the first push.

## Problem signal

A job retry policy in the model, or job arguments, change the request and the step.

## Rejected alternatives

- Reading the compose exit code as the job result: it cannot tell the job's exit from a Compose failure.
- Running every job of the project on an up: an unrelated job would run because a group was selected.

## Consequences

- The command-line interface maps `job.` codes and the new command in the operation schema when it first emits them.
- A recovered job whose container exited zero with the recorded spec hash is settled by observation.

## Verification evidence

`tests/unit/operation/jobs_test.go`, `tests/unit/plan/diff_test.go`, the runtime conformance scenario
for jobs against Docker, and `tests/integration/delivery`.
