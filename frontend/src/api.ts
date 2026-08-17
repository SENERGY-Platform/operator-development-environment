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
  ) {
    super(message);
    this.name = "ApiError";
  }

  /** 403 is final: the user lacks the developer role, or may not see this. */
  get isForbidden(): boolean {
    return this.status === 403;
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
    try {
      const body = (await response.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // Response had no JSON body; the status text stands.
    }
    throw new ApiError(response.status, message);
  }
  return (await response.json()) as T;
}

const get = <T>(path: string) => request<T>(path);
const post = <T>(path: string, body: unknown) =>
  request<T>(path, { method: "POST", body: JSON.stringify(body) });

// --- M0 ---

export interface Session {
  user_id: string;
  username: string;
  email: string;
  roles: string[];
  is_admin: boolean;
  exposure_tier: string;
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
  device_type_id: string;
  connection_state: string;
  shared?: boolean;
  permissions?: Record<string, boolean>;
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
  device_id: string;
  service_id: string;
  variable_path: string;
}

export interface Window {
  from: string;
  to: string;
}

export interface RawWindow extends Window {
  source: "default" | "developer_override";
  truncated: boolean;
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

export interface QuickProfile {
  series_ref: SeriesRef;
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
  raw_available: boolean;
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
};
