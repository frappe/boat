Boat is the per-host daemon that owns Firecracker microVM lifecycle mechanics for
the Atlas control plane. Atlas holds desired state; Boat holds fact.

For coding taste refer to [llm/Taste.md](./llm/Taste.md). Read it before writing
code — it settles the places where Atlas's rules and Go idiom disagree, so no
package has to settle them again.

## Source of truth

- **`spec/33-boat.md` in the Atlas repo** (`github.com/frappe/atlas`) governs
  this repo's contract: the Atlas↔Boat operation set, the correctness invariants
  (fence epoch, CAS on contended reservations, `desired_power` vs
  `observed_status`, write-ahead journaling), and the dividing rule for what
  Boat may decide. Boat implements that chapter; it does not extend it. Behaviour
  the chapter describes changes in the chapter first.
- **`api/openapi.yaml`** is that contract in machine-readable form — see
  "Generated code" below.
- **`llm/wo-0-contract.md`** fixes the package signatures for the work in flight.
  Several agents build against those signatures at once; if one is wrong, say so
  rather than silently changing it.

## THE RULE

**Every host-side service is written in Go, and every one of them is a separate
systemd unit invoked through the same `boat` binary.** Multi-call binary, the
busybox model: separate units, separate processes, **one build artifact**.

```
ExecStart=/usr/local/bin/boat daemon                # the API daemon + reconciler
ExecStart=/usr/local/bin/boat networkd              # was atlas-networkd.service
ExecStart=/usr/local/bin/boat pool                  # was atlas-pool.service
ExecStart=/usr/local/bin/boat wake-trap             # was atlas-wake-trap.service
ExecStart=/usr/local/bin/boat gateway               # was gateway.service
ExecStartPre=/usr/local/bin/boat vm-network-up %i   # firecracker-vm@ hooks
```

Why this is a rule and not a preference:

- **One artifact means one version per host.** A host cannot run a new networkd
  against an old wake-trap, because they are the same build. Version skew
  between host components stops being a state that exists.
- **One binary swap re-points every unit at once.** That is what makes
  self-update a single atomic act with a single rollback, rather than a
  choreography across five packages with a partial-version window in the middle.
- **It deletes the durable-package staleness bug class outright** — no
  `/var/lib/atlas/bin` module shadowing, no venv, no `sys.path` shims, and none
  of the five invocation styles the host carries today.

So: a new host-side service is **a new subcommand plus a new unit file**. Never a
second binary, never a shell script, never a helper the units invoke directly.
The CLI and the units are clients of the same API surface the daemon serves —
never alternate paths with powers the API lacks.

## Dependency policy

Dependencies are **pragmatic, not stdlib-only**. There is no standing rule
against adding one, and there is a standing rule about how:

- The standard library is the default. A dependency has to buy something the
  stdlib genuinely does not offer.
- It must be **pure Go with no CGO**. The release binary is built
  `CGO_ENABLED=0` and dropped onto a bare Ubuntu host with no toolchain; a
  dependency that needs a C library breaks the static build, which is not a
  preference but a deployment requirement.
- Prefer one small library over a framework, and prefer a library whose
  transitive set you can read in one sitting.
- **Every accepted dependency is recorded in the ledger below, with its
  rationale, in the same commit that adds it to `go.mod`.** This list is what the
  signed-release supply-chain sign-off reads (`spec/33-boat.md` §12,
  `spec/23-supply-chain.md`). A dependency added without its row is not a smaller
  change; it is an unreviewable one.

### Ledger

| Dependency | Why it is here |
|---|---|
| `go.etcd.io/bbolt` | Boat's store. A single-file transactional key/value store with real ACID write transactions, which is what the write-ahead journal needs: the record of a non-idempotent decision must land in the same transaction as the state it justifies, or a crash-then-retry is not deterministic. Pure Go with no CGO, so it does not cost us the static binary — the reason it wins over SQLite. Embedded, so there is no second process to supervise on a host whose whole point is being self-sufficient. Same choice, for the same reasons, that fly.io's flyd made. |
| `github.com/oapi-codegen/runtime` | The support code the generated server in `internal/wire` calls into — parameter binding, `application/json` request/response plumbing. It is a consequence of the IDL decision, not a separate one: `api/openapi.yaml` is the source of truth and the typed server is generated from it, so the alternative to this dependency is hand-writing the marshalling the generator already produces correctly. Pure Go. Its own transitive set (`apapsch/go-jsonmerge`, `google/uuid`) arrives with it and is not separately chosen. |

## Generated code

`api/openapi.yaml` is the contract. `internal/wire/wire.gen.go` is **generated
from it** by `oapi-codegen` and **checked in**, so a build needs neither the
network nor the code generator.

- **Never hand-edit `internal/wire`.** A change to the wire surface is a change
  to the IDL followed by `make generate`; an edit to the generated file is
  reverted by the next regeneration and is invisible in review until then.
- The generator version is pinned in the `Makefile` so the checked-in output is
  reproducible.
- The generated types are not the persisted types. `internal/model` is what the
  host remembers; `internal/wire` is what Atlas sees. Mapping between them is a
  few lines in `internal/api` and is worth paying to keep the store free to grow
  checkpoints and fence epochs the API has no business exposing.

## Working here

- `make check` (gofmt + vet + tests with `-race`) before claiming anything is
  done. `make build` produces the static host binary; `make generate` refreshes
  the wire package.
- Host packaging lives beside the code and is reviewed with it:
  `systemd/boat.service` and `sudoers.d/boat`. Boat runs **non-root** under a
  pinned NOPASSWD allow-list — a new privileged command means a new enumerated
  line in `sudoers.d/boat`, with a comment, never a widened wildcard.
- `README.md` describes what actually ships today. Keep it that way: it is a
  walking skeleton and it should not read like a finished system.
