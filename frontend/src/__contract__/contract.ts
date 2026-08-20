/*
 * Contract check, not shipped. The JSON beside this file was produced by a
 * running backend, so assigning it to the declared types fails the build on any
 * field the frontend expects and the backend does not send, or vice versa.
 *
 * `satisfies` rather than a plain annotation, because it also rejects an *extra*
 * field: a renamed field would otherwise pass as one absent optional plus one
 * unnoticed extra.
 *
 * Loose<T> widens string-literal unions to string. A JSON import types every
 * string as `string`, so without it the check fails on `confidence: "uncertain"`
 * not being the union member it plainly is — noise that would hide the mismatches
 * actually being looked for. Structure and field sets are still checked exactly.
 */
import type {
  AvailabilityList,
  ChartCreated,
  ChartData,
  ChartSpec,
  ChartConfirmation,
  ChatSession,
  ChatSessionDetail,
  KernelFiles,
  KernelStatus,
  LLMProfileView,
  LimitsSurface,
  ProfileOverrideRecord,
  ProfileResult,
  ProviderInfo,
  QuickProfileList,
  RelationProfile,
  RelationProposal,
  RuleDecision,
  SelectionResult,
  SeriesProfile,
  Session,
  SessionPage,
  Spend,
  TierChange,
  ToolCallRecord,
  ToolSurface,
  UsageRecord,
} from "../api";

import adminLimits from "./admin_limits.json";
import adminToolCalls from "./admin_tool_calls.json";
import adminUsage from "./admin_usage.json";
import availability from "./availability.json";
import chartConfirmation from "./chart_confirmation.json";
import chartCreated from "./chart_created.json";
import chartData from "./chart_data.json";
import chartList from "./charts.json";
import chatProviders from "./chat_providers.json";
import chatSession from "./chat_session.json";
import chatSessions from "./chat_sessions.json";
import chatTierChanges from "./chat_tier_changes.json";
import chatTools from "./chat_tools.json";
import kernelFiles from "./kernel_files.json";
import kernelStatus from "./kernel_status.json";
import override from "./override.json";
import profile from "./profile.json";
import profiles from "./profiles.json";
import projection from "./projection.json";
import quick from "./quick.json";
import relation from "./relation.json";
import relationDecision from "./relation_decision.json";
import relationDecisions from "./relation_decisions.json";
import relationSets from "./relation_sets.json";
import selection from "./selection.json";
import platformSession from "./session.json";
import sessions from "./sessions.json";

type Loose<T> = T extends string
  ? string
  : T extends number | boolean | null | undefined
    ? T
    : T extends readonly (infer U)[]
      ? Loose<U>[]
      : T extends object
        ? { [K in keyof T]: Loose<T[K]> }
        : T;

export const checked = {
  quick: quick satisfies Loose<QuickProfileList>,
  selection: selection satisfies Loose<SelectionResult>,
  profiles: profiles satisfies Loose<ProfileResult>,
  profile: profile satisfies Loose<SeriesProfile>,
  projection: projection satisfies Loose<LLMProfileView>,
  sessions: sessions satisfies Loose<SessionPage>,
  availability: availability satisfies Loose<AvailabilityList>,
  override: override.override satisfies Loose<ProfileOverrideRecord>,

  // M3. Emitted by the API test harness rather than captured from a platform, for
  // the same reason selection.json was — see the README. The field sets are the
  // backend's own marshalling; the values are a fake's.
  session: platformSession satisfies Loose<Session>,
  providers: chatProviders.providers satisfies Loose<ProviderInfo[]>,
  tools: chatTools satisfies Loose<ToolSurface>,
  chatSessions: chatSessions.sessions satisfies Loose<ChatSession[]>,
  chatSession: chatSession satisfies Loose<ChatSessionDetail>,
  tierChanges: chatTierChanges.changes satisfies Loose<TierChange[]>,
  adminLimits: adminLimits satisfies Loose<LimitsSurface>,
  adminUsage: adminUsage.usage satisfies Loose<UsageRecord[]>,
  adminSpend: adminUsage.spend satisfies Loose<Spend>,
  adminToolCalls: adminToolCalls.tool_calls satisfies Loose<ToolCallRecord[]>,

  // M4. Emitted by the API test harness against the in-memory JupyterHub of
  // pkg/kernel/kerneltest, for the same reason as the M3 files above.
  kernelStatus: kernelStatus satisfies Loose<KernelStatus>,
  kernelFiles: kernelFiles satisfies Loose<KernelFiles>,

  // M5. Emitted by the API test harness too. The values are a fake's — the points
  // are a constant — but the annotations are a real profiler run over the harness
  // fixture, so the confirmable field paths in there are the backend's own.
  chartCreated: chartCreated satisfies Loose<ChartCreated>,
  charts: chartList.charts satisfies Loose<ChartSpec[]>,
  chartData: chartData satisfies Loose<ChartData>,
  chartConfirmation: chartConfirmation satisfies Loose<ChartConfirmation>,

  // M6. Emitted by the API test harness over the *real* profiler: the fixture is two
  // synthetic power series a room apart, so the thresholds, the contingency counts and
  // the exception window in here are the detectors' own rather than a fake's. Only the
  // series behind them is synthetic.
  relationSets: relationSets satisfies Loose<RelationProposal>,
  relation: relation satisfies Loose<RelationProfile>,
  relationDecision: relationDecision satisfies Loose<RuleDecision>,
  relationDecisions: relationDecisions.decisions satisfies Loose<RuleDecision[]>,
};
