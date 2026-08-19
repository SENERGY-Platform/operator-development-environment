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
  };
  /** Present only when the LLM surface is configured (M3). */
  max_exposure_tier?: Tier;
  limits?: Limits;
  spend?: Spend;
  providers?: ProviderInfo[];
  /** Present only when an execution backend is configured (M4). */
  kernel?: { workspace: string; kernel: string };
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
}

export interface PendingConfirmation {
  id: string;
  tool: string;
  input: unknown;
  tier: Tier;
  created_at: string;
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

// --- M4: the developer's own pod (SPEC §5.6) ---

export interface KernelStatus {
  user: string;
  server_ready: boolean;
  /** "spawn" or "stop" while the Hub is working. A cold start takes up to a minute. */
  server_pending?: string;
  server_url?: string;
  started?: string;
  last_activity?: string;
  kernel_id?: string;
  kernel_name?: string;
  profile?: string;
  busy: boolean;
  /** The persistent working directory. Only what is written here survives the pod. */
  workspace: string;
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
  createChatSession: (body: { title?: string; provider?: string; model?: string; exposure_tier?: Tier }) =>
    post<ChatSession>("/chat/sessions", body),
  chatSession: (id: string) => get<ChatSessionDetail>(`/chat/sessions/${encodeURIComponent(id)}`),
  deleteChatSession: (id: string) => del(`/chat/sessions/${encodeURIComponent(id)}`),
  /** The developer's tier control (§3.2). There is no LLM tool for this. */
  setTier: (id: string, tier: Tier) =>
    put<ChatSession>(`/chat/sessions/${encodeURIComponent(id)}/tier`, { exposure_tier: tier }),
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
  kernelStatus: () => get<KernelStatus>("/kernel"),
  /**
   * Spawns the pod, starts a kernel and installs the platform token. Called on
   * pane open: a cold start is up to a minute, and §5.6 wants that spent while
   * the developer is still reading rather than after they press run.
   */
  kernelEnsure: () => post<KernelStatus>("/kernel", {}),
  kernelRestart: () => post<KernelStatus>("/kernel/restart", {}),
  kernelInterrupt: () => post<{ interrupted: boolean }>("/kernel/interrupt", {}),
  /** Ends the kernel. The pod stays: it is the developer's, and their files are on it. */
  kernelShutdown: () => del("/kernel"),
  kernelFiles: (path?: string) => get<KernelFiles>(`/kernel/files${query({ path })}`),

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
};
