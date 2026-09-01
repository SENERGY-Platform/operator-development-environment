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
  Experiment,
  ExperimentLaunch,
  ExperimentLogs,
  ExperimentSummary,
  Interpretation,
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
  RepoCommit,
  RepoCommitDraft,
  RepoConnection,
  Workbench,
  RepoFile,
  RepoPush,
  RepoScaffoldResult,
  RepoStatus,
  RepoTree,
  GitHubRepository,
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
import experiment from "./experiment.json";
import experimentInterpretation from "./experiment_interpretation.json";
import experimentInterpretationDecided from "./experiment_interpretation_decided.json";
import experimentLaunch from "./experiment_launch.json";
import experimentLogs from "./experiment_logs.json";
import experimentResults from "./experiment_results.json";
import experimentResultsFailed from "./experiment_results_failed.json";
import experimentList from "./experiments.json";
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
import repoCommit from "./repo_commit.json";
import repoCommitMessage from "./repo_commit_message.json";
import repoConnection from "./repo_connection.json";
import workbenches from "./workbenches.json";
import repoFile from "./repo_file.json";
import repoPush from "./repo_push.json";
import repoRepositories from "./repo_repositories.json";
import repoScaffold from "./repo_scaffold.json";
import repoStatus from "./repo_status.json";
import repoTree from "./repo_tree.json";
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

  // M7. Emitted by the API test harness, and less of it is a fake's than the sets
  // above: the status, the tree, the file and the commit come from a real git
  // working copy in a temporary directory, so the fields in them are git's own
  // answers. Only the GitHub identity and the repository list are invented.
  workbenches: workbenches.workbenches satisfies Loose<Workbench[]>,
  repoConnection: repoConnection satisfies Loose<RepoConnection>,
  repoRepositories: repoRepositories.repositories satisfies Loose<GitHubRepository[]>,
  repoStatus: repoStatus satisfies Loose<RepoStatus>,
  repoTree: repoTree satisfies Loose<RepoTree>,
  repoFile: repoFile satisfies Loose<RepoFile>,
  repoScaffold: repoScaffold satisfies Loose<RepoScaffoldResult>,
  repoCommit: repoCommit satisfies Loose<RepoCommit>,
  repoCommitDraft: repoCommitMessage satisfies Loose<RepoCommitDraft>,
  repoPush: repoPush satisfies Loose<RepoPush>,

  // M8. Emitted by the API test harness, and split the way the M7 files are. The
  // launch, the record and the listing come from a *real git working copy* and a
  // real `git archive`, so the commit SHAs and the package sizes in them are git's
  // own answers — and the two experiment_*.json runs are from two different commits
  // on purpose, because that is what §5.11 item 7 is about. What is a double's: the
  // metrics, the Ray statuses and the log line, since neither a cluster nor a
  // tracking server can be had in a test.
  experimentLaunch: experimentLaunch satisfies Loose<ExperimentLaunch>,
  experiments: experimentList.experiments satisfies Loose<Experiment[]>,
  experiment: experiment satisfies Loose<Experiment>,
  experimentResults: experimentResults satisfies Loose<ExperimentSummary>,
  // The same type for a run that failed, which is the only one carrying D34's
  // `failure` block: the exception, its frames, and the message as it was raised —
  // this route serves it unmasked, and a model reads it masked below L2.
  experimentResultsFailed: experimentResultsFailed satisfies Loose<ExperimentSummary>,
  experimentLogs: experimentLogs satisfies Loose<ExperimentLogs>,

  // M9. Emitted by the API test harness through the *real* poller and the real
  // chat engine, so the injected summary, the proposal's fingerprint and the
  // decision record in here are the backend's own — only the model's wording is
  // scripted. The evaluation criteria come from a real evaluation.yaml at a real
  // commit, and the pair in experiment_results.json is deliberate: the primary
  // criterion is a bare `met: true` and the secondary one is the `not_computed`
  // object, so both arms of the verdict are checked rather than only whichever the
  // fixture happened to produce.
  experimentInterpretation: experimentInterpretation satisfies Loose<Interpretation>,
  experimentInterpretationDecided:
    experimentInterpretationDecided satisfies Loose<Interpretation>,
};
