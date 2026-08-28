# Running the tests

What the suite covers, how to run each half, and which checks exist to catch a
specific class of mistake rather than to raise a number.

## Applies when

Running or extending the test suite. The contract-fixture check is the part worth
reading before adding a field to an API response.

**Not this if**: the question is what testing standards require in general.
This document is only what *this* repository does.

`geltung`: `einzelfall`.

```bash
go generate ./...            # only after changing a route or an annotation
go test -race ./...          # backend
cd frontend && npm test      # frontend: the unit suite (vitest)
cd frontend && npm run build # frontend: type-check and bundle
```

The frontend build is also a **contract test**. `frontend/src/__contract__` holds
JSON captured from a running backend, assigned to the types the SPA declares for
those endpoints, so a renamed or dropped field fails the build instead of
becoming `undefined` in front of a developer. It earned its place on the first
run by catching four defects, and caught a fifth on the M3 pass — a `duration_ms`
field carrying nanoseconds. See the README in that directory, including how to
recapture the fixtures when the shape changes on purpose.

**Frontend tests.** `npm test` runs Vitest over the SPA's runtime logic, in jsdom
where a document is needed. It covers the router — sticky `?session=` in an href,
the base path under `/` and under `/ode/`, and which movements push a history
entry and which replace one — the resolution of a path to a pane, including a
route whose backend this deployment does not serve and an address that names no
view, and the formatters that render durations, sizes and ratios.

`theme.test.ts` reads `index.css` and asserts the shape of the theme: three
scopes rather than shadcn's two, every colour the light theme names restated by
both dark ones, and a `--series-n` for each of the eight lines a chart can draw.
That last set is the failure mode worth a test — a token added to one scope and
not the others is a colour that keeps its light value on a dark page, which is
legible in the theme its author had open and wrong in the other.

Several panes are mounted and read: the chat pane and its conversation, the code
pane, the workbench bar, and the experiments run document, whose three-state
criteria and empty comparison are assertions about rendered output rather than
about the helpers underneath it. Those tests select on the semantic class names
the markup still carries — `form.composer`, `.turn.ode`, `button.session-open` —
which is why those names survived the move to Tailwind: they are hooks now rather
than styling, and removing one is a test-visible change rather than a silent one.

What it does not cover is most of the SPA: the WebSocket layer and the profiler
and exploration views have no tests of their own, and nothing here runs in a
browser, so a rule that renders differently in Firefox than in jsdom's model of
CSS is still found by a person. The suite is narrow on purpose — a broad shallow
one would cost more to maintain than it would catch.

The fixtures are regenerated from the API test harness rather than a platform:

```bash
ODE_WRITE_CONTRACT=$PWD/frontend/src/__contract__ \
  go test ./pkg/api/ -run ContractFixtures
```

The backend tests use no containers and no network. The device repository is a
fake and test tokens are minted unsigned — deliberately, since signatures are
the gateway's concern — so the suite runs in a few seconds without the platform.
The M3 packages hold to the same rule: the LLM providers are exercised through a
scripted fake that returns a fixed event script, so a whole tool loop runs with no
API key, and the Postgres stores are only wired in `pkg.Start` — the tests run the
memory implementations of the same interfaces.

The MCP tests are the exception worth knowing about: they run a real MCP client
against a real server over `httptest`, because the property being checked is that a
second transport enforces the same gate, and a fake client would only prove the
server agrees with itself.

M4 has the same shape and one addition. `pkg/kernel/kerneltest` is a JupyterHub
and one singleuser server in memory, speaking the real protocol rather than a
simplification of it — a spawn that is pending before it is ready, a scoped
token, an execute that produces busy / input / stream / reply / idle in that
order — because everything worth testing in `pkg/kernel` depends on that
ordering. It is a package rather than a test file because `pkg/api` needs it too,
and two copies of a protocol double would drift.

The addition is a handful of tests that do need a cluster, skipped unless it is
there:

```bash
ODE_JUPYTERHUB_URL=http://proxy-public.<hub-namespace>.svc.cluster.local \
ODE_JUPYTERHUB_TOKEN=... ODE_JUPYTERHUB_USER=... \
  go test ./pkg/kernel/ -run Live -v
```

They exist because the parts of §5.6 most likely to be wrong are the ones a fake
cannot check: whether the credential really holds the scopes, whether the
WebSocket handshake races the kernel's channel bridge, and above all whether a
file written in one session is present in the next. The last one restarts the
kernel and reads the file back, which is the acceptance criterion itself. A
mistyped Hub field — `progress_url` as an integer — was caught on their first
run and would have passed every unit test in the package.

M7 takes the same idea further, because most of what it does is not ODE's code.
The repo tests fake GitHub's API and JupyterHub, and fake nothing else: the remote
is a **real bare repository** in a temporary directory, `git` is a real git, and
the cell ODE sends into the pod is executed by a **real python3** against a real
filesystem. So a clone, a commit and a push in those tests are the operations
themselves, the acceptance criterion is checkable rather than asserted — the test
reads `refs/heads/main` out of the remote afterwards — and the workspace helper is
covered as the Python it is rather than as a Go paraphrase that could agree with
the test while disagreeing with the pod. `python3 -m py_compile` over the
scaffolded files is in there too, because a template that renders is not
necessarily a template that parses.

The consequence is that those tests **skip** where git or python3 is missing,
loudly. A green run without them proves nothing about the operations under test.

M6 has the same shape and is worth one note. Its API tests run the **real** profiler
over two synthetic power series a room apart, rather than faking the
`activity_pattern` the relational pass rests on. A fake pattern would let the two
halves agree with each other while disagreeing with what a developer sees in the
profiler view, and the thing being checked is precisely that they do not — the
thresholds, the contingency counts and the exception window in `relation.json` are
the detectors' own. The rule logic itself is checked separately in `pkg/relations`
against a fixture built so every figure in it can be worked out by hand: an oven that
runs 19:00–22:00 nightly and 10:00–10:30 each morning, lights that follow only the
evening run, and therefore a confidence of exactly 6/7 with a 06:00–12:00 exception.

Detector correctness is checked against fixtures with known answers rather than
against the platform (§5.4.14): a synthesised 15-minute series with an
injected gap, a monotonic counter with two resets, a bimodal washing-machine
load, white noise against a random walk. That is what makes the profiler testable
without an LLM and without the cluster.

