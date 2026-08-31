# The SeriesProfile schema, the detectors and the numerics

The field-by-field shape of a `SeriesProfile`, the order the detectors were built
in, and which numerical methods are available in Go and which are deliberately
`not_computed`.

## Applies when

Adding a detector or a profile field, or checking what a given field is supposed
to contain. This is reference detail extracted from §5.4; the *decisions*
behind it — immutability, the override overlay, provenance, never-null, the cache
key, the projection — stay in [component-design.md](component-design.md) §5.4.1
to §5.4.11, as D19 to D30.

**Not this if**: the question is how a profile is *computed* rather than what it
contains — the two-pass read and its failure modes are in
[profiler-reads.md](profiler-reads.md), and the guarantees callers depend on are
in [profiler-contracts.md](profiler-contracts.md).

The schema and the numerics availability hold generally. The thresholds inside
individual detectors do not: they were chosen against this platform's data, and
they are the first thing to question when a detector fires on the wrong series.

## 5.4.12 `SeriesProfile` schema

```json
{
  "profile_id": "...", "tier": "full",
  "series_ref": {"device_id": "...", "service_id": "...", "variable_path": "..."},
  "detector_version": "1.0.0",
  "analysis_window": {"from": "...", "to": "..."},
  "raw_window": {"from": "...", "to": "...", "source": "default | developer_override"},
  "computed_at": "...",

  "service_context": { "...": "see §5.4.1" },

  "coverage": {"n_points": 0, "expected_points": 0, "completeness_ratio": 0.0},

  "sampling": {
    "detected_interval_s": 900,
    "regularity": "regular | irregular | mixed",
    "confidence": "certain | likely | uncertain",
    "gaps": [{"from": "...", "to": "...", "duration_s": 0,
              "classification": "device_offline | sensor_fault | ingestion_gap | unknown"}]
  },

  "value_semantics": {
    "kind": "instantaneous | cumulative_counter | binary | categorical | status",
    "kind_confidence": "likely",
    "kind_evidence": {"monotonic_ratio": 0.0, "distinct_values": 0},
    "characteristic_id": "... | null",
    "unit": "W",
    "unit_source": "characteristic | unit_reference | inferred | unknown | conflict",
    "declared_range": {"min": 0, "max": 0},
    "range_violation_ratio": 0.0,
    "counter_resets": ["..."],
    "available_conversions": [{"to_characteristic_id": "...", "to_unit": "kW", "distance": 1}]
  },

  "distribution": {
    "min": 0, "max": 0, "mean": 0, "median": 0, "p01": 0, "p99": 0,
    "zero_ratio": 0.0,
    "constant_runs": [{"from": "...", "to": "...", "value": 0, "duration_s": 0}]
  },

  "temporal_structure": {
    "dominant_periods_s": [86400, 604800],
    "trend": {"slope": 0.0, "significant": false},
    "stationarity": {"adf_p": 0.0}
  },

  "activity_pattern": {
    "classification": "continuous | session_based | intermittent | status",
    "classification_confidence": "likely",
    "idle_level": 0.0, "active_threshold": 0.0,
    "session_stats": {"count": 0, "median_duration_s": 0,
                      "inter_arrival_median_s": 0, "median_energy": 0.0},
    "session_exemplars": [{"from": "...", "to": "...", "duration_s": 0,
                           "energy": 0.0, "peak": 0.0}],
    "sessions_ref": "/profiles/{profile_id}/sessions"
  },

  "quality_flags": [{"flag": "frozen_sensor", "confidence": "certain",
                     "evidence": {"longest_constant_run_s": 0}}],

  "recommendations": {
    "advisory": true,
    "resample_to_s": 900,
    "interpolation_strategy": "none | linear | ffill",
    "usable_range": {"from": "...", "to": "..."},
    "exclusions": [{"from": "...", "to": "...", "reason": "..."}]
  },

  "provenance": { "...": "see §5.4.4" }
}
```

Any field above may instead carry the `not_computed` object of §5.4.6.

## 5.4.13 Detector build order

1. **Sampling interval** — modal inter-arrival delta, irregularity ratio, gap list. **Raw pass.**
2. **Value semantics** — highest-impact detector. `cumulative_counter` when monotonic ratio > 0.95; detect resets via large negative deltas. Misreading a cumulative kWh counter as instantaneous power produces silent garbage. **Raw pass.**
3. **Unit resolution** — ontology first, inference as fallback (§5.4.11). **No read.**
4. **Gap classification** — correlate gaps with `GET /devices/{id}/connection-state` history. A gap while the device was offline is *expected*, not a sensor fault. Materially reduces false quality flags.
5. **Interaction check** — a `Service` with `interaction: "request"` is polled, not streamed. Confirm `event` or `event+request` before treating the variable as a time series.
6. **Periodicity** — ACF peaks plus FFT; report daily and weekly explicitly. **Aggregated pass.**
7. **Session detection** — bimodal KDE or Otsu for the idle/active split, hysteresis, minimum duration, sub-threshold gap merging. All parameters developer-adjustable. **Raw or fine bucket** — coarse buckets destroy short sessions.
8. **Cross-variable relationships** — §5.4.1, using the service-scoped batch.
9. **Quality flags** — frozen sensor, negative values on unsigned quantities, DST ambiguity, range violation.

**Time handling is not optional.** Store and compute in UTC, display in local time, flag DST transition windows. Silent DST bugs in 15-minute meter data are a recurring failure mode in this domain.

## 5.4.14 Numerics in Go (D30)

Go has no SciPy. This is the one place where D30 costs effort rather than saving it, so it is specified rather than discovered during M1b.

| Detector | Implementation |
|---|---|
| Modal inter-arrival, gaps, monotonic ratio, counter resets, zero ratio, constant runs | Plain Go. No library needed |
| Percentiles, mean, median, variance | `gonum.org/v1/gonum/stat` (BSD-3-Clause) |
| Periodicity — FFT | `gonum.org/v1/gonum/dsp/fourier` |
| Periodicity — ACF | `stat.AutoCorrelation`, peak-picking in plain Go |
| Idle/active split — Otsu | ~40 lines of plain Go; the histogram formulation is elementary |
| Idle/active split — KDE | Gaussian kernel over a fixed grid, plain Go. Silverman bandwidth |
| **Stationarity — ADF** | **No Go implementation exists.** See below |

**ADF is the only genuine gap.** The augmented Dickey-Fuller test needs an OLS regression on lagged differences plus MacKinnon critical values. `gonum/stat/regression` covers the OLS part; the MacKinnon surface has to be tabulated.

Do not fake it. `temporal_structure.stationarity` carries the `not_computed` object of §5.4.6 with `reason: "out_of_scope"` until it is implemented deliberately, and D24 exists precisely so that an absent field is read as *"could not determine"* rather than *"stationary"*. Implementing ADF is a discrete, testable task with published reference values — schedule it inside M1b, do not let it block the other eight detectors.

**Verification.** Detector correctness is checked against fixtures with known answers, not against the platform: a synthesised 15-minute series with an injected gap, a monotonic counter with two resets, a bimodal washing-machine load. This is what makes the profiler testable without an LLM (§4) and without the cluster.
