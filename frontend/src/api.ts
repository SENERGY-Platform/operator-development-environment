/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { token } from "./keycloak";

const BASE = import.meta.env.VITE_API_BASE ?? "/api";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    /**
     * The error body, when the backend sent one. Kept because some refusals carry
     * more than a sentence: the repo routes answer 409 with a `needs` field naming
     * the step the developer has not taken, and the pane shows a connect card or a
     * repository picker rather than an error on the strength of it.
     */
    readonly body?: Record<string, unknown>,
  ) {
    super(message);
    this.name = "ApiError";
  }

  /** 403 is final: the user lacks the developer role, or may not see this. */
  get isForbidden(): boolean {
    return this.status === 403;
  }

  /** What the backend says is missing, for the refusals that say so. */
  get needs(): string | undefined {
    const needs = this.body?.needs;
    return typeof needs === "string" ? needs : undefined;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const accessToken = await token();
  const headers: Record<string, string> = {};
  if (accessToken) headers.Authorization = `Bearer ${accessToken}`;
  if (init?.body) headers["Content-Type"] = "application/json";

  const response = await fetch(`${BASE}${path}`, { ...init, headers });

  if (!response.ok) {
    let message = response.statusText;
    let body: Record<string, unknown> | undefined;
    try {
      body = (await response.json()) as Record<string, unknown>;
      if (typeof body.error === "string") message = body.error;
    } catch {
      // Response had no JSON body; the status text stands.
    }
    throw new ApiError(response.status, message, body);
  }
  // 204 carries no body, and calling json() on one throws. DELETE answers 204, so
  // this is the normal path rather than an edge case.
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

const get = <T>(path: string) => request<T>(path);
const post = <T>(path: string, body: unknown) =>
  request<T>(path, { method: "POST", body: JSON.stringify(body) });
const put = <T>(path: string, body: unknown) =>
  request<T>(path, { method: "PUT", body: JSON.stringify(body) });
const del = (path: string) => request<void>(path, { method: "DELETE" });

/**
 * Which workbench the repository and kernel calls act in.
 *
 * Module-level rather than a parameter on thirty functions, and it sits here for
 * the reason the queue below does: this is the layer that knows what the SPA is
 * doing as a whole. The panes show one workbench at a time — opening a chat session
 * switches to that session's — so a single value is what "the one on screen" means.
 *
 * Empty means "my only workbench", which is what the backend assumes for a request
 * that names none. So a developer who has one never sees any of this.
 */
let activeWorkbench = "";

export const setActiveWorkbench = (id: string | null) => {
  activeWorkbench = id ?? "";
};

export const getActiveWorkbench = () => activeWorkbench;

/**
 * Appends a workbench to a path that acts in one.
 *
 * The workbench is a parameter rather than read from `activeWorkbench` here,
 * because a queued call builds its URL when it *runs* — and by then the developer
 * may have switched panes. Taking it from the queue's own captured value is what
 * stops a file read that was issued against one operator being sent to another.
 * Callers that are not queued build their URL immediately and take the active one.
 */
const inWorkbench = (path: string, workbench = activeWorkbench): string => {
  if (!workbench) return path;
  return `${path}${path.includes("?") ? "&" : "?"}workbench=${encodeURIComponent(workbench)}`;
};

/**
 * The queues the repository operations run through — one per workbench.
 *
 * Every one of them executes a cell in the developer's pod, and a kernel runs one
 * cell at a time. The backend waits a bounded while for an idle one rather than
 * refusing the second caller, so a collision is no longer an error — but it is
 * still a queue, and this is the only place that knows which requests the SPA
 * issued itself. A page load asks for the repository status, the file tree and the
 * open file at once; sending them one after another means they wait in the browser
 * rather than each holding a request open on the way to the same kernel.
 *
 * Per workbench, because each has its own kernel: one queue for all of them would
 * put a file read in one operator behind a clone in another, which is precisely
 * what having several is meant to avoid.
 *
 * Only the operations that run in the pod go through here. The GitHub connection
 * and the repository list are ODE's own reads, and putting them behind a clone
 * would make the picker feel like the clone.
 */
const workspaceQueues = new Map<string, Promise<unknown>>();
const workspace = <T>(call: (workbench: string) => Promise<T>): Promise<T> => {
  // Captured now and handed to the call when it runs: the queue a call joins and
  // the workbench it names are both the ones it was issued against, whatever the
  // panes switch to while it waits.
  const key = activeWorkbench;
  const run = () => call(key);
  const tail = workspaceQueues.get(key) ?? Promise.resolve();
  // Both handlers are the same call: the queue is a chain of attempts, and one
  // that failed must not keep the ones behind it from being made. The tail is
  // caught separately so a rejection a caller already handles is not also an
  // unhandled one here.
  const next = tail.then(run, run);
  workspaceQueues.set(key, next.catch(() => undefined));
  return next;
};

// --- M0 ---

export interface Session {
  user_id: string;
  username: string;
  email: string;
  roles: string[];
  is_admin: boolean;
  /** The tier a *new* chat session starts at. A live tier is session-scoped. */
  exposure_tier: Tier;
  features: {
    profiler: boolean;
    selection: boolean;
    chat: boolean;
    mcp: boolean;
    kernel: boolean;
    charts: boolean;
    relations: boolean;
    repo: boolean;
    experiments: boolean;
  };
  /** Present only when the repo surface is configured (M7): what the GitHub
   * consent screen will ask for, so the SPA can say it before GitHub does. */
  repo?: { scopes: string[] };
  /** Present only when the LLM surface is configured (M3). */
  max_exposure_tier?: Tier;
  limits?: Limits;
  spend?: Spend;
  providers?: ProviderInfo[];
  /** Present only when an execution backend is configured (M4). */
  kernel?: { workspace: string; kernel: string };
  /** Present only when Ray and MLflow are configured (M8). The two URLs are what a
   * browser should open — routinely a different host from the API base ODE calls —
   * and `scoped_job_token` says whether a job gets a credential of its own
   * (§3.1 item 6) or the developer's session token. The pane needs that before a
   * developer starts a long run, not after one dies at hour two. */
  experiments?: { ray_url: string; mlflow_url: string; scoped_job_token: boolean };
}

export interface AspectTreeNode {
  id: string;
  name: string;
  root_id: string;
  parent_id: string;
  children: AspectTreeNode[] | null;
}

export interface OntologyFunction {
  id: string;
  name: string;
  display_name: string;
  concept_id: string;
  rdf_type: string;
}

export interface Device {
  id: string;
  name: string;
  /** Computed per request, and what the platform's own UIs show. Prefer it. */
  display_name?: string;
  device_type_id: string;
  /** Computed per request, so not always populated. */
  device_type_name?: string;
  connection_state: string;
  shared?: boolean;
  permissions?: Record<string, boolean>;
}

/** deviceLabel is the one place that decides how a device is named on screen. */
export function deviceLabel(device: Device): string {
  return device.display_name || device.name || device.id;
}

export interface DeviceList {
  devices: Device[];
  total: number;
  limit: number;
  offset: number;
}

// --- M1: the not_computed contract (SPEC D24) ---

/**
 * Every computable profile field is either a value or an explicit non-result.
 * Never null, never absent — so a reader can tell "could not determine" from
 * "no". The union is what makes that unavoidable on this side too: you cannot
 * read `.mean` off a `Value<Distribution>` without first deciding what to do
 * about the case where it was not computed.
 */
export type Value<T> = T | NotComputed;

export interface NotComputed {
  status: "not_computed";
  reason:
    | "insufficient_coverage"
    | "insufficient_span"
    | "wrong_kind"
    | "read_failed"
    | "out_of_scope";
  detail: string;
}

export function isNotComputed<T>(value: Value<T>): value is NotComputed {
  return (
    typeof value === "object" &&
    value !== null &&
    (value as NotComputed).status === "not_computed"
  );
}

/** Ordinal only, and always accompanied by evidence (SPEC D23). */
export type Confidence = "certain" | "likely" | "uncertain";

export interface SeriesRef {
  /**
   * A series is addressed by a device's service or by an export, and the backend
   * omits whichever pair does not apply. Both are declared required here because
   * every profile this SPA asks for is a device's: no pane requests an export
   * profile, so no response it can elicit carries `export_id` instead. Widening
   * these to optional is the first step of adding one.
   */
  device_id: string;
  service_id: string;
  /** Set instead of the two above on a profile of an export, where the variable
   * path is the export's column name. Only the LLM tool surface and `POST
   * /profiles` with an `export_id` produce one today. */
  export_id?: string;
  variable_path: string;
}

export interface Window {
  from: string;
  to: string;
}

export interface RawWindow extends Window {
  source: "default" | "developer_override";
  truncated: boolean;
  /**
   * The row cap the read was actually made with — the configured point bound
   * divided by the variables read, because the response carries one value per
   * variable per row. Absent only on a profile captured before the field existed.
   */
  row_limit?: number;
  /** The gateway refused the first attempt and it was retried with half the rows. */
  limit_reduced?: boolean;
}

export interface ProvenanceEntry {
  read_mode: "raw" | "aggregated" | "none";
  source: "ontology" | "detector" | "inference" | "developer" | "api";
  detector?: string;
  ref?: string;
  window?: Window;
  group_time?: string;
  note?: string;
}

/**
 * Keys differ per variable: one with no characteristic has no
 * `value_semantics.characteristic_id` entry at all. The value type admits
 * undefined because a lookup really can miss, and `Record<string, T>` would
 * promise otherwise and let a caller dereference nothing.
 */
export type Provenance = Record<string, ProvenanceEntry | undefined>;

// --- M1a: QuickProfile ---

export interface Aggregate {
  group_time: string;
  group_type: string;
}

export interface AvailabilityWindow {
  from: string;
  to: string;
  span_days: number;
  aggregates: Aggregate[];
  raw_available: boolean;
}

export interface Volume {
  bytes: number;
  bytes_per_day: number;
  estimated_interval_s: Value<number>;
  estimate_basis: string;
  confidence: Confidence;
}

export interface Declared {
  characteristic_id: string | null;
  unit: string;
  unit_source: string;
  min_value: Value<number>;
  max_value: Value<number>;
  type: string;
  function_id?: string;
  aspect_id?: string;
}

export interface Liveness {
  connection_state: string;
  last_value_age_s: Value<number>;
  basis: string;
}

export interface Completeness {
  status: "complete" | "partial";
  missing: string[];
  consequence?: string;
}

export interface RankHints {
  span_days: number;
  coverage_proxy: number;
  is_live: boolean;
  score: number;
}

/**
 * The candidate's device as a human reads it, beside the SeriesRef machines key on.
 * The ids are not repeated here — `series_ref.device_id` is the one.
 */
export interface DeviceInfo {
  name: string;
  device_type_id: string;
  device_type_name: string;
}

export interface QuickProfile {
  series_ref: SeriesRef;
  device: DeviceInfo;
  tier: string;
  availability: Value<AvailabilityWindow>;
  volume: Value<Volume>;
  declared: Declared;
  interaction: string;
  liveness: Liveness;
  ontology_completeness: Completeness;
  rank_hints: RankHints;
  queryable: boolean;
  reason?: string;
  provenance: Provenance;
}

export interface ReadCounts {
  availability: number;
  usage: number;
  /** The number that matters at tier L0: it must be zero. */
  values: number;
}

export interface SkippedDevice {
  device_id: string;
  name: string;
  reason: string;
}

export interface QuickProfileList {
  candidates: QuickProfile[];
  skipped: SkippedDevice[];
  reads: ReadCounts;
  coverage_window: Window;
  devices_listed: number;
  total_devices: number;
  /** How many devices the listing was allowed to expand, after clamping. */
  device_limit: number;
}

export interface AvailabilityList {
  device_id: string;
  availability: {
    serviceId: string;
    from?: string;
    to?: string;
    groupTime?: string;
    groupType?: string;
  }[];
}

export interface UsageList {
  usage: { deviceId: string; bytes: number; bytesPerDay: number; updatedAt: string }[];
}

// --- M1b: SeriesProfile ---

/**
 * What the two passes returned, as opposed to what the detectors made of it. The
 * one block that is always populated, so an empty profile can explain itself.
 */
export interface ReadSummary {
  /**
   * Whether the platform holds unbucketed data for this service — and a
   * non-result when the availability probe itself failed. A boolean could not tell
   * the two apart, and read as a false, an unanswered probe sends a reader looking
   * for retention that is not the cause (D24).
   */
  raw_available: Value<boolean>;
  raw_rows: number;
  values_present: number;
  null_rows: number;
  aggregated_buckets: number;
  diagnosis?: string;
}

export interface Coverage {
  n_points: number;
  expected_points: number;
  completeness_ratio: number;
}

export interface Gap {
  from: string;
  to: string;
  duration_s: number;
  classification: "device_offline" | "sensor_fault" | "ingestion_gap" | "unknown";
}

export interface Sampling {
  detected_interval_s: number;
  regularity: "regular" | "irregular" | "mixed";
  confidence: Confidence;
  irregularity_ratio: number;
  gaps: Gap[];
}

export type ValueKind =
  | "instantaneous"
  | "cumulative_counter"
  | "binary"
  | "categorical"
  | "status";

export interface KindEvidence {
  monotonic_ratio: number;
  distinct_values: number;
  negative_deltas: number;
  non_numeric_ratio: number;
}

export interface Conversion {
  to_characteristic_id: string;
  to_unit: string;
  distance: number;
}

export interface ValueSemantics {
  kind: Value<ValueKind>;
  kind_confidence: Value<Confidence>;
  kind_evidence: Value<KindEvidence>;
  /** Canonical and authoritative; null is legitimate and never fabricated (D29). */
  characteristic_id: string | null;
  unit: string;
  unit_source: string;
  declared_range: { min: Value<number>; max: Value<number> };
  range_violation_ratio: Value<number>;
  counter_resets: Value<string[]>;
  available_conversions: Conversion[];
}

export interface ConstantRun {
  from: string;
  to: string;
  value: number;
  duration_s: number;
  points: number;
}

export interface Distribution {
  min: number;
  max: number;
  mean: number;
  median: number;
  p01: number;
  p99: number;
  std_dev: number;
  zero_ratio: number;
  constant_runs: ConstantRun[];
}

export interface PeriodEvidence {
  period_s: number;
  method: string;
  strength: number;
  label: string;
  /**
   * How far either side of `period_s` the method cannot tell one period from
   * another: half a bucket for a lag, half the spread between neighbouring bins
   * for a frequency.
   *
   * Worth rendering rather than hiding. A period alone invites a precision that is
   * not there — an FFT bin at 720 buckets covers 160h to 206h and was printed as
   * "648000.0" seconds, which reads as a distinct cycle beside the 604800 the ACF
   * found rather than as the same one seen through a wider lens.
   *
   * Absent on evidence computed before detector version 1.1.0, and omitted where
   * the method reports no resolution.
   */
  resolution_s?: number;
}

export interface Trend {
  slope: number;
  slope_per_day: number;
  r2: number;
  significant: boolean;
  t_stat: number;
}

export interface Stationarity {
  adf_stat: number;
  lag_order: number;
  n_obs: number;
  critical_values: Record<string, number>;
  /** A bracket rather than a point p-value; see SPEC §5.4.14 and the README. */
  p_value_bracket: { lower: number; upper: number; note: string };
  stationary: boolean;
  confidence: Confidence;
  regression: string;
}

export interface TemporalStructure {
  dominant_periods_s: Value<number[]>;
  period_evidence: Value<PeriodEvidence[]>;
  trend: Value<Trend>;
  stationarity: Value<Stationarity>;
}

export interface SessionStats {
  count: number;
  median_duration_s: number;
  inter_arrival_median_s: number;
  median_energy: number;
}

export interface SessionExemplar {
  from: string;
  to: string;
  duration_s: number;
  energy: number;
  peak: number;
}

export interface ActivityPattern {
  classification: "continuous" | "session_based" | "intermittent" | "status";
  classification_confidence: Confidence;
  idle_level: number;
  active_threshold: number;
  threshold_method: string;
  threshold_params: {
    min_duration_s: number;
    merge_gap_s: number;
    hysteresis_frac: number;
  };
  session_stats: Value<SessionStats>;
  session_exemplars: SessionExemplar[];
  sessions_ref: string;
}

export interface QualityFlag {
  flag: string;
  confidence: Confidence;
  evidence: Record<string, unknown>;
}

export interface Exclusion {
  from: string;
  to: string;
  reason: string;
}

export interface Recommendations {
  /** Always true. Binding only on explicit developer promotion (D28). */
  advisory: boolean;
  resample_to_s: Value<number>;
  interpolation_strategy: Value<string>;
  usable_range: Value<Window>;
  exclusions: Exclusion[];
}

export interface SiblingVariable {
  path: string;
  characteristic_id: string;
  kind: string;
}

export interface Relationship {
  type: "integral_of" | "derivative_of" | "redundant_with" | "inconsistent_with";
  other_path: string;
  evidence: {
    correlation: number;
    residual_ratio: number;
    overlap_points: number;
    implied_scale: number;
  };
  confidence: Confidence;
}

export interface ServiceContext {
  service_id: string;
  interaction: string;
  sibling_variables: SiblingVariable[];
  relationships: Relationship[];
}

export interface Resolution {
  field_path: string;
  computed_value: unknown;
  confirmed_value: unknown;
  action: "confirm" | "correct" | "reject";
  override_id: string;
  created_by: string;
  created_at: string;
  note?: string;
  supersedes?: string[];
}

export interface SeriesProfile {
  profile_id: string;
  tier: string;
  series_ref: SeriesRef;
  detector_version: string;
  analysis_window: Window;
  raw_window: RawWindow;
  computed_at: string;
  cache_key: string;
  service_context: ServiceContext;
  read_summary: ReadSummary;
  coverage: Value<Coverage>;
  sampling: Value<Sampling>;
  value_semantics: ValueSemantics;
  distribution: Value<Distribution>;
  temporal_structure: TemporalStructure;
  activity_pattern: Value<ActivityPattern>;
  quality_flags: QualityFlag[];
  recommendations: Recommendations;
  provenance: Provenance;
  /** Added by Resolve: the overlay, merged at read time only (D21). */
  resolution: Record<string, Resolution>;
}

export interface ProfileResult {
  profiles: SeriesProfile[];
  reads: ReadCounts;
  from_cache: string[];
  analysis_window: Window;
  raw_window: RawWindow;
  group_time: string;
}

// --- M1b: the LLM projection (D26) ---

export interface GapSummary {
  count: number;
  total_duration_s: number;
  by_classification: Record<string, number>;
  largest: Gap[];
}

export interface SamplingView {
  detected_interval_s: number;
  regularity: string;
  confidence: Confidence;
  irregularity_ratio: number;
  gaps: GapSummary;
}

export interface DistributionView {
  min: number;
  max: number;
  mean: number;
  median: number;
  p01: number;
  p99: number;
  std_dev: number;
  zero_ratio: number;
  constant_runs: { count: number; longest?: ConstantRun };
}

export interface ValueSemanticsView {
  kind: Value<ValueKind>;
  kind_confidence: Value<Confidence>;
  kind_evidence: Value<KindEvidence>;
  characteristic_id: string | null;
  unit: string;
  unit_source: string;
  declared_range: { min: Value<number>; max: Value<number> };
  range_violation_ratio: Value<number>;
  counter_resets: { status?: NotComputed; count: number; first?: string[] };
  available_conversions: Conversion[];
  confirmed?: string[];
}

export interface ActivityView {
  classification: string;
  classification_confidence: Confidence;
  idle_level: number;
  active_threshold: number;
  threshold_method: string;
  session_stats: Value<SessionStats>;
  session_exemplars: SessionExemplar[];
  sessions_ref: string;
}

export interface Elision {
  field: string;
  total: number;
  shown: number;
  fetch?: string;
}

export interface LLMProfileView {
  profile_id: string;
  tier: string;
  series_ref: SeriesRef;
  detector_version: string;
  analysis_window: Window;
  raw_window: RawWindow;
  service_context: ServiceContext;
  coverage: Value<Coverage>;
  sampling: Value<SamplingView>;
  value_semantics: ValueSemanticsView;
  distribution: Value<DistributionView>;
  temporal_structure: TemporalStructure;
  activity_pattern: Value<ActivityView>;
  quality_flags: QualityFlag[];
  recommendations: Recommendations;
  overrides: Resolution[];
  elided: Elision[];
}

export interface SessionRecord {
  from: string;
  to: string;
  duration_s: number;
  energy: number;
  peak: number;
  points: number;
}

export interface SessionPage {
  sessions: SessionRecord[];
  total: number;
  next_cursor?: string;
}

export interface OverrideRequest {
  field_path: string;
  action: "confirm" | "correct" | "reject";
  computed_value?: unknown;
  confirmed_value?: unknown;
  note?: string;
}

/** What the store appended, echoed back with the id and author it assigned. */
export interface ProfileOverrideRecord {
  override_id: string;
  created_at: string;
  created_by: string;
  series_ref: SeriesRef;
  profile_id: string;
  detector_version?: string;
  field_path: string;
  computed_value: unknown;
  confirmed_value: unknown;
  action: "confirm" | "correct" | "reject";
  note?: string;
}

// --- M2: semantic selection (SPEC §5.2) ---

/**
 * Matched is the evidence behind one text-to-ontology resolution, and the reason
 * the UI can show *why* a function was offered. The matcher is lexical, so an
 * unaudited match would be a guess the developer has no way to check.
 */
export interface Matched {
  score: number;
  matched_terms: string[];
  basis: "display_name" | "name" | "explicit_id";
}

export interface FunctionMatch {
  id: string;
  name: string;
  rdf_type: string;
  concept_id: string;
  matched: Matched;
}

/** descendants_included is always true: an aspect criterion covers its subtree. */
export interface AspectMatch {
  id: string;
  name: string;
  descendants_included: boolean;
  matched: Matched;
}

export interface DeviceClassMatch {
  id: string;
  name: string;
  matched: Matched;
}

/**
 * One FilterCriteria as sent, with what it found. There is one request per
 * criterion because the platform ANDs a criteria list, so this is also the
 * request count — and `device_types: 0` is how "the ontology knows this, the
 * platform has none" becomes visible.
 */
export interface Criterion {
  function_id?: string;
  aspect_id?: string;
  device_class_id?: string;
  interaction?: string;
  device_types: number;
}

export interface Selectable {
  device_type_id: string;
  /** Empty when this account can reach no device of the type; show the id then. */
  device_type_name: string;
  service_id: string;
  service_name?: string;
  path: string;
  characteristic_id: string | null;
  unit: string;
  unit_source: string;
  interaction: string;
  type?: string;
  function_id?: string;
  aspect_id?: string;
  aspect_name?: string;
  queryable: boolean;
  reason?: string;
  ontology_completeness: Completeness;
}

export interface CandidateDevice {
  device_id: string;
  /** Already the platform's display name where it has one. */
  name: string;
  connection_state: string;
  device_type_id: string;
  device_type_name: string;
  permissions: Record<string, boolean>;
  series: number;
}

/** SPEC D16: completeness discovered at runtime, per device type, and reported. */
export interface OntologyGap {
  device_type_id: string;
  device_type_name: string;
  missing: string[];
  consequence: string;
  paths: string[];
}

export interface SelectionReads {
  selectables: number;
  device_lists: number;
  availability: number;
  usage: number;
  /** Must be zero: the whole operation is tier L0. */
  values: number;
}

export interface SelectionResult {
  intent: string;
  terms: string[];
  unmatched_terms: string[];
  matched_functions: FunctionMatch[];
  matched_aspects: AspectMatch[];
  matched_device_classes: DeviceClassMatch[];
  criteria: Criterion[];
  selectables: Selectable[];
  candidate_devices: CandidateDevice[];
  ontology_gaps: OntologyGap[];
  candidates: QuickProfile[];
  skipped: SkippedDevice[];
  reads: SelectionReads;
  coverage_window: Window;
  device_limit: number;
  total_devices: number;
  /** What an empty list would otherwise leave the reader to infer. */
  notes: string[];
}

export interface SelectionRequest {
  intent?: string;
  function_ids?: string[];
  aspect_ids?: string[];
  device_class_ids?: string[];
  interaction?: "event" | "request" | "event+request" | "any";
  include_controlling?: boolean;
  match_limit?: number;
  min_score?: number;
  limit?: number;
  window?: Window;
  /** Absent means true; false asks for the ontology resolution alone. */
  rank?: boolean;
}

export interface QuickProfileQuery {
  search?: string;
  from?: string;
  to?: string;
  includeUnqueryable?: boolean;
}

export interface ProfileRequest {
  device_id: string;
  service_id: string;
  analysis_window?: Window;
  raw_window?: Window;
  group_time?: string;
}

function query(params: Record<string, string | number | boolean | undefined>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === "" || value === false) continue;
    search.set(key, String(value));
  }
  const encoded = search.toString();
  return encoded ? `?${encoded}` : "";
}

// --- M3: the LLM surface (SPEC §3.2, §3.3, §5.7, §5.8) ---

/**
 * Tier is the data exposure tier. A string union rather than a number, matching
 * the wire form: the backend marshals "L0" precisely so a reader never has to
 * know whether the levels are 0- or 1-based.
 */
export type Tier = "L0" | "L1" | "L2";

export const TIERS: Tier[] = ["L0", "L1", "L2"];

/** What each tier exposes, for the persistent indicator §3.2 asks for. */
export const TIER_EXPOSES: Record<Tier, string> = {
  L0: "Ontology, device names and types, availability, volume and rate estimates, connection state, QuickProfile. No values.",
  L1: "L0 plus SeriesProfile and RelationProfile: statistics, detected periods, session stats, quality flags.",
  L2: "L1 plus downsampled series previews, which are actual values.",
};

export interface Capabilities {
  tools: boolean;
  streaming: boolean;
  system: boolean;
  max_tokens?: number;
  models?: string[];
  tools_out_of_band?: boolean;
  degraded?: boolean;
  degraded_reason?: string;
}

export interface ProviderInfo {
  name: string;
  capabilities: Capabilities;
  default: boolean;
}

export interface ToolInfo {
  name: string;
  description: string;
  effect: string;
  min_tier: Tier;
  confirm: boolean;
  implemented: boolean;
  unavailable: string;
}

export interface TierInfo {
  tier: Tier;
  exposes: string;
  available: string[];
}

export interface ToolSurface {
  tools: ToolInfo[];
  tiers: TierInfo[];
  /** The capabilities §5.8 denies: name → why it is a developer action. */
  denied: Record<string, string>;
}

export interface Limits {
  period?: string;
  token_cap?: number;
  cost_cap?: number;
  soft_warn_fraction?: number;
  global_cost_cap?: number;
  allowed_providers?: string[];
  allowed_models?: string[];
  max_tier?: Tier;
  max_concurrent_sessions?: number;
  kernel_cpu_default?: string;
  kernel_cpu_max?: string;
  kernel_mem_default?: string;
  kernel_mem_max?: string;
  max_concurrent_ray_jobs?: number;
}

export interface Spend {
  tokens: number;
  cost: number;
  requests: number;
  from: string;
  to: string;
}

export interface LimitsRecord {
  subject: string;
  limits: Limits;
  updated_at: string;
  updated_by: string;
}

export interface ModelPrice {
  model: string;
  input_per_mtok: number;
  output_per_mtok: number;
  cached_input_per_mtok?: number;
}

export interface LimitsSurface {
  limits: LimitsRecord[];
  defaults: Limits;
  /** Which fields this build acts on, and which are stored for a later milestone. */
  enforced: string[];
  declared: Record<string, string>;
  pricing: ModelPrice[];
  currency: string;
}

export interface UsageRecord {
  user_sub: string;
  session_id?: string;
  provider: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cached_input_tokens?: number;
  cost: number;
  cost_estimated: boolean;
  at: string;
}

export interface ToolCallRecord {
  user_sub: string;
  session_id: string;
  tool: string;
  tier: Tier;
  outcome: string;
  duration_ms: number;
  at: string;
}

export interface ProposedSeries {
  device_id: string;
  service_id: string;
  variable_path: string;
  role?: string;
  reason?: string;
}

export interface ProposedSelection {
  rationale: string;
  series: ProposedSeries[];
  proposed_at: string;
}

export interface ChatSession {
  id: string;
  user_sub: string;
  title: string;
  provider: string;
  model: string;
  exposure_tier: Tier;
  /**
   * Whether this conversation runs a recognised `run_code` without asking.
   *
   * Off unless the developer turned it on. It changes who is asked, not what the
   * assistant may do: the tier still bounds what it can see, and anything the
   * backend does not recognise is still confirmed.
   */
  auto_run: boolean;
  /**
   * The working context this conversation acts in: whose checkout `write_file`
   * writes into and whose kernel `run_code` runs in. Absent on a session created
   * before workbenches existed, which the backend reads as "my only one".
   */
  workbench_id?: string;
  selection?: ProposedSelection;
  created_at: string;
  updated_at: string;
  message_count: number;
}

export type ChatRole = "user" | "assistant";

export interface ChatContent {
  type: "text" | "tool_use" | "tool_result";
  text?: string;
  tool_use_id?: string;
  tool_name?: string;
  tool_input?: unknown;
  tool_result?: string;
  is_error?: boolean;
}

export interface ChatMessage {
  session_id: string;
  seq: number;
  role: ChatRole;
  content: ChatContent[];
  created_at: string;
  /** Who put the message in the conversation (M9, §5.13).
   *
   * Absent or `""` is the developer, which is what every message written before M9
   * was. `"ode"` is a message **ODE composed and injected** — today that means a
   * finished run's structured summary, which the assistant then answers as though
   * it had been asked.
   *
   * `replay()` has to render the two differently. An injected message carries a
   * block of JSON and is stored with the user role because that is what a model
   * reads as input; showing it in the developer's own voice would be a lie about
   * who said it. */
  origin?: "" | "ode";
  /** What an injected message is about — the experiment id, for a run summary — so
   * a pane can render it as a result card rather than as prose, and so a reader can
   * find the message belonging to one run without parsing it. */
  subject?: string;
}

export interface PendingConfirmation {
  id: string;
  tool: string;
  input: unknown;
  tier: Tier;
  created_at: string;
  /**
   * Set while a provider's own tool loop is holding this call open, waiting for the
   * decision. It changes what answering means: a held call is answered in place on
   * the stream already being watched, while an ordinary confirmation resumes a turn
   * that stopped. Never persisted — nothing waits across a restart.
   */
  out_of_band?: boolean;
}

export interface ChatSessionDetail {
  session: ChatSession;
  messages: ChatMessage[];
  pending_confirmations: PendingConfirmation[];
}

export interface TierChange {
  session_id: string;
  user_sub: string;
  from: Tier;
  to: Tier;
  at: string;
}

export interface ToolResult {
  call_id: string;
  tool: string;
  outcome: string;
  content: unknown;
  is_error: boolean;
  duration_ms: number;
}

export interface Usage {
  input_tokens: number;
  output_tokens: number;
  cached_input_tokens?: number;
  provider?: string;
  model?: string;
  cost_eur?: number;
  cost_estimated?: boolean;
}

export interface LimitWarning {
  scope: string;
  kind: string;
  cap: number;
  spent: number;
  fraction: number;
}

/** The refusal shape §3.2 fixes, as the LLM relays it. */
export interface TierRefusal {
  blocked_by_tier: Tier;
  required: Tier;
  tool?: string;
  hint?: string;
}

/** One step of a running tool, for showing that a slow one is alive. */
export interface ToolProgress {
  tool: string;
  stage: string;
  detail: string;
}

/** One event of the chat stream. */
export interface ChatEvent {
  type:
    | "text_delta"
    | "tool_call"
    | "tool_result"
    | "done"
    | "error"
    | "confirmation_required"
    | "limit_exceeded"
    | "warning"
    | "usage"
    | "progress";
  text?: string;
  tool_call?: { id: string; name: string; input: unknown };
  tool_result?: ToolResult;
  confirmation?: PendingConfirmation;
  usage?: Usage;
  warnings?: LimitWarning[];
  progress?: ToolProgress;
  limit?: Record<string, unknown>;
  stop_reason?: string;
  error?: string;
}

/**
 * What one conversation is doing, as reported by `chat_watch`.
 *
 * Separate from ChatEvent, and deliberately thin: this is what a list row needs,
 * not what a conversation needs. The stream of a turn belongs to whoever has that
 * conversation open; this says only which session changed and to what, for every
 * session at once, so the panel can mark a conversation the developer is not
 * currently looking at.
 */
export interface SessionActivity {
  session_id: string;
  /**
   * `running` is a turn in flight, `waiting` one stopped on a confirmation the
   * developer owes an answer to, `idle` no turn at all.
   */
  state: "running" | "waiting" | "idle";
  at: string;
}

// --- M5: the exploration pane (SPEC §5.9, §5.10) ---

/**
 * Who put an element on a chart. Stamped by the backend, never by a specification,
 * so a band the profiler detected and a band a model wrote are told apart on
 * screen — they are different kinds of claim.
 */
export type ChartAuthor = "developer" | "llm" | "profiler";

/**
 * What a chart axis is drawn against.
 *
 * More than a string on purpose (D29): the characteristic id is canonical and
 * convertible, the unit string is derived and advisory. `confirmable` is what the
 * pane offers §5.10's control on, and it is false where the ontology answered
 * outright — the ontology reducing how often a human is asked is part of the
 * design argument, not a gap in it.
 */
export interface ChartUnit {
  unit: string;
  unit_source: string;
  characteristic_id: string | null;
  available_conversions: Conversion[];
  confirmable: boolean;
  confirmed: boolean;
  /** What the resolver said before the developer spoke, so the two stay diffable. */
  computed_unit?: string;
  confirmed_by?: string;
  note?: string;
}

export interface ChartAxis extends ChartUnit {
  /** The series do not share a unit, so the label would be a lie. */
  mixed: boolean;
  from: string;
}

export interface ChartAnnotation {
  annotation_id: string;
  type: string;
  from: string;
  to: string;
  label: string;
  severity: "info" | "warn" | "error";
  source?: string;
  confirmable: boolean;
  /** The profile field confirming this writes to. Present when confirmable. */
  field_path?: string;
  series_index?: number;
  author: ChartAuthor;
}

export interface ChartMarker {
  marker_id: string;
  at: string;
  label: string;
  source?: string;
  series_index?: number;
  author: ChartAuthor;
}

export interface ChartSeriesSpec {
  ref: SeriesRef;
  /** none | diff | rate | resample:<interval> | convert:<characteristic id> */
  transform?: string;
  label?: string;
  /** The profile whose detections are drawn as annotations. */
  profile_id?: string;
}

export interface ChartSpec {
  chart_id: string;
  title: string;
  caption?: string;
  series: ChartSeriesSpec[];
  annotations: ChartAnnotation[];
  markers: ChartMarker[];
  y_axis: { unit?: string; unit_source?: string };
  window: Window;
  group_time?: string;
  author: ChartAuthor;
  created_by: string;
  created_at: string;
  session_id?: string;
}

export interface ChartSeriesResolution {
  index: number;
  ref: SeriesRef;
  label: string;
  transform: string;
  profile_id?: string;
  unit: ChartUnit;
  notes: string[];
}

export interface ChartCreated {
  spec: ChartSpec;
  series: ChartSeriesResolution[];
  y_axis: ChartAxis;
  notes: string[];
}

export interface ChartPoint {
  t: string;
  v: number;
}

export interface ChartSeriesData extends ChartSeriesResolution {
  group_time: string;
  group_type: string;
  math?: string;
  points: ChartPoint[];
  non_numeric_dropped: number;
  null_rows: number;
  /** The override overlay for this series: what the developer has already decided. */
  confirmations: ProfileOverrideRecord[];
}

export interface ChartData {
  chart_id: string;
  title: string;
  caption?: string;
  window: Window;
  group_time: string;
  /** Every series shares one bucket, so points at the same x are the same interval. */
  aligned: boolean;
  series: ChartSeriesData[];
  annotations: ChartAnnotation[];
  markers: ChartMarker[];
  y_axis: ChartAxis;
  annotations_dropped: number;
  reads: { devices: number; queries: number; points: number };
  notes: string[];
}

export interface ChartRequest {
  session_id?: string;
  title?: string;
  caption?: string;
  series: ChartSeriesSpec[];
  annotations?: Partial<ChartAnnotation>[];
  markers?: Partial<ChartMarker>[];
  y_axis?: { unit?: string; unit_source?: string };
  window?: { from?: string; to?: string };
  group_time?: string;
}

export interface ChartConfirmRequest {
  series_index: number;
  field_path: string;
  action: "confirm" | "correct" | "reject";
  computed_value?: unknown;
  confirmed_value?: unknown;
  note?: string;
}

export interface ChartConfirmation {
  override: ProfileOverrideRecord;
  /** The series as it resolves *after* the confirmation, so the axis can relabel. */
  series: ChartSeriesResolution;
  confirmable: Record<string, string>;
}


// --- M6: multi-device conditional patterns (SPEC §5.5) ---

export type MemberState = "active" | "idle" | "unknown";

/**
 * How a set was arrived at, strongest first. A graph carries direction and share as
 * well as membership, which is more than a device group asserts and far more than a
 * shared aspect label.
 */
export type SetOrigin =
  | "graph_siblings"
  | "graph_flow"
  | "device_group"
  | "aspect_node"
  | "aspect_subtree";

/** Where a member sits in the graph that proposed it (SPEC §5.5). */
export interface GraphPlacement {
  graph_id: string;
  graph_name?: string;
  /** "sibling", "upstream" or "downstream". */
  role: string;
  via?: string;
  via_name?: string;
  /**
   * The share of this device's own output attributed along the edge, not a
   * contribution share: the platform requires each node's outgoing weights to sum to
   * 100, so a device feeding one node always reads 100 and says nothing. Below that
   * means its flow is split across several downstream nodes.
   */
  weight?: number;
  /** Edges between this member and the graph's sink. Equal depth means same level. */
  depth: number;
}

export interface CandidateSetMember {
  ref: SeriesRef;
  label: string;
  device_name: string;
  device_type_name?: string;
  service_name?: string;
  function_id?: string;
  aspect_id?: string;
  aspect_name?: string;
  connection_state?: string;
  characteristic_id: string | null;
  unit: string;
  unit_source: string;
  /** Absent for a member the aspect hierarchy proposed rather than a graph. */
  graph?: GraphPlacement;
  /**
   * False for a member reached through a graph rather than through the requested
   * aspect — a site meter is not in the kitchen, and that is the pair a sub-metering
   * question is about.
   */
  from_aspect: boolean;
}

export interface CandidateSet {
  set_id: string;
  origin: SetOrigin;
  name: string;
  rationale: string;
  aspect_id?: string;
  aspect_name?: string;
  aspect_path?: string[];
  device_group_id?: string;
  graph_id?: string;
  graph_name?: string;
  graph_node?: string;
  members: CandidateSetMember[];
  devices: number;
  truncated: boolean;
  notes: string[];
}

export interface AspectRef {
  id: string;
  name: string;
  parent_id?: string;
  depth: number;
}

export interface RelationProposal {
  aspect_id: string;
  aspect_name: string;
  include_descendants: boolean;
  subtree: AspectRef[];
  sets: CandidateSet[];
  candidate_devices: CandidateDevice[];
  ontology_gaps: OntologyGap[];
  reads: RelationReads;
  notes: string[];
}

export interface RelationReads {
  aligned: number;
  profiles: number;
  /**
   * Metadata reads: one per service a pass profiles, one per graph neighbour a
   * proposal resolves from outside the aspect. Apart from `values` because a device
   * read does not touch the exposure tier — but counted, so a proposal that made a
   * dozen of them does not look free.
   */
  devices: number;
  values: number;
}

/**
 * How idle and active were decided for one member. `usable: false` carries the
 * reason instead, and a member that reads unusable took part in no rule — which is
 * the first thing to check when an expected rule is missing.
 */
export interface RelationStateSummary {
  usable: boolean;
  reason?: NotComputed;
  method: string;
  threshold: number;
  /** "detector" or "confirmed": a rule computed against a corrected threshold is a different claim. */
  threshold_source: string;
  classification: "continuous" | "session_based" | "intermittent" | "status" | "";
  active_buckets: number;
  idle_buckets: number;
  unknown_buckets: number;
  duty_cycle: number;
  /**
   * How many aligned buckets carried a value, populated whether or not a state series
   * could be derived. It is what separates "the read came back empty" from "the read
   * came back full and no idle/active split was found in it" — both otherwise report
   * every bucket unknown.
   */
  observed_buckets: number;
}

export interface RelationMember {
  ref: SeriesRef;
  label: string;
  device_name?: string;
  service_name?: string;
  aspect_id?: string;
  aspect_name?: string;
  profile_id: string;
  unit: string;
  kind: ValueKind | "";
  state: RelationStateSummary;
}

/** The 2×2 table for a pair, with the four counts that make the ratios checkable. */
export interface Contingency {
  active_active: number;
  active_idle: number;
  idle_active: number;
  idle_idle: number;
  observed: number;
  active_rate_a: number;
  active_rate_b: number;
}

export interface ConditionedContingency {
  dimension: string;
  bucket: string;
  contingency: Contingency;
}

export interface PairRelation {
  a: number;
  b: number;
  overall: Contingency;
  conditions: ConditionedContingency[];
}

export interface RuleTerm {
  member: number;
  label: string;
  state: MemberState;
}

/** A condition under which a rule demonstrably does not hold (§5.5). */
export interface RuleException {
  dimension: string;
  bucket: string;
  from_hour?: number;
  to_hour?: number;
  samples: number;
  confidence: number;
  drop: number;
}

export interface RuleDecision {
  decision_id: string;
  created_at: string;
  created_by: string;
  rule_id: string;
  relation_id: string;
  detector_version?: string;
  action: "confirm" | "correct" | "reject";
  computed: DecidedRule;
  confirmed?: DecidedRule;
  note?: string;
}

export interface DecidedRule {
  statement: string;
  anomaly?: string;
  support?: number;
  confidence?: number;
  lift?: number;
  exceptions: RuleException[];
}

/**
 * A candidate rule. Never a configured rule and never an anomaly definition until
 * the developer confirms it (§5.5, D28) — which is what the advisory field says in
 * the document itself.
 */
export interface CandidateRule {
  rule_id: string;
  antecedent: RuleTerm;
  consequent: RuleTerm;
  statement: string;
  anomaly: string;
  support: number;
  confidence: number;
  lift: number;
  samples: number;
  violations: number;
  /** Ordinal, and never "certain": that level is reserved for confirmed values (D23). */
  strength: Confidence;
  exceptions: RuleException[];
  decision?: RuleDecision;
  advisory: string;
}

export interface RuleParams {
  min_support: number;
  min_confidence: number;
  min_lift: number;
  min_samples: number;
  hour_buckets: number;
  exception_drop: number;
}

export interface RelationConditioning {
  hour_of_day: boolean;
  weekday_weekend: boolean;
}

export interface RelationProfile {
  relation_id: string;
  detector_version: string;
  cache_key: string;
  computed_at: string;
  tier: string;
  window: Window;
  group_time: string;
  grid_seconds: number;
  buckets: number;
  observed: number;
  members: RelationMember[];
  params: RuleParams;
  conditioning: RelationConditioning;
  pairs: PairRelation[];
  candidate_rules: CandidateRule[];
  candidate_set_id?: string;
  reads: RelationReads;
  notes: string[];
}

export interface RelationRequest {
  members: { device_id: string; service_id: string; variable_path: string; label?: string }[];
  window?: { from: string; to: string };
  grid_seconds?: number;
  params?: Partial<RuleParams>;
  conditioning?: RelationConditioning;
  candidate_set_id?: string;
}

export interface RuleDecisionRequest {
  rule_id: string;
  action: "confirm" | "correct" | "reject";
  confirmed?: DecidedRule;
  note?: string;
}

// --- M4: the developer's own pod (SPEC §5.6) ---

export interface KernelStatus {
  user: string;
  server_ready: boolean;
  /** "spawn" or "stop" while the Hub is working. A cold start takes up to a minute. */
  server_pending?: string;
  server_url?: string;
  started?: string;
  last_activity?: string;
  /** Which workbench's kernel this is, absent for the default one. */
  workbench?: string;
  kernel_id?: string;
  /**
   * How many kernels ODE is holding in this pod. Each is a Python process under one
   * memory limit, which is what a developer needs to see when a run is OOM-killed.
   */
  kernel_count: number;
  kernel_name?: string;
  profile?: string;
  /** Busy in *this* workbench. Another one being busy says nothing about this. */
  busy: boolean;
  /** The persistent working directory. Only what is written here survives the pod. */
  workspace: string;
  /**
   * Where this kernel actually runs, relative to the workspace and empty for its
   * root — the workbench's checkout, so a cell's relative paths land beside the
   * operator's code.
   */
  directory?: string;
  workspace_ready: boolean;
}

export interface KernelFile {
  name: string;
  path: string;
  type: string;
  size: number;
  last_modified: string | null;
}

export interface KernelFiles {
  workspace: string;
  path: string;
  entries: KernelFile[];
}

/** One thing a running cell produced. The stream always ends with a `done`. */
export interface KernelEvent {
  kind: "stream" | "execute_result" | "display_data" | "error" | "status" | "execute_input" | "done";
  stream?: string;
  text?: string;
  /** Renderings other than text/plain, keyed by media type. */
  mime?: Record<string, string>;
  execution_count?: number;
  error_name?: string;
  error_value?: string;
  traceback?: string[];
  state?: string;
  status?: string;
  truncated?: boolean;
  error?: string;
}


// --- M7 ---

/** The GitHub account ODE holds a token for. Never the token (§5.11 item 1). */
export interface GitHubIdentity {
  login: string;
  name?: string;
  avatar_url?: string;
  scopes?: string[];
  connected_at: string;
  /**
   * What the grant lacks of what ODE asked for. A developer can narrow it on the
   * consent screen, and without `workflow` a push that touches the build workflow
   * is rejected — so this is shown before the first push, not after.
   */
  missing_scopes?: string[];
}

export interface RepoConnection {
  connected: boolean;
  scopes_requested: string[];
  identity?: GitHubIdentity;
}

export interface RepoAuthorize {
  url: string;
  state: string;
  scopes: string[];
  /** Reported because it has to match the OAuth app's registered callback exactly. */
  redirect_uri: string;
}

export interface GitHubRepository {
  full_name: string;
  name: string;
  owner: string;
  description?: string;
  private: boolean;
  default_branch: string;
  clone_url: string;
  html_url: string;
  pushed_at?: string;
  /** False means selecting it will work and pushing will not. */
  can_push: boolean;
  empty: boolean;
}

export interface RepoLink {
  /** The working context this checkout belongs to. */
  workbench_id: string;
  full_name: string;
  name: string;
  owner: string;
  default_branch: string;
  private: boolean;
  clone_url: string;
  html_url: string;
  /** Where the checkout is, relative to the workspace on the developer's PVC. */
  path: string;
  /** The Operator Lib the scaffold pinned (D15). Absent if ODE did not scaffold it. */
  operator_lib_ref?: string;
  scaffolded_at?: string;
  selected_at: string;
}

/** One entry of `git status`. A file can be both staged and unstaged. */
export interface RepoChange {
  path: string;
  kind: "modified" | "added" | "deleted" | "renamed" | "copied" | "untracked" | "unmerged" | "typechange";
  staged: boolean;
  unstaged: boolean;
  renamed_from?: string;
}

export interface RepoScaffoldState {
  present: string[];
  missing: string[];
  complete: boolean;
}

/**
 * One working context: a repository checkout on the developer's PVC and the kernel
 * that runs in it.
 *
 * A workbench holds at most one repository, and a repository is open in at most one
 * workbench — two kernels and two streams of git commands in one working tree is a
 * corrupted index, so the backend refuses it.
 */
export interface Workbench {
  id: string;
  /** The developer's own name for it. Empty falls back to the repository. */
  title?: string;
  /** The repository open in it. `full_name` is empty until one is selected. */
  link: RepoLink;
  created_at: string;
  last_used_at: string;
}

/** What a workbench is called on screen, in the order the backend falls back. */
export const workbenchLabel = (bench: Workbench): string =>
  bench.title || bench.link.full_name || bench.id;

export interface RepoStatus {
  link: RepoLink;
  cloned: boolean;
  workspace: string;
  branch?: string;
  upstream?: string;
  ahead: number;
  behind: number;
  /** Ahead and behind at once: the case that needs a human. */
  diverged: boolean;
  detached: boolean;
  /** A clone of an empty repository: a branch with no commits yet. */
  unborn: boolean;
  head?: string;
  head_subject?: string;
  head_date?: string;
  remote?: string;
  remote_mismatch?: boolean;
  changes: RepoChange[];
  dirty: boolean;
  /** Whether this answer included a fetch. A stale zero is not agreement. */
  fetched: boolean;
  scaffold: RepoScaffoldState;
}

export interface RepoScaffoldResult {
  written: string[];
  skipped: string[];
  operator_lib_ref: string;
  hint: string;
}

export interface RepoCommit {
  sha: string;
  subject: string;
  files: number;
  branch: string;
}

export interface RepoPush {
  branch: string;
  remote: string;
  /** git's own reporting: a rejected push and a pull request URL both arrive here. */
  output?: string;
  head_sha?: string;
}

/** One node of the working copy's tree. `.git` is the only thing excluded (D14). */
export interface RepoNode {
  name: string;
  path: string;
  type: "file" | "directory";
  size: number;
  modified?: string;
  children?: RepoNode[];
  /** Entries the walk was not allowed to report. */
  elided?: number;
}

export interface RepoTree {
  root: string;
  tree: RepoNode;
  excluded: string[];
}

export interface RepoFile {
  path: string;
  size: number;
  text: string;
  /** A binary file is shown as one rather than handed to an editor that would corrupt it. */
  binary: boolean;
  truncated: boolean;
  modified?: string;
  language?: string;
}

export interface RepoWriteResult {
  path: string;
  size: number;
  repository: string;
  /** Always false. Writing a file changes the working copy and nothing else. */
  committed: boolean;
}

// --- M8 ---

/** A Ray job's state, in Ray's own vocabulary (§5.12).
 *
 * Not translated: these are the strings the Ray dashboard beside the pane shows,
 * and a second vocabulary would give a developer two answers to reconcile. */
export type ExperimentStatus = "PENDING" | "RUNNING" | "STOPPED" | "SUCCEEDED" | "FAILED";

/** One submitted experiment, as ODE records it.
 *
 * This is the only record of the join between a Ray submission, an MLflow run and
 * the commit the job was built from: Ray forgets a submission when the cluster
 * restarts, and MLflow knows the run but not which working copy produced it. */
export interface Experiment {
  experiment_id: string;
  /** Ray's own id for the job. ODE mints it rather than letting Ray, so a
   * resubmitted request is refused instead of becoming a second job. */
  submission_id: string;
  mlflow_run_id: string;
  mlflow_experiment_id: string;
  /** The per-user experiment name of D17, deterministic from the developer and the
   * repository — so a developer's runs land in one experiment across sessions. */
  mlflow_experiment_name: string;
  /** The chat session it was launched from, when it was launched from one. */
  session_id?: string;
  repository: string;
  /** The commit the job package was built from. §5.11 item 7: every run is
   * reproducible from a specific code state, and this is that state. */
  commit_sha: string;
  branch?: string;
  entrypoint: string;
  /** The `gcs://` URI of the working directory Ray unpacks. Its name is a hash of
   * the archive, which is why two launches from one commit share it. */
  package_uri: string;
  package_bytes: number;
  /** True when the archive was already on the cluster and was not uploaded again. */
  package_reused: boolean;
  status: ExperimentStatus;
  /** Ray's own message for a job that failed. Never a log. */
  message?: string;
  /** Whether the job carries a token minted for it (§3.1 item 6) or the
   * developer's session token. False is a supported deployment and a stated
   * limitation, not a fault — see `ExperimentCredential`. */
  scoped_credential: boolean;
  submitted_at: string;
  updated_at: string;
  started_at?: string;
  ended_at?: string;
}

/** What the job will authenticate to the platform with, and for how long.
 *
 * Part of the launch answer rather than a log line because the difference decides
 * whether a long run is viable: a job reads its training data from the platform
 * directly (§5.3.4), and a session token expires while the run is still going. */
export interface ExperimentCredential {
  /** "exchanged" — minted for this job through the Keycloak token exchange — or
   * "session", the developer's own. */
  source: "exchanged" | "session";
  /** As the issuer reported it. Absent for a session token: ODE does not validate
   * tokens (§3.1 step 2) and does not read their expiry. */
  expires_in_seconds?: number;
  /** The limitation, stated. True means a run outliving the developer's session
   * loses its platform access partway through. */
  expires_with_session: boolean;
  /** The sentence to show. Written by the backend so every surface says the same. */
  note?: string;
}

/** One submission, made. */
export interface ExperimentLaunch extends Experiment {
  credential: ExperimentCredential;
  /** Where a browser should open MLflow, so a run can be opened without the SPA
   * knowing ODE's configuration. */
  mlflow_tracking_uri?: string;
  /** Things that did not stop the launch but that the developer should read — a
   * token exchange that answered with a shorter lifetime than configured, say. */
  warnings?: string[];
}

/** One metric, this run against the previous one (§5.13).
 *
 * `lower_is_better` is beside `direction` on purpose: whether a smaller number is
 * an improvement is a property of the metric, and without the developer's
 * evaluation criteria the backend goes by the metric's *name*. Showing the rule
 * beside the verdict is what keeps it from reading as a judgement. */
export interface MetricDelta {
  metric: string;
  previous: number;
  current: number;
  delta: number;
  direction: "better" | "worse" | "unchanged";
  lower_is_better: boolean;
}

/** An explicit non-result about an evaluation criterion (§5.4.6, D24).
 *
 * The same `status: "not_computed"` word `NotComputed` above uses, on purpose: a
 * reader — and an assistant — should meet one vocabulary for "this could not be
 * determined", not one per feature. The *reasons* are their own set, because the
 * profiler's closed list is about reading a series and these are about reading a
 * file in a repository. Rendering is shared; the repairs are not. */
export interface CriterionNotComputed {
  status: "not_computed";
  /** Each names a different repair, which is the whole point of telling them
   * apart. `no_developer_credential` in particular is not a fault: the summary was
   * built when the run finished, with ODE's own Ray and MLflow credential and
   * nobody connected, and `evaluation.yaml` is read on the developer's behalf. */
  reason:
    | "no_criteria_file"
    | "criteria_unreadable"
    | "criteria_unparseable"
    | "no_criterion_stated"
    | "no_threshold"
    | "metric_not_reported"
    | "no_developer_credential";
  detail: string;
}

/** Whether a criterion was met: `true`, `false`, or an explicit non-result.
 *
 * **The third arm is not a nicety.** A missing `evaluation.yaml`, a file outside
 * the subset ODE reads, a metric the run never logged and a summary built before
 * the developer was back are four different facts, and a boolean would have
 * flattened every one of them to "the run missed the target". Render the object
 * form as *unknown with a reason*, never as a failure. */
export type Verdict = boolean | CriterionNotComputed;

/** One of the developer's evaluation criteria, applied to a run (§5.13, M9).
 *
 * Read out of `evaluation.yaml` **at the run's commit** — a criterion is part of
 * the code state a run came from (§5.11 item 7), so a threshold the developer
 * tightened while the job ran does not retroactively fail it. ODE reads that file
 * and never writes it: §5.8 denies every tool that could, and `write_file` refuses
 * the path.
 *
 * Where the developer's file names no metric, a criterion the run *tagged itself
 * with* is the fallback, and `source` says which of the two this is. */
export interface EvaluationCriterion {
  /** Empty exactly when there was no criterion to evaluate; `met` then says why. */
  metric?: string;
  /** Absent when the criteria file names a metric and no target for it, and when
   * there was no criterion at all. Present — and possibly `0` — when the developer
   * wrote one, which the scaffold's own file does. Rendering an absent threshold as
   * zero would show a target nobody set. */
  threshold?: number;
  /** What the run logged for the metric. Absent — not zero — when it logged none,
   * because a metric of exactly zero is a real reading. */
  value?: number;
  met: Verdict;
  /** `goal_stated` says whether the file named the direction or whether it was
   * inferred from the metric's name. Shown beside the verdict for the reason
   * `lower_is_better` is shown beside a delta: a verdict whose rule is invisible
   * reads as a judgement. */
  goal: "minimise" | "maximise";
  goal_stated: boolean;
  lower_is_better: boolean;
  /** Where the criterion came from, so nobody reads it as ODE's judgement. */
  source: string;
}

export interface ExperimentResourceUsage {
  duration_s: number;
  /** Only when the job logged a memory metric. Absent rather than zero, for D24's
   * reason: a zero read as "used no memory" would be a fabricated finding. */
  peak_memory_mb?: number;
  peak_memory_source?: string;
}

/** §5.13's compact structured summary — the only shape a finished run is ever
 * reduced to, for the pane and for the assistant alike.
 *
 * Params, metrics and tags. **Never logs**, never stdout, never an artifact: an
 * LLM reading a training process's raw output is the same category of mistake as
 * an LLM reading a raw series (§4). Logs have their own route. */
export interface ExperimentSummary {
  run_id: string;
  experiment_id: string;
  submission_id: string;
  commit_sha: string;
  repository?: string;
  entrypoint?: string;
  status: ExperimentStatus;
  /** Whether the run is in a state it will not leave. A false here means the
   * metrics below are a snapshot rather than a result. */
  finished: boolean;
  params: Record<string, string>;
  /** The latest value per key, not the history. */
  metrics: Record<string, number>;
  tags: Record<string, string>;
  /** Empty for the first run of an experiment, which the note says in words —
   * an empty comparison means "nothing to compare against", never "no change". */
  comparison_to_previous: MetricDelta[];
  /** Always present, never absent. A run with no criteria file, a file ODE could
   * not read, a summary built before the developer was back — each is a criterion
   * whose `met` carries a reason, not a criterion that is missing. */
  evaluation_criteria: EvaluationCriterion;
  /** The other metrics the file asks to watch. Beyond §5.13's literal shape, and
   * present because the scaffold's own `evaluation.yaml` has a `secondary_metrics`
   * key and dropping it would make the summary a partial reading of it. */
  secondary_criteria?: EvaluationCriterion[];
  resource_usage: ExperimentResourceUsage;
  /** What the comparison is against, so a claim about an improvement is checkable. */
  previous_run_id?: string;
  started_at?: string;
  ended_at?: string;
  note?: string;
}

/** The concrete next adjustment an interpretation proposed, or why there is none
 * (§5.13, M9).
 *
 * A struct rather than a string because "the assistant proposed nothing" and "the
 * assistant proposed the empty string" are different facts. `text` is present
 * exactly when `status` is absent. */
export interface Proposal {
  /** A fingerprint of what was proposed, for this experiment. A decision is keyed
   * by it, which is what makes a rejection survive the run being interpreted
   * again — the same wording produces the same id. */
  proposal_id?: string;
  text?: string;
  status?: "not_computed";
  /** `not_interpreted_yet` is a turn that has not run because the developer has
   * not been connected since the run finished; `no_proposal_stated` is an assistant
   * that read the run and named no next step. Two different facts, and neither is
   * "there is nothing to change". */
  reason?: "no_proposal_stated" | "not_interpreted_yet";
  detail?: string;
}

/** The three answers of §5.13's last sentence. */
export type ProposalDecisionKind = "accepted" | "edited" | "rejected";

/** One developer's verdict on one proposal, append-only.
 *
 * The same shape a `RuleDecision` and a `ProfileOverrideRecord` have, and for the
 * same reason: a developer who changes their mind adds a record rather than
 * replacing one, and "the assistant proposed X and the developer edited it to Y" is
 * the finding. */
export interface ProposalDecision {
  decision_id: string;
  created_at: string;
  created_by: string;
  experiment_id: string;
  run_id?: string;
  proposal_id: string;
  decision: ProposalDecisionKind;
  /** The assistant's own wording, copied into the record so it stays readable
   * after the interpretation it came from has been recomputed. */
  proposed: string;
  /** The developer's own form of the adjustment. Present only for an edit. */
  edited?: string;
  note?: string;
  /** **Always false**, and serialised rather than omitted so a reader meets D28
   * rather than having to know it: accepting a proposal records agreement and
   * changes nothing. */
  binding: boolean;
}

/** One finished run, interpreted (§5.13).
 *
 * Recomputed rather than stored: the summary comes from MLflow, the interpretation
 * from the conversation the assistant wrote it in, and only the decisions come from
 * a table — the split §5.4.3 makes between a recomputable artifact and a record of
 * human judgement.
 *
 * `interpreted_at` absent means the turn has not run yet, which for a run whose
 * developer has not been connected since it finished is the normal state rather
 * than a failure: the summary is built with ODE's own Ray and MLflow credential,
 * and the turn waits for the developer's own token (§3.1 items 3 and 5). */
export interface Interpretation {
  experiment_id: string;
  run_id: string;
  session_id: string;
  summary: ExperimentSummary;
  /** When the summary was built, which is when the run finished — not when the
   * developer came back to it. */
  summary_at: string;
  interpretation: string;
  interpreted_at?: string;
  proposal: Proposal;
  /** The decision that currently stands, merged at read time. */
  decision?: ProposalDecision;
  /** The whole log for this proposal, oldest first. */
  decisions: ProposalDecision[];
}

/** A job's driver output, tail-capped.
 *
 * The developer's own view. Deliberately not available to the assistant, which is
 * §5.13 made structural rather than conventional: this has a route and no tool. */
export interface ExperimentLogs {
  submission_id: string;
  logs: string;
  truncated: boolean;
}

/** One service's framing verdict (D6).
 *
 * The backend half of the probe: it asks the service for its `X-Frame-Options`
 * and `Content-Security-Policy: frame-ancestors`. The frontend half is still the
 * pane's — a hidden iframe with a load timeout, falling back to a link-only card —
 * because a header can permit framing while the page still refuses to render in
 * one, and only a browser finds that out.
 *
 * **"unknown" is a real answer.** ODE is inside the cluster and the browser is
 * not, so a service ODE cannot reach may frame perfectly: try the iframe anyway. */
export interface EmbedProbe {
  service: "ray" | "mlflow";
  url: string;
  embeddable: "yes" | "no" | "unknown";
  /** The header that decided it, or why nothing did. A verdict without the header
   * is not actionable by whoever would have to change it. */
  reason: string;
  probed_at: string;
  /** Zero when the service answered nothing at all. */
  status?: number;
}

export interface EmbedReport {
  services: EmbedProbe[];
  /** Whether this came from the TTL cache rather than a fresh probe. */
  cached: boolean;
  ttl: string;
  as_of: string;
}

export const api = {
  session: () => get<Session>("/session"),
  aspectTree: () => get<{ tree: AspectTreeNode[] }>("/ontology/aspect-tree"),
  functions: (rdfType: "measuring" | "controlling" = "measuring") =>
    get<{ functions: OntologyFunction[]; rdf_type: string }>(
      `/ontology/functions?rdf_type=${rdfType}`,
    ),
  devices: (search: string) =>
    get<DeviceList>(`/devices?limit=100${search ? `&search=${encodeURIComponent(search)}` : ""}`),

  availability: (deviceId: string) =>
    get<AvailabilityList>(`/timeseries/availability${query({ device_id: deviceId })}`),
  usage: (deviceIds: string[]) =>
    get<UsageList>(`/timeseries/usage${query({ device_ids: deviceIds.join(",") })}`),

  quickProfiles: (params: QuickProfileQuery = {}) =>
    get<QuickProfileList>(
      `/quick-profiles${query({
        search: params.search,
        from: params.from,
        to: params.to,
        include_unqueryable: params.includeUnqueryable,
        limit: 100,
      })}`,
    ),

  selection: (body: SelectionRequest) => post<SelectionResult>("/selection", body),

  createProfiles: (body: ProfileRequest) => post<ProfileResult>("/profiles", body),
  profile: (id: string) => get<SeriesProfile>(`/profiles/${encodeURIComponent(id)}`),
  projection: (id: string, tokenBudget?: number) =>
    get<LLMProfileView>(
      `/profiles/${encodeURIComponent(id)}/projection${query({ token_budget: tokenBudget })}`,
    ),
  sessions: (id: string, params: { limit?: number; cursor?: string } = {}) =>
    get<SessionPage>(`/profiles/${encodeURIComponent(id)}/sessions${query(params)}`),
  createOverride: (id: string, body: OverrideRequest) =>
    post<{ override: ProfileOverrideRecord; confirmable: Record<string, string> }>(
      `/profiles/${encodeURIComponent(id)}/overrides`,
      body,
    ),

  // --- M3 ---

  providers: () => get<{ providers: ProviderInfo[]; default: string }>("/llm/providers"),
  toolSurface: () => get<ToolSurface>("/llm/tools"),

  chatSessions: () => get<{ sessions: ChatSession[] }>("/chat/sessions"),
  createChatSession: (body: {
    title?: string;
    provider?: string;
    model?: string;
    exposure_tier?: Tier;
    /** The working context to act in. Absent means the developer's only one. */
    workbench_id?: string;
  }) => post<ChatSession>("/chat/sessions", body),
  chatSession: (id: string) => get<ChatSessionDetail>(`/chat/sessions/${encodeURIComponent(id)}`),
  deleteChatSession: (id: string) => del(`/chat/sessions/${encodeURIComponent(id)}`),
  /**
   * The developer's own name for a conversation, in place of the first few words of
   * its opening message. An empty title clears it, and the next message titles the
   * session again.
   */
  renameChatSession: (id: string, title: string) =>
    put<ChatSession>(`/chat/sessions/${encodeURIComponent(id)}/title`, { title }),
  /**
   * Moves a conversation to another workbench: which checkout its file tools write
   * into, and which kernel its cells run in.
   *
   * An empty id clears the assignment, which the backend reads as "my only one".
   * The move leaves a note in the conversation — every file and cell result above it
   * happened in the previous checkout — so a caller showing the history has to read
   * it back afterwards.
   *
   * Refused with 400 while a turn is running on the session: that turn is acting in
   * the workbench the move would take it away from.
   */
  moveChatSession: (id: string, workbenchId: string) =>
    put<ChatSession>(`/chat/sessions/${encodeURIComponent(id)}/workbench`, {
      workbench_id: workbenchId,
    }),
  /** The developer's tier control (§3.2). There is no LLM tool for this. */
  setTier: (id: string, tier: Tier) =>
    put<ChatSession>(`/chat/sessions/${encodeURIComponent(id)}/tier`, { exposure_tier: tier }),
  /**
   * The standing answer to a `run_code` confirmation whose code the backend
   * recognises as an inspection. Also has no LLM tool, for the same reason.
   */
  setAutoRun: (id: string, on: boolean) =>
    put<ChatSession>(`/chat/sessions/${encodeURIComponent(id)}/auto-run`, { auto_run: on }),
  tierChanges: (id: string) =>
    get<{ changes: TierChange[] }>(`/chat/sessions/${encodeURIComponent(id)}/tier-changes`),

  adminLimits: () => get<LimitsSurface>("/admin/limits"),
  adminSubjectLimits: (sub: string) =>
    get<{ subject: string; effective: Limits; spend: Spend }>(
      `/admin/limits/${encodeURIComponent(sub)}`,
    ),
  setAdminLimits: (sub: string, limits: Limits) =>
    put<{ subject: string; effective: Limits }>(
      sub ? `/admin/limits/${encodeURIComponent(sub)}` : "/admin/limits",
      limits,
    ),
  adminUsage: (params: { sub?: string; period?: string; limit?: number } = {}) =>
    get<{ usage: UsageRecord[]; spend: Spend; period: string; currency: string }>(
      `/admin/usage${query(params)}`,
    ),
  adminToolCalls: (params: { sub?: string; period?: string; limit?: number } = {}) =>
    get<{ tool_calls: ToolCallRecord[]; period: string }>(`/admin/tool-calls${query(params)}`),

  // --- M4 ---

  /** Reads what is running. Starts nothing, so it is safe to poll. */
  kernelStatus: () => get<KernelStatus>(inWorkbench("/kernel")),
  /**
   * Spawns the pod, starts a kernel and installs the platform token. Called on
   * pane open: a cold start is up to a minute, and §5.6 wants that spent while
   * the developer is still reading rather than after they press run.
   */
  kernelEnsure: () => post<KernelStatus>(inWorkbench("/kernel"), {}),
  kernelRestart: () => post<KernelStatus>(inWorkbench("/kernel/restart"), {}),
  kernelInterrupt: () => post<{ interrupted: boolean }>(inWorkbench("/kernel/interrupt"), {}),
  /** Ends the kernel. The pod stays: it is the developer's, and their files are on it. */
  kernelShutdown: () => del(inWorkbench("/kernel")),
  kernelFiles: (path?: string) =>
    get<KernelFiles>(inWorkbench(`/kernel/files${query({ path })}`)),

  // --- M5 ---

  /** Validates and stores a specification. Reads no values (§5.9). */
  createChart: (body: ChartRequest) => post<ChartCreated>("/charts", body),
  charts: (params: { sessionId?: string; limit?: number } = {}) =>
    get<{ charts: ChartSpec[]; count: number }>(
      `/charts${query({ session_id: params.sessionId, limit: params.limit })}`,
    ),
  chart: (id: string) => get<ChartSpec>(`/charts/${encodeURIComponent(id)}`),
  /**
   * The values behind a chart, read as the developer. The window and bucket
   * overrides are how the pane zooms without minting a second chart.
   */
  chartData: (id: string, params: { from?: string; to?: string; groupTime?: string } = {}) =>
    get<ChartData>(
      `/charts/${encodeURIComponent(id)}/data${query({
        from: params.from,
        to: params.to,
        group_time: params.groupTime,
      })}`,
    ),
  /** §5.10, into the profiler's overlay. There is no LLM tool for this (§5.8). */
  confirmChart: (id: string, body: ChartConfirmRequest) =>
    post<ChartConfirmation>(`/charts/${encodeURIComponent(id)}/confirmations`, body),
  deleteChart: (id: string) => del(`/charts/${encodeURIComponent(id)}`),

  // --- M6 ---

  /** Sets proposed from an aspect node. Reads no values, so this is tier L0 (§5.5). */
  candidateSets: (params: {
    aspectId: string;
    includeDescendants?: boolean;
    limit?: number;
    maxMembers?: number;
  }) =>
    get<RelationProposal>(
      `/relations/candidate-sets${query({
        aspect_id: params.aspectId,
        include_descendants: params.includeDescendants,
        limit: params.limit,
        max_members: params.maxMembers,
      })}`,
    ),
  /**
   * The HTTP form of a relational pass. The pane uses the WebSocket instead: this
   * profiles every participating service before it aligns them, which is the longest
   * read ODE makes, and the socket can cancel it.
   */
  createRelation: (body: RelationRequest) => post<RelationProfile>("/relations", body),
  relation: (id: string) => get<RelationProfile>(`/relations/${encodeURIComponent(id)}`),
  /** §5.10. Developer action only; there is no LLM tool for it (§5.8). */
  decideRule: (id: string, body: RuleDecisionRequest) =>
    post<RuleDecision>(`/relations/${encodeURIComponent(id)}/rule-decisions`, body),
  ruleDecisions: (ruleId: string) =>
    get<{ rule_id: string; decisions: RuleDecision[] }>(
      `/relations/rule-decisions${query({ rule_id: ruleId })}`,
    ),

  // --- M7 ---

  /**
   * The developer's working contexts. One repository checkout and one kernel each,
   * so two operators can be in flight at once without either one's working copy
   * moving under the other.
   */
  workbenches: () => get<{ workbenches: Workbench[]; max: number }>("/workbenches"),
  /** Opens an empty one. The repository is chosen afterwards, with repoSelect. */
  createWorkbench: (title?: string) => post<Workbench>("/workbenches", { title: title ?? "" }),
  renameWorkbench: (id: string, title: string) =>
    put<Workbench>(`/workbenches/${encodeURIComponent(id)}`, { title }),
  /** Closes one. The checkout stays on the PVC — it may hold uncommitted work. */
  deleteWorkbench: (id: string) => del(`/workbenches/${encodeURIComponent(id)}`),

  repoConnection: () => get<RepoConnection>("/repo/connection"),
  /** Begins the OAuth flow. The state is single-use and bound to this developer. */
  repoAuthorize: () => post<RepoAuthorize>("/repo/connection/authorize", {}),
  /** Completes it, with the code GitHub returned to the SPA's callback. */
  repoConnect: (code: string, state: string) =>
    post<GitHubIdentity>("/repo/connection", { code, state }),
  repoDisconnect: () => del("/repo/connection"),

  repoRepositories: () => get<{ repositories: GitHubRepository[] }>("/repo/repositories"),
  /**
   * Creates an *empty* repository and scaffolds the working copy. Nothing is
   * committed: the developer's own commit is the repository's first (§5.11 item 5).
   */
  repoCreate: (body: {
    name: string;
    description?: string;
    private?: boolean;
    organisation?: string;
    scaffold?: boolean;
  }) => workspace((wb) => post<RepoStatus>(inWorkbench("/repo/repositories", wb), body)),
  repoSelect: (fullName: string, scaffold = false) =>
    workspace((wb) =>
      post<RepoStatus>(inWorkbench("/repo/link", wb), { full_name: fullName, scaffold }),
    ),
  repoUnlink: () => del(inWorkbench("/repo/link")),

  /**
   * `fetch` contacts the remote, which is what makes the divergence current. The
   * pane asks for one when it opens and when the developer presses refresh, and
   * not on every poll.
   */
  repoStatus: (fetch = false) =>
    workspace((wb) => get<RepoStatus>(inWorkbench(`/repo${query({ fetch: fetch || undefined })}`, wb))),
  repoFetch: () => workspace((wb) => post<RepoStatus>(inWorkbench("/repo/fetch", wb), {})),
  repoScaffold: () =>
    workspace((wb) => post<RepoScaffoldResult>(inWorkbench("/repo/scaffold", wb), {})),

  repoCommit: (message: string, paths?: string[]) =>
    workspace((wb) => post<RepoCommit>(inWorkbench("/repo/commit", wb), { message, paths })),
  repoPush: (branch?: string) =>
    workspace((wb) => post<RepoPush>(inWorkbench("/repo/push", wb), { branch })),
  repoStash: (message?: string) =>
    workspace((wb) => post<RepoStatus>(inWorkbench("/repo/stash", wb), { message })),
  /** The one destructive action, so the flag is required on both sides. */
  repoDiscard: () =>
    workspace((wb) => post<RepoStatus>(inWorkbench("/repo/discard", wb), { confirm: true })),

  repoFiles: () => workspace((wb) => get<RepoTree>(inWorkbench("/repo/files", wb))),
  repoFile: (path: string) =>
    workspace((wb) => get<RepoFile>(inWorkbench(`/repo/files/content${query({ path })}`, wb))),
  repoWriteFile: (path: string, content: string) =>
    workspace((wb) =>
      put<RepoWriteResult>(inWorkbench("/repo/files/content", wb), { path, content }),
    ),
  repoDeleteFile: (path: string, recursive = false) =>
    workspace((wb) =>
      del(inWorkbench(`/repo/files/content${query({ path, recursive: recursive || undefined })}`, wb)),
    ),
  repoMakeDir: (path: string) =>
    workspace((wb) => post<{ created: boolean }>(inWorkbench("/repo/files/directory", wb), { path })),
  // --- M8 ---

  /**
   * Submits a training run from the **committed** repository state and creates the
   * MLflow run it will log to. A working copy with uncommitted changes answers 409
   * with `needs: "commit"` and the paths — the pane offers a commit rather than an
   * error, because a run's commit SHA is only meaningful if it is what ran.
   */
  launchExperiment: (body: {
    entrypoint?: string;
    env_vars?: Record<string, string>;
    run_name?: string;
    session_id?: string;
  } = {}) => post<ExperimentLaunch>("/experiments", body),

  /** The caller's own experiments, newest first. Statuses are refreshed from Ray
   * for the runs that have not finished, and only those, so polling a list of
   * finished runs costs the cluster nothing. */
  experiments: (limit?: number) =>
    get<{
      experiments: Experiment[];
      count: number;
      ray_url: string;
      mlflow_url: string;
    }>(`/experiments${query({ limit })}`),

  experiment: (id: string) => get<Experiment>(`/experiments/${encodeURIComponent(id)}`),

  /** §5.13's structured summary, including the comparison against the previous run
   * of the same experiment — usually the number that answers whether a change
   * helped. Carries no logs. */
  experimentResults: (id: string) =>
    get<ExperimentSummary>(`/experiments/${encodeURIComponent(id)}/results`),

  /** The developer's own view of a job's output. There is no LLM tool for this, and
   * that is the design rather than an omission (§5.13). */
  experimentLogs: (id: string) =>
    get<ExperimentLogs>(`/experiments/${encodeURIComponent(id)}/logs`),

  stopExperiment: (id: string) =>
    post<Experiment>(`/experiments/${encodeURIComponent(id)}/stop`, {}),

  /** D6's backend half. Pass `refresh` when the developer presses re-probe; the
   * pane should still try a hidden iframe with a timeout regardless of the answer,
   * because "unknown" means ODE could not tell rather than that framing fails. */
  embedProbes: (refresh = false) =>
    get<EmbedReport>(`/experiments/embed${query({ refresh: refresh || undefined })}`),

  /** §5.13's interpretation of one finished run: the summary, the assistant's
   * reading of it, the concrete next adjustment it proposed, and the developer's
   * decision on that proposal if they have given one.
   *
   * The interpretation itself is delivered **into the conversation** — that is
   * where the developer reads it — and this route is the pane's view of the same
   * thing as a document. */
  interpretation: (id: string) =>
    get<Interpretation>(`/experiments/${encodeURIComponent(id)}/interpretation`),

  /** Accept, edit or reject a proposed next experiment (§5.13's last sentence).
   *
   * `proposal_id` is what the developer was looking at, and a stale one answers
   * **409** rather than recording agreement with something they never read — re-read
   * the interpretation and decide on the proposal that stands.
   *
   * Nothing here is binding (D28). Accepting records agreement and launches
   * nothing; promoting a value into `evaluation.yaml` or the operator config is a
   * separate act the developer performs, and no tool exists for it (§5.8). */
  decideProposal: (
    id: string,
    body: { proposal_id: string; decision: ProposalDecisionKind; edited?: string; note?: string },
  ) =>
    post<Interpretation>(
      `/experiments/${encodeURIComponent(id)}/interpretation/decision`,
      body,
    ),
};
