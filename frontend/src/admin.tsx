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

import { useCallback, useEffect, useState } from "react";
import {
  TIERS,
  api,
  type Limits,
  type LimitsSurface,
  type Tier,
  type ToolCallRecord,
  type UsageRecord,
} from "./api";
import { Muted, Pane, Section, dateTime, describe, num, shortId } from "./ui";

/**
 * The admin surface of §3.3, behind the `admin` realm role.
 *
 * Two things it makes visible that a plain form would hide:
 *
 *   - which limits this build actually enforces. The kernel and Ray caps are §3.3
 *     fields whose enforcement points arrive with M4 and M7; storing them silently
 *     would let an administrator believe a resource cap was in force.
 *   - whether a cost cap can bind at all. A cost cap is computed from ODE's own
 *     price table, so a model with no configured price accrues zero cost and the
 *     cap quietly does not apply to it.
 */
export function AdminView() {
  const [surface, setSurface] = useState<LimitsSurface | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const reload = useCallback(async () => {
    try {
      setSurface(await api.adminLimits());
    } catch (e: unknown) {
      setError(describe(e));
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  const save = useCallback(
    async (subject: string, limits: Limits) => {
      setError(null);
      setNotice(null);
      try {
        await api.setAdminLimits(subject, limits);
        setNotice(subject ? `Saved limits for ${subject}.` : "Saved the global limits.");
        await reload();
      } catch (e: unknown) {
        setError(describe(e));
      }
    },
    [reload],
  );

  if (error && !surface) return <Muted>{error}</Muted>;
  if (!surface) return <Muted>Loading…</Muted>;

  const global = surface.limits.find((record) => record.subject === "");
  const perUser = surface.limits.filter((record) => record.subject !== "");

  return (
    <main className="panes admin-layout">
      <Pane title="LLM limits" subtitle="Applies to every developer unless overridden per user">
        {error && <Muted>{error}</Muted>}
        {notice && <p className="notice notice-info">{notice}</p>}

        <LimitsForm
          subject=""
          initial={global?.limits ?? surface.defaults}
          currency={surface.currency}
          enforced={surface.enforced}
          declared={surface.declared}
          onSave={save}
        />

        <Section title="Per-user overrides" note={`${perUser.length} configured`} defaultOpen={perUser.length > 0}>
          <Muted>
            A per-user record layers over the global one field by field. Two fields do not: the
            global spend cap is never a user's, and the maximum exposure tier takes the lower of
            the two — a per-user override cannot raise a deployment-wide ceiling.
          </Muted>
          {perUser.map((record) => (
            <Section
              key={record.subject}
              title={record.subject}
              note={`updated ${dateTime(record.updated_at)} by ${shortId(record.updated_by)}`}
              defaultOpen={false}
            >
              <LimitsForm
                subject={record.subject}
                initial={record.limits}
                currency={surface.currency}
                enforced={surface.enforced}
                declared={surface.declared}
                onSave={save}
              />
            </Section>
          ))}
          <AddOverride onSave={save} />
        </Section>

        <PricingTable surface={surface} />
      </Pane>

      <Pane title="Accounting" subtitle="Per Keycloak subject, one record per provider request">
        <UsageReport currency={surface.currency} />
      </Pane>

      <Pane title="Tool audit" subtitle="What the assistant reached for, refusals included">
        <ToolAudit />
      </Pane>
    </main>
  );
}

function LimitsForm({
  subject,
  initial,
  currency,
  enforced,
  declared,
  onSave,
}: {
  subject: string;
  initial: Limits;
  currency: string;
  enforced: string[];
  declared: Record<string, string>;
  onSave: (subject: string, limits: Limits) => void;
}) {
  const [draft, setDraft] = useState<Limits>(initial);
  const isEnforced = (field: string) => enforced.includes(field);

  const numberField = (
    field: keyof Limits,
    label: string,
    hint: string,
  ) => (
    <label className="limit-field" key={String(field)}>
      <span>
        {label}
        {!isEnforced(String(field)) && (
          <span className="badge" title={declared[String(field)]}>
            not yet enforced
          </span>
        )}
      </span>
      <input
        type="number"
        min={0}
        step="any"
        value={draft[field] === undefined ? "" : String(draft[field])}
        placeholder="no limit"
        onChange={(e) =>
          setDraft({
            ...draft,
            // An empty field means "inherit", not "zero". Sending 0 would forbid
            // everything, which is why the backend's fields are pointers.
            [field]: e.target.value === "" ? undefined : Number(e.target.value),
          })
        }
      />
      <span className="hint">{hint}</span>
    </label>
  );

  return (
    <form
      className="limits-form"
      onSubmit={(e) => {
        e.preventDefault();
        onSave(subject, draft);
      }}
    >
      <label className="limit-field">
        <span>Period</span>
        <input
          value={draft.period ?? ""}
          placeholder="720h"
          onChange={(e) => setDraft({ ...draft, period: e.target.value || undefined })}
        />
        <span className="hint">A Go duration. Caps apply over this window.</span>
      </label>

      {numberField("token_cap", "Token cap", "Hard cap on input + output tokens per period.")}
      {numberField("cost_cap", "Cost cap", `Hard cap in ${currency}, from the price table below.`)}
      {subject === "" &&
        numberField(
          "global_cost_cap",
          "Global cost cap",
          `Across every developer, in ${currency}. Only meaningful here.`,
        )}
      {numberField(
        "soft_warn_fraction",
        "Warn at",
        "Fraction of a cap where a warning is raised, e.g. 0.8. Nothing is blocked.",
      )}
      {numberField(
        "max_concurrent_sessions",
        "Concurrent sessions",
        "How many chat sessions one developer may have open.",
      )}

      <label className="limit-field">
        <span>Maximum exposure tier</span>
        <select
          value={draft.max_tier ?? ""}
          onChange={(e) =>
            setDraft({ ...draft, max_tier: (e.target.value || undefined) as Tier | undefined })
          }
        >
          <option value="">no limit (L2)</option>
          {TIERS.map((tier) => (
            <option key={tier} value={tier}>
              {tier}
            </option>
          ))}
        </select>
        <span className="hint">
          The highest tier a developer may raise a session to (§3.2). They always keep the ability
          to lower it.
        </span>
      </label>

      <Section title="Kernel and Ray caps" defaultOpen={false}>
        <Muted>
          Stored now so the schema and this form do not change when the enforcement points arrive.
          Nothing here binds in this build.
        </Muted>
        {(
          [
            ["kernel_cpu_default", "Kernel CPU default"],
            ["kernel_cpu_max", "Kernel CPU maximum"],
            ["kernel_mem_default", "Kernel memory default"],
            ["kernel_mem_max", "Kernel memory maximum"],
          ] as [keyof Limits, string][]
        ).map(([field, label]) => (
          <label className="limit-field" key={String(field)}>
            <span>
              {label}
              <span className="badge" title={declared[String(field)]}>
                not yet enforced
              </span>
            </span>
            <input
              value={(draft[field] as string | undefined) ?? ""}
              placeholder="unset"
              onChange={(e) => setDraft({ ...draft, [field]: e.target.value || undefined })}
            />
          </label>
        ))}
        {numberField("max_concurrent_ray_jobs", "Concurrent Ray jobs", "Arrives with M7.")}
      </Section>

      <div className="form-actions">
        <button type="submit">Save</button>
        <button type="button" onClick={() => setDraft(initial)}>
          Reset
        </button>
      </div>
    </form>
  );
}

function AddOverride({ onSave }: { onSave: (subject: string, limits: Limits) => void }) {
  const [subject, setSubject] = useState("");
  return (
    <form
      className="new-override"
      onSubmit={(e) => {
        e.preventDefault();
        if (subject.trim()) {
          // An empty policy is the right starting point: it inherits everything, and
          // the administrator then changes only what they mean to.
          onSave(subject.trim(), {});
          setSubject("");
        }
      }}
    >
      <input
        value={subject}
        onChange={(e) => setSubject(e.target.value)}
        placeholder="Keycloak subject (sub)"
        aria-label="Keycloak subject"
      />
      <button type="submit" disabled={!subject.trim()}>
        Add override
      </button>
    </form>
  );
}

/**
 * PricingTable is where a cost cap becomes checkable. ODE estimates cost from this
 * table because no provider returns one, so an unpriced model accrues nothing and
 * the cap silently does not apply to it.
 */
function PricingTable({ surface }: { surface: LimitsSurface }) {
  return (
    <Section
      title="Model prices"
      note={`${surface.pricing.length} configured, in ${surface.currency}`}
      defaultOpen={false}
    >
      {surface.pricing.length === 0 ? (
        <Muted>
          No prices are configured, so every request is accounted at zero cost and a cost cap
          cannot bind. Token caps still work. Set <code>llm_pricing</code> in the configuration.
        </Muted>
      ) : (
        <table className="grid">
          <thead>
            <tr>
              <th>Model or prefix</th>
              <th>Input / MTok</th>
              <th>Output / MTok</th>
              <th>Cached input</th>
            </tr>
          </thead>
          <tbody>
            {surface.pricing.map((price) => (
              <tr key={price.model}>
                <td>{price.model}</td>
                <td>{price.input_per_mtok}</td>
                <td>{price.output_per_mtok}</td>
                <td>{price.cached_input_per_mtok || <span className="muted">as input</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <Muted>
        A price matches a model exactly, or as the longest matching prefix — so one entry can
        cover a family and a specific entry still wins.
      </Muted>
    </Section>
  );
}

function UsageReport({ currency }: { currency: string }) {
  const [subject, setSubject] = useState("");
  const [period, setPeriod] = useState("720h");
  const [data, setData] = useState<{
    usage: UsageRecord[];
    spend: { tokens: number; cost: number; requests: number };
  } | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    try {
      const result = await api.adminUsage({ sub: subject || undefined, period, limit: 200 });
      setData({ usage: result.usage, spend: result.spend });
    } catch (e: unknown) {
      setError(describe(e));
    }
  }, [period, subject]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <>
      <form
        className="search"
        onSubmit={(e) => {
          e.preventDefault();
          void load();
        }}
      >
        <input
          value={subject}
          onChange={(e) => setSubject(e.target.value)}
          placeholder="All developers"
          aria-label="Keycloak subject"
        />
        <input
          value={period}
          onChange={(e) => setPeriod(e.target.value)}
          placeholder="720h"
          aria-label="Period"
        />
        <button type="submit">Load</button>
      </form>

      {error && <Muted>{error}</Muted>}
      {!data && <Muted>Loading…</Muted>}
      {data && (
        <>
          <dl className="kv">
            <dt>Requests</dt>
            <dd>{num(data.spend.requests)}</dd>
            <dt>Tokens</dt>
            <dd>{num(data.spend.tokens)}</dd>
            <dt>Estimated cost</dt>
            <dd>
              {data.spend.cost.toFixed(4)} {currency}
            </dd>
          </dl>
          {data.usage.length === 0 ? (
            <Muted>No requests in this period.</Muted>
          ) : (
            <table className="grid">
              <thead>
                <tr>
                  <th>When</th>
                  <th>Developer</th>
                  <th>Model</th>
                  <th>Tokens</th>
                  <th>Cost</th>
                </tr>
              </thead>
              <tbody>
                {data.usage.map((record, index) => (
                  <tr key={index}>
                    <td>{dateTime(record.at)}</td>
                    <td title={record.user_sub}>{shortId(record.user_sub)}</td>
                    <td>
                      {record.provider} / {record.model}
                    </td>
                    <td>
                      {num(record.input_tokens + record.output_tokens)}
                      {record.cached_input_tokens ? (
                        <span className="muted" title="Cached input, priced separately">
                          {" "}
                          +{num(record.cached_input_tokens)} cached
                        </span>
                      ) : null}
                    </td>
                    <td>
                      {record.cost_estimated ? (
                        record.cost.toFixed(4)
                      ) : (
                        <span className="muted" title="This model has no configured price, so a cost cap cannot see it">
                          unpriced
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </>
  );
}

/**
 * ToolAudit is the record that makes the tier argument checkable after the fact:
 * which tools the assistant reached for, at which tier, and which were refused.
 */
function ToolAudit() {
  const [calls, setCalls] = useState<ToolCallRecord[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .adminToolCalls({ period: "720h", limit: 200 })
      .then((result) => {
        if (!cancelled) setCalls(result.tool_calls);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(describe(e));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (error) return <Muted>{error}</Muted>;
  if (!calls) return <Muted>Loading…</Muted>;
  if (calls.length === 0) return <Muted>No tool calls recorded.</Muted>;

  const refused = calls.filter((call) => call.outcome !== "ok").length;

  return (
    <>
      <Muted>
        {num(calls.length)} calls, {num(refused)} refused or failed.
      </Muted>
      <table className="grid">
        <thead>
          <tr>
            <th>When</th>
            <th>Developer</th>
            <th>Tool</th>
            <th>Tier</th>
            <th>Outcome</th>
          </tr>
        </thead>
        <tbody>
          {calls.map((call, index) => (
            <tr key={index}>
              <td>{dateTime(call.at)}</td>
              <td title={call.user_sub}>{shortId(call.user_sub)}</td>
              <td>{call.tool}</td>
              <td>
                <span className={`tier tier-${call.tier}`}>{call.tier}</span>
              </td>
              <td>
                <span className={`tool-outcome outcome-${call.outcome}`}>{call.outcome}</span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
