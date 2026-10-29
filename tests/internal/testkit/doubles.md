# Registered test doubles

A double sits only at a boundary this repository does not own, is registered here with its reason, and is
checked against the real thing by a contract fixture (rule TE2).

| Double | Stands in for | Reason | Contract fixture |
| --- | --- | --- | --- |
| `FakeRuntime` (`fakeruntime.go`) | the Docker daemon behind `compose.Adapter` | The operation tests need crashes at exact points, an unreachable target and a vanished volume, none of which Docker can produce on demand, and they must run without a daemon. `RefuseCancelled` mirrors the adapter's refusal of a call made with an ended context; the conformance set checks it on both. | `RuntimeConformance` (`runtimecontract.go`), run on the fake by `tests/unit/operation/runtime_contract_test.go` and on the real adapter by `tests/integration/contract_test.go` |
| `DockerShim` (`docker.go`) | the `docker` binary | Proves that no Docker call happens before a refusal; it records calls and fails them all. | None needed: it never answers a call. |
