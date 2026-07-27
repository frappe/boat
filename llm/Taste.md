Guidelines for writing good code in Boat

This is Atlas's `llm/Taste.md` rendered into Go. The standards are the same; the
idiom is the one a Go reviewer would apply. Where a rule and Go genuinely
conflict, the conflict is named and settled here — once, in this file — rather
than being re-decided differently in every package.

## Shape

1. Choose clean code over clever code. Reflection, `interface{}` plumbing,
   code generation you wrote yourself, and channel choreography are all ways of
   moving a problem somewhere a reader cannot see it. If a reviewer has to
   simulate the program to know what it does, it is not finished.
1. Give behaviour a home. A type owns a responsibility and its methods are how
   you ask it to do the job: `vm.Manager` starts and stops, `store.Store`
   remembers, `run.Runner` runs. Free functions are for pure derivation
   (`paths`, `run.Quote`). This is Atlas's "write object oriented code" without
   the ceremony Go does not want: no getters and setters, no interface per type,
   no inheritance simulated with embedding.
1. Keep functions small, ideally 10 lines. A function that needs a blank line to
   separate its phases is two functions.
1. Keep files small, between 100 and 300 lines, and name them after the idea
   they hold. One `vm.go` carrying start, stop and observe is three files
   (`start.go`, `stop.go`, `observe.go`); their tests sit beside them.
1. Keep packages small, fewer than ~15 files. A package is one responsibility
   with a name to match. There is no `util`, `common`, `helpers`, or `types`
   package — those names are what a missing responsibility looks like.
1. Avoid abbreviations. `virtualMachine`, not `vm`; `operation`, not `op`;
   `identifier`, not `id`. The exception is the same one Atlas grants: a local
   inside a five-line scope, where the declaration is on screen with every use.
   Go's initialism convention still applies and is not an abbreviation:
   `UUID`, `API`, `HTTP`, `URL`, `ID` are all-caps when exported and all-lower
   when not — `APISocket`, `apiSocket`, `uuid` — never `Uuid` or `Api`.
1. Use the standard library as much as possible. `net/http`, `os/exec`,
   `encoding/json`, `log/slog`, `context`, `errors`, `testing` are the house
   framework. Dependencies are pragmatic rather than forbidden, but every one is
   argued and recorded in the ledger in `CLAUDE.md`; reach for the ledger before
   you reach for `go get`.
1. Reuse. Write as little code as possible. Most of Boat is a port, not a
   design: the Python host surface (`scripts/`, `scripts/lib/atlas/`) is the
   conformance oracle, and a Go module is right when it renders the same
   commands the Python one did. Port the WHY comments with the code — they
   record host-verified facts (uutils `install` cannot read `/dev/stdin`;
   `TimeoutStopSec` is load-time only; a failed restore consumes its marker)
   that no amount of Go taste rediscovers.
1. Build the minimum working thing, then iterate towards the goal. WO-0 starts
   and stops one VM. Everything the walking skeleton does not need is a later
   work order, not a hook left in for it.
1. Always write tests, and make sure they work. They live next to the code they
   cover: `start_test.go` beside `start.go`, in the same package unless the test
   is genuinely a consumer of the API. Table-driven where the cases are data.
   `make check` runs them with `-race`.
1. A test must not need a host. That is why `internal/run` is the only package
   that runs a subprocess: everything else is pure functions over strings.
   Preserve that by passing a `*run.Runner` in rather than constructing one deep
   in a call tree, and by taking narrow interfaces where a test wants a fake.
1. Comments explain WHY. What the code does is already written down; a comment
   earns its place by recording the reason, the constraint, or the failure that
   made this the shape. `// increment the counter` is noise; `// is-active after
   start, because systemctl start returns before the service settles` is the
   only place that fact exists.

## Semantics

These are the rules Boat holds about behaviour rather than about form. They are
not stylistic and a review should not treat them as negotiable.

1. **One operation = one verb = one op record.** The Go analogue of Atlas's "one
   operation = one shell script = one Task row". A verb is the unit of work, of
   idempotency, and of audit: it takes a UUID, does the whole job, and leaves
   exactly one record behind. Compose **inside** a verb — a helper, a loop, a
   sequence of steps in one function — never by having a caller chain two RPCs
   and hope. Two verbs that are always called back to back are one verb;
   a caller that has to run three of them in order is a missing verb. Chained
   RPCs have no single record, no single retry, and no defined state after the
   second one fails.
1. **Every verb is idempotent.** Retry means re-run. Starting a running VM
   succeeds; stopping a stopped one succeeds; re-posting a completed
   `operation_id` returns the first result and runs nothing. There is no repair
   mode, no `--force`, no "fix" variant of a verb — a second code path that only
   runs after something went wrong is the path that is never tested and never
   right.
1. **Fail loud at the boundary; never fall back.** A `sudo systemctl` that
   failed, a Firecracker socket that refused, a store that will not open: return
   the error, say what failed once, and let the operator retry. Do not degrade,
   do not substitute a default, do not log-and-continue. A degraded success is
   worse than a clean failure because it hides the fault from the only system
   that could fix it — Atlas sees `Success`, stops retrying, and the host stays
   broken. `err != nil` is never discarded silently; if it is genuinely
   best-effort, say so in a comment naming the backstop that makes it safe.
1. **Observed state is read, never inferred.** A VM's status comes from the
   host — the unit's `ActiveState`, the on-disk markers — never from the fact
   that a command exited zero. Atlas used to set status from Task success, which
   made the control plane a record of its own intentions. Boat exists to end
   that; do not reintroduce it one field at a time.
1. **Boat knows a VM as a UUID plus numbers.** If a decision would come out
   differently depending on who owns the VM, what runs inside it, or what it
   costs, that decision is Atlas's and the answer arrives in the request. Guest
   identity crosses the boundary as opaque bytes Boat writes without parsing.
1. **The spec chapter is the source of truth.** `spec/33-boat.md` in the Atlas
   repo governs this repo's contract, and `api/openapi.yaml` is that contract in
   machine-readable form. Behaviour the chapter describes changes in the chapter
   first. If the code and the chapter disagree, that is a bug in one of them and
   it gets reported, not reconciled quietly in a handler.

## Where Go wins, and where Atlas wins

Two real tensions, settled:

- **Receiver names — Atlas wins.** Go idiom is a one- or two-letter receiver
  (`func (m *Manager)`). Boat spells it: `func (manager *Manager)`,
  `func (virtualMachine VirtualMachine)`. A receiver is just a parameter, and
  "avoid abbreviations" exists so that a reader landing in the middle of a file
  knows what a name is without scrolling. The cost is a few characters per
  signature; the alternative is a codebase where the no-abbreviations rule has a
  hole in it exactly where the most-used identifier lives. Go's other receiver
  rule still holds: the name is the **same** on every method of a type.
- **`ctx` and `err` — Go wins.** `ctx context.Context` is the first parameter of
  anything that blocks, and errors are `err`. Spelling them out fights the
  language rather than an abbreviation habit: `context` shadows the package and
  `error` shadows the builtin type, so the "clear" name is the one that breaks.
  These two are the whole exception list; `vm`, `op`, `cfg`, `svc`, `req` are
  not on it.
- **Package names — Go wins.** Packages are short, lowercase, one word, no
  underscores, no plurals: `vm`, `run`, `api`, `store`, `paths`. `internal/vm`
  stays `vm` because `virtualmachine.Manager` reads worse at every call site
  than `vm.Manager` does, and because a package name is read *with* the
  identifier that follows it. The rule that keeps this honest: the short name is
  the **package**, never an identifier inside it. `vm.Manager`, not
  `vm.VMManager`; inside the package the variable is still `virtualMachine`.
- **Interfaces belong to the consumer.** Declare the narrow interface where it
  is used (`api.OperationStore` names the four methods the handlers need), not
  beside the implementation. Accept interfaces, return concrete types. This is
  also the reuse rule: the interface stays small because it is defined by a real
  need instead of by everything a type happens to do.
- **Errors are values.** Wrap with `%w` and add the context the caller lacks;
  compare with `errors.Is`/`errors.As`, never with string matching. No `panic`
  in library code — a panic in a daemon that supervises live VMs takes the
  daemon down. The stubs that currently panic are marked as stubs and are the
  only exception until they are written.
- **`gofmt` is not a style opinion.** It is the format. `make check` fails on
  unformatted code, and nobody argues about braces.
