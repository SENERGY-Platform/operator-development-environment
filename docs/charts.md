# Chart specifications and where a transform runs

The LLM emits a declarative chart specification and never receives the values.
That constraint is what decides where a transform is evaluated, and it is the
reason a unit conversion is not something a model may compute.

## Applies when

Adding a transform, a chart type or an annotation kind, or working out why a
chart's axis label differs from the variable's declared unit.

**Not this if**: the question is the confirmation a developer gives *from* a
chart — that lands in the profiler's override overlay, see
[profiler-contracts.md](profiler-contracts.md).

## The transforms, and where they run

Every transform of §5.9 is a field of `POST /queries/v2`. Nothing in
`pkg/charts` does arithmetic on a value.

| Transform | Becomes |
| --- | --- |
| `none` | `groupType: mean` at the chart's bucket |
| `resample:900s` | `groupTime: 15m` — and `90s` stays 90 seconds rather than rounding to a minute |
| `diff` | `groupType: difference-last`, differenced server-side |
| `rate` | the same, plus `math: /3600` for an hourly bucket |
| `convert:<characteristic>` | `sourceCharacteristicId` → `targetCharacteristicId` with the `conceptId` the platform needs, refused up front when the target is not reachable through the concept's conversion graph |

A `convert:` on a variable with no characteristic is refused rather than sent: a
fabricated characteristic id would silently authorise a wrong conversion, which is
the failure D29 names in as many words.

The point cap widens the bucket; it never truncates the window. A chart of a year
comes back at whatever bucket fits, and says that it did — a chart showing the first
tenth of its window while claiming the whole of it is the misreading the cap exists
to prevent.