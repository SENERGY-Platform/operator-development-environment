# Why aspect identity compares the set, not the alias

SNRGY-4648: the device repository moved `ContentVariable`, `FilterCriteria`,
`ImportTypeFilterCriteria` and `PathOption` from one aspect id to a list
(`aspect_ids`, `aspect_nodes`), keeping the old singular field as a deprecated
alias that holds only the alphabetically first entry. Everywhere ODE used to
compare that alias for identity, it now compares the whole set
(`ontology.AspectIDs`, `ontology.EqualAspectSets`).

## Applies when

Changing or reviewing `sameQuantity` (`pkg/profiler/relationships.go`),
`findCounterpart`/`findCounterpartInService`
(`pkg/experiments/retarget.go`), or anything else that asks "do these two
variables measure the same thing" from a function id and an aspect.

**Not this if**: the question is which aspect(s) a *selectable* carries for
display — that is `ontology.AspectRef` and the `aspects` field documented in
[component-design.md](component-design.md) §5.2. This note is about equality,
not projection.

## The counter-example

Two variables, each classified under two aspects:

```
power [electricity, kitchen]
power [electricity, living_room]
```

Both carry the same function (`fn-power`) and both declare `electricity` first
alphabetically, so the deprecated `aspect_id` alias reads `"electricity"` for
either one. A comparison written against that alias — `a.AspectID ==
b.AspectID` — sees two identical strings and concludes the two variables
measure the same thing. They do not: one is in the kitchen, the other in the
living room, and `electricity` is the one hierarchy they happen to share, not
the whole of what classifies either series.

Before SNRGY-4648 this case could not arise, because a variable carried at most
one aspect, full stop. The alias is exactly as wide as the single id it used to
be — it just no longer *is* the id, it is a projection of a set, and a
projection is lossy by construction. Comparing the projection compares less
than the model actually states.

## The fix, and the one place it does not apply

`sameQuantity` and `findCounterpart`/`findCounterpartInService` compare
`AspectIDs` as a set (`ontology.EqualAspectSets`), never the alias. Both sides
are sorted and deduplicated at construction (`ontology.AspectIDs`), which is
what makes the comparison a plain positional check rather than a second sort on
every call — a comparison built from ids assembled any other way is not safe to
feed it.

The two functions differ on what an **empty** set means, and both keep their
pre-SNRGY-4648 behaviour deliberately:

- `sameQuantity` treats two empty sets as equal, matching the old `"" == ""`: a
  function match with no declared aspect on either side is still a claim worth
  making (and the characteristic-based branch beneath it does not need aspects
  at all).
- `findCounterpart`/`findCounterpartInService` refuse to match on an empty set:
  the semantic branch only ever runs once the origin variable has `AspectIDs`
  non-empty, which is the same guard the single-alias version had
  (`origin.AspectID == ""` used to refuse the same way).

## Where the alias is still read, and why that is correct there

`ontology.AspectIDs(list, alias)` is the one reading point for the alias, and
it is still consulted — as a *fallback*, never for comparison. A document the
platform has not migrated, or an upstream type that still only sets the
singular field, carries exactly one aspect in truth, and the alias names it
correctly. What changed is that nothing downstream compares two *already
multi-valued* lists by looking only at one element of each.
