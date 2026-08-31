# Contained execution for `run_code`

**Status**: implemented 2026-08-31, off by default (`kernel_contain_cells`).

Outstanding, and it is a decision rather than a task: the deployed NetworkPolicy
was checked and closes the cluster-internal half only. `ses-prod-rke2` has
`jupyterhub/singleuser` binding ODE's pods, with enumerated egress to the hub,
proxy, DNS, trainer, MLflow storage and the ingress — and one blanket rule
allowing `0.0.0.0/0` minus the private ranges on every port, which is z2jh's
`nonPrivateIPs` default. Containment as deployed therefore withholds authority,
not confidentiality. Closing the rest means turning that default off and
enumerating what a training cell legitimately fetches (PyPI, the open datasets
§5.10 pulls in), which restricts the work and is not a free tightening. See
`docs/authorisation-and-exposure-tiers.md`.

## Goal

A developer inspecting data in their own pod is not asked. Measured target: 84%
of cells run without a confirmation, against 13% today.

## Why not a better recogniser

Measured over 261 decided `run_code` confirmations (`TestCorpusProbe` in
`pkg/plaincode`):

| approach | runs unasked |
|---|---|
| today | 13% |
| + a third-party Python parser | 14.6% |
| + attribute resolution against the developer's checkout | 29% |
| contained execution | 84% |

A parser is worth four cells. The ceiling on *any* recogniser is 29%, because 147
of the 261 cells hold `subprocess`, `sys`, `importlib`, `urllib` or an `open` in
write mode — and half the rest name members of the developer's own module, an
open set a fixed list never catches up with.

So the question changes. Not "is this cell recognisably an inspection" — which
Python cannot answer soundly, and which `pkg/plaincode` says in its own doc
comment it does not try to answer — but "does this cell need the platform token".
That one has an answer, and it does not need to be predicted: run the cell
without the token and find out.

Rejected: a third-party parser for the gate. Recorded here because the option was
researched rather than assumed — `go-python/gpython` cannot parse f-strings, the
walrus, `match` or `async` (grammar targets ≤3.5), so it would *lower* recognition;
`tree-sitter/go-tree-sitter` needs CGO against `CGO_ENABLED=0` in the Dockerfile;
`odvcencio/gotreesitter` (MIT, pure Go, Python among 206 embedded grammars) is
viable and is the one to reach for if a parser is ever wanted — but for indexing
the developer's module, never on the gate path, where a bad parse would decide
whether a human is asked.

## Approach

The token is not in the pod spec. `pushEnvironmentLocked`
(`pkg/kernel/session.go:1013`) installs it into the *running kernel* with a silent
cell, guarded by `bench.pushedToken`. That is what makes this cheap: containment
is a property ODE already controls at the Python level, not a second pod.

1. **Withhold the token by default.** A bench starts with `ODE_WORKSPACE` and the
   platform URLs but no `SENERGY_TOKEN`. `singleuser-image/ode_platform.py:27`
   already raises a clear error when it is unset, so a cell that needs it fails
   legibly rather than mysteriously.
2. **Run contained cells without asking.** No confirmation, no recogniser.
3. **On a token failure, ask once and re-run.** The confirmation card says what it
   has always said, and the developer's approval is what installs the token for
   that cell.
4. **Withdraw the token after the cell.** Same silent-cell mechanism in reverse,
   so the bench returns to contained. The namespace is untouched, which is the
   point: data a confirmed cell fetched stays inspectable by contained cells
   afterwards, and that is the whole use case.
5. **Shrink `pkg/plaincode`.** It stops being the gate. `CredentialPath` stays
   exported — `pkg/tools/repo.go:187` uses it independently of auto mode.

One kernel, not two. Two would put the fetched dataframe in a namespace the
inspecting cell cannot see, which breaks the case this exists for.

## What this does not contain, and must be said plainly

**Egress is not ODE's to close, and the deployed policy does not close it.**
Checked rather than assumed: `jupyterhub/singleuser` on `ses-prod-rke2` allows
`0.0.0.0/0` except the private ranges, on every port. A contained cell can POST a
dataframe out or fetch remote code. With only the token half, the honest claim is
"the credential the confirmation exists to protect is absent", which is narrower
than Claude Code's sandbox and must not be described as equivalent.

**A confirmed cell can stash the token** in a variable, after which contained
cells can use it. Same standing as the existing redaction hygiene, which
`docs/authorisation-and-exposure-tiers.md` already concedes: "Code that
deliberately encodes the token defeats it, and nothing here pretends otherwise."

**Contained is not read-only.** 49 of the 218 contained cells write into the
workspace, and that is how a developer's work persists. Workspace writes stay.

## Not doing

Flipping `CGO_ENABLED`. Widening the vocabulary further. Attribute resolution
against the checkout — it becomes uninteresting at 84% and is dropped unless
containment turns out not to land.

## Risk

The failure mode inverts. Today a wrong answer costs an unnecessary prompt; here
it costs an unnecessary execution. What holds the line is that the execution
happens somewhere with no credential in it, so this is only sound to the extent
step 1 and step 4 are airtight. Two tests carry that and must fail loudly:
a bench that never had a token pushed has no `SENERGY_TOKEN` in `os.environ`, and
a bench that had one pushed for a confirmed cell has none after it.

Rollback is the config flag: default off, so a deployment that has not applied the
NetworkPolicy keeps today's behaviour.

## Verification

- `TestCorpusProbe` before/after, with the new date in `gateChanges`.
- New `pkg/kernel` tests for the two properties under Risk.
- `pkg/kernel/kernel_test.go:287` already asserts the token is not echoed back;
  it must keep passing.
- The three bounding properties in `pkg/tools/dispatch_test.go`.
- A live cell through `pkg/kernel/live_test.go` proving a contained cell runs and
  a token-needing one fails the way the plan says.

## Note for whoever measures this later

`gateChanges` in `pkg/plaincode/corpus_probe_test.go` is the dated list of every
change to what `run_code` asks about. Add the containment date to it when this
lands. A rate measured across that date is two different questions added
together, and the list is the only thing that makes the split visible.
