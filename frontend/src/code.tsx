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

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ApiError,
  api,
  type GitHubRepository,
  type RepoConnection,
  type RepoFile,
  type RepoNode,
  type RepoStatus,
  type Session,
} from "./api";
import { monaco, monacoLanguage } from "./monaco";
import { setParam, useParam } from "./router";
import { Busy, Muted, Pane, bytes, dateTime, describe, shortId } from "./ui";
import { WorkbenchBar } from "./workbench";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";

/**
 * The Code view (§5.11).
 *
 * Four states, in the order a developer meets them: connect GitHub, pick or create
 * a repository, then the working copy — a full file tree with Monaco beside it,
 * over a bar carrying the repository's state and the git actions.
 *
 * Three things on screen are load-bearing rather than decorative.
 *
 *   - **Where the checkout is.** The path on the per-user PVC is reachable, because
 *     "somewhere in your pod" is not something a developer can act on, and because
 *     it is the same directory their kernel runs in.
 *
 *   - **What is uncommitted, always.** §5.11 item 6: work found on reopen is
 *     surfaced with the three answers beside it — commit, stash, discard — and
 *     never silently reset. The bar carries the count at all times and opens the
 *     three answers itself on reopen. Discard asks again before it runs.
 *
 *   - **That saving is not committing.** The editor's save writes the working
 *     copy. Commit and push are separate buttons, because they are separate
 *     decisions (§5.11 item 5).
 */
export function CodeView({ session }: { session: Session }) {
  const [connection, setConnection] = useState<RepoConnection | null>(null);
  const [status, setStatus] = useState<RepoStatus | null>(null);
  const [needs, setNeeds] = useState<"github_connection" | "repository" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(true);

  // One loader for both, because the answer to "what should this pane show" is the
  // pair: a connection without a repository is the picker, and a repository always
  // implies a connection.
  const reload = useCallback(async (fetchRemote = false) => {
    setBusy(true);
    setError(null);
    try {
      const current = await api.repoConnection();
      setConnection(current);
      if (!current.connected) {
        setStatus(null);
        setNeeds("github_connection");
        return;
      }
      try {
        setStatus(await api.repoStatus(fetchRemote));
        setNeeds(null);
      } catch (e: unknown) {
        if (e instanceof ApiError && e.status === 409) {
          setStatus(null);
          setNeeds("repository");
          return;
        }
        throw e;
      }
    } catch (e: unknown) {
      setError(describe(e));
    } finally {
      setBusy(false);
    }
  }, []);

  // Which workbench the panes are acting in. Read here so the pane reloads when it
  // changes — the bar below switches it, and so does opening a chat session that
  // belongs to another operator.
  const workbench = useParam("workbench");

  useEffect(() => {
    // A fetch on open, which is where §5.11 item 5's "report divergence" belongs:
    // the developer is looking at the pane for the first time this session.
    //
    // "This session" has to be enforced rather than assumed. The pane used to be a
    // tab nobody opened twice, so mounting it and opening it were the same event;
    // it is now half of the landing route, so it mounts on every reload — and an
    // unconditional fetch here would put a GitHub round trip in front of every page
    // load, for a divergence the developer has already been shown. Once per browser
    // session, and the Fetch button is there for the rest.
    void reload(!fetchedThisSession());
  }, [reload, workbench]);

  if (busy && !connection) return <Pane title="Code" subtitle="Loading…"><Busy>Loading…</Busy></Pane>;

  return (
    <main className="panes code">
      {/*
        Which operator this pane is showing, and the others that are open. Above
        everything else here, including the connect card: switching workbenches is
        what the rest of the pane is about, and burying it under a repository
        picker would make a second operator look like a second repository.
      */}
      <WorkbenchBar onSwitched={() => void reload()} />
      {error && (
        <Pane title="Code" subtitle="The operator repository">
          <p className="error text-destructive">{error}</p>
          <Button variant="outline" onClick={() => void reload()}>Try again</Button>
        </Pane>
      )}
      {!error && needs === "github_connection" && (
        <ConnectPane session={session} connection={connection} />
      )}
      {!error && needs === "repository" && connection?.connected && (
        <>
          <RepositoryPicker onSelected={() => void reload(true)} />
          <ConnectedPane connection={connection} onDisconnected={() => void reload()} />
        </>
      )}
      {!error && status && (
        <WorkingCopy
          status={status}
          connection={connection}
          onStatus={setStatus}
          onReload={reload}
        />
      )}
    </main>
  );
}

/** The connect card: what ODE will ask GitHub for, and why. */
function ConnectPane({
  session,
  connection,
}: {
  session: Session;
  connection: RepoConnection | null;
}) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const scopes = connection?.scopes_requested ?? session.repo?.scopes ?? ["repo", "workflow"];

  const connect = async () => {
    setPending(true);
    setError(null);
    try {
      const authorize = await api.repoAuthorize();
      // The state is remembered here rather than sent through GitHub alone: the
      // backend checks it too, and keeping a copy lets the callback report a
      // mismatch before it spends a round trip.
      sessionStorage.setItem("ode.github.state", authorize.state);
      window.location.assign(authorize.url);
    } catch (e: unknown) {
      setError(describe(e));
      setPending(false);
    }
  };

  return (
    <Pane title="GitHub" subtitle="The operator lives in a repository of yours">
      <p>
        ODE clones the repository into your own workspace, writes files there when you
        or the assistant ask it to, and commits and pushes only when you say so.
      </p>
      <p className="muted text-muted-foreground">
        The token is stored encrypted and separately from your platform session. It asks
        for{" "}
        {scopes.map((scope, index) => (
          <span key={scope}>
            {index > 0 && ", "}
            <code>{scope}</code>
          </span>
        ))}
        : the first to read and write the repository, the second because the build
        workflow is a file in it and GitHub refuses a push that touches one without it.
      </p>
      {error && <p className="error text-destructive">{error}</p>}
      <Button variant="default"
        className={pending ? "primary busy animate-pulse" : "primary"}
        onClick={() => void connect()}
        disabled={pending}
      >
        {pending ? "Opening GitHub…" : "Connect GitHub"}
      </Button>
    </Pane>
  );
}

/** The connected account, and the way out of it. */
function ConnectedPane({
  connection,
  onDisconnected,
}: {
  connection: RepoConnection;
  onDisconnected: () => void;
}) {
  const identity = connection.identity;
  const [pending, setPending] = useState(false);
  if (!identity) return null;

  const disconnect = async () => {
    setPending(true);
    try {
      await api.repoDisconnect();
      onDisconnected();
    } finally {
      setPending(false);
    }
  };

  return (
    <Pane
      title="GitHub account"
      subtitle="Stored encrypted, separately from your platform session"
      className="account"
    >
      <dl className="kv">
        <dt>Account</dt>
        <dd>{identity.login}</dd>
        <dt>Connected</dt>
        <dd>{dateTime(identity.connected_at)}</dd>
        <dt>Scopes</dt>
        <dd>{(identity.scopes ?? []).join(", ") || "—"}</dd>
      </dl>
      {identity.missing_scopes && identity.missing_scopes.length > 0 && (
        <p className="warn text-foreground">
          The grant is missing {identity.missing_scopes.join(", ")}. A push that touches{" "}
          <code>.github/workflows/</code> will be rejected until you reconnect and allow it.
        </p>
      )}
      <p className="muted text-muted-foreground">
        Disconnecting forgets the token. Your working copy stays where it is — it is on
        your own storage, and ODE does not delete your work.
      </p>
      <Button variant="outline" onClick={() => void disconnect()} disabled={pending}>
        Disconnect
      </Button>
    </Pane>
  );
}

/** Pick an existing repository, or create one and have it scaffolded. */
function RepositoryPicker({ onSelected }: { onSelected: () => void }) {
  const [repositories, setRepositories] = useState<GitHubRepository[] | null>(null);
  const [filter, setFilter] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState<string | null>(null);

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [isPrivate, setPrivate] = useState(false);

  useEffect(() => {
    let live = true;
    api
      .repoRepositories()
      .then((result) => {
        if (live) setRepositories(result.repositories);
      })
      .catch((e: unknown) => {
        if (live) setError(describe(e));
      });
    return () => {
      live = false;
    };
  }, []);

  const select = async (fullName: string) => {
    setPending(fullName);
    setError(null);
    try {
      await api.repoSelect(fullName);
      onSelected();
    } catch (e: unknown) {
      setError(describe(e));
    } finally {
      setPending(null);
    }
  };

  const create = async () => {
    setPending("create");
    setError(null);
    try {
      await api.repoCreate({ name, description, private: isPrivate, scaffold: true });
      onSelected();
    } catch (e: unknown) {
      setError(describe(e));
    } finally {
      setPending(null);
    }
  };

  const shown = (repositories ?? []).filter((repository) =>
    repository.full_name.toLowerCase().includes(filter.toLowerCase()),
  );

  return (
    <Pane title="Repository" subtitle="Work on one of yours, or create one from the operator template">
      <form
        className="repo-create"
        onSubmit={(event) => {
          event.preventDefault();
          if (name.trim()) void create();
        }}
      >
        <Input
          placeholder="new-operator-name"
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
        <Input
          placeholder="What it does (optional)"
          value={description}
          onChange={(event) => setDescription(event.target.value)}
        />
        <label className="checkbox flex items-center gap-2 text-sm">
          <Checkbox checked={isPrivate} onCheckedChange={(checked) => setPrivate(checked)} />
          Private
        </label>
        <Button variant="default"
          className={pending === "create" ? "primary busy animate-pulse" : "primary"}
          type="submit"
          disabled={!name.trim() || pending === "create"}
        >
          {pending === "create" ? "Creating…" : "Create and scaffold"}
        </Button>
      </form>
      <p className="muted text-muted-foreground">
        A created repository starts empty and the template is written into your working
        copy — the first commit is yours to make and review.
      </p>

      {error && <p className="error text-destructive">{error}</p>}

      <div className="repo-filter">
        <Input
          placeholder="Filter your repositories"
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
        />
      </div>
      {!repositories && <Busy>Reading your repositories…</Busy>}
      {repositories && shown.length === 0 && <Muted>No repository matches.</Muted>}
      <ul className="repo-list">
        {shown.map((repository) => (
          <li key={repository.full_name}>
            <div>
              <span className="repo-name">{repository.full_name}</span>
              {repository.private && <span className="badge inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">private</span>}
              {repository.empty && <span className="badge inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">empty</span>}
              {!repository.can_push && <span className="badge warn inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs text-foreground">read-only</span>}
              {repository.description && <p className="muted text-muted-foreground">{repository.description}</p>}
            </div>
            <Button variant="outline"
              className={pending === repository.full_name ? "busy animate-pulse" : undefined}
              onClick={() => void select(repository.full_name)}
              disabled={pending === repository.full_name}
            >
              {pending === repository.full_name ? "Cloning…" : "Work on this"}
            </Button>
          </li>
        ))}
      </ul>
    </Pane>
  );
}

/** The working copy: the tree and the editor, with the repository's state under them. */
function WorkingCopy({
  status,
  connection,
  onStatus,
  onReload,
}: {
  status: RepoStatus;
  connection: RepoConnection | null;
  onStatus: (status: RepoStatus) => void;
  onReload: (fetchRemote?: boolean) => Promise<void>;
}) {
  // The tree is re-read whenever something moved the working copy under it — a
  // stash, a discard, the scaffold writing files. The counter lives here rather
  // than in the bar because the two are siblings: the part that changes the files
  // is not the part that lists them.
  const [treeVersion, setTreeVersion] = useState(0);

  return (
    <>
      <FilesPane status={status} version={treeVersion} onChanged={() => void onReload()} />
      <RepoBar
        status={status}
        connection={connection}
        onStatus={onStatus}
        onReload={onReload}
        onChanged={() => setTreeVersion((version) => version + 1)}
      />
    </>
  );
}

/** Which of the bar's panels is open. One at a time — see the note on the bar. */
type Panel = "repository" | "changes" | "warnings";

/**
 * The repository's state as a bar under the workspace, with the detail in panels
 * over it.
 *
 * This was a pane beside the file tree, and in the split it was wrong in the one
 * dimension that mattered. The code view is two columns of the right-hand half, so
 * the state got about a third of it — enough for the actions row to squeeze the
 * repository's name into eleven characters and wrap it letter by letter — while the
 * editor, the thing the developer is actually reading, paid for the rest.
 *
 * What a bar can carry and what it cannot is the whole of the design. The branch,
 * how far the checkout is from the remote, how many files are uncommitted and how
 * many warnings there are: facts of a few characters each, and they belong on
 * screen at all times. The four warning paragraphs, the change list and the commit
 * message field are not that. They are read while something is being decided, and a
 * bar that tried to hold them would be the pane again.
 *
 * So the counts stay in the bar and open a panel above it. One panel at a time —
 * two panels over the editor would be two answers to one question — and a panel
 * closes on Escape and on a click outside it, which is the pattern the "Under the
 * hood" menu already uses in the shell.
 *
 * §5.11 item 6 is why the changes panel opens itself. Work found on reopen has to
 * be surfaced with the three answers beside it — commit, stash, discard — and a
 * count in a bar is a surfacing but not that. Once per browser session, the same
 * reading of "reopen" the fetch on mount takes; after that the panel is the
 * developer's, because one that reopened on every status change would sit over the
 * editor for a whole working afternoon.
 */
function RepoBar({
  status,
  connection,
  onStatus,
  onReload,
  onChanged,
}: {
  status: RepoStatus;
  connection: RepoConnection | null;
  onStatus: (status: RepoStatus) => void;
  onReload: (fetchRemote?: boolean) => Promise<void>;
  onChanged: () => void;
}) {
  const [message, setMessage] = useState("");
  const [pending, setPending] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [panel, setPanel] = useState<Panel | null>(null);
  const bar = useRef<HTMLDivElement | null>(null);
  const triggers = useRef<Partial<Record<Panel, HTMLButtonElement | null>>>({});

  const act = async (label: string, run: () => Promise<RepoStatus | void>) => {
    setPending(label);
    setError(null);
    setNote(null);
    try {
      const next = await run();
      if (next) onStatus(next);
      else await onReload();
      onChanged();
    } catch (e: unknown) {
      setError(describe(e));
    } finally {
      setPending(null);
    }
  };

  const commit = () =>
    act("commit", async () => {
      const committed = await api.repoCommit(message);
      setMessage("");
      setNote(
        `Committed ${shortId(committed.sha)} on ${committed.branch}: ${committed.files} file(s). Nothing is pushed yet.`,
      );
      onStatus(await api.repoStatus());
    });

  const push = () =>
    act("push", async () => {
      const pushed = await api.repoPush();
      setNote(pushed.output || `Pushed ${pushed.branch} to ${pushed.remote}.`);
      onStatus(await api.repoStatus(true));
    });

  /*
   * The two actions the backend refuses when this checkout's origin is not the
   * linked repository — 409, `needs: "remote_match"`.
   *
   * Disabled rather than left to fail on the click. The refusal is decided by a
   * fact ODE is already holding and already showing in the bar, so letting the
   * button stay live makes the developer write a commit message and press it in
   * order to be told something the screen knew before they started. Worse for push,
   * where the thing being confirmed is whether their work is about to land in a
   * repository they did not choose.
   *
   * Stash and discard stay live, and so does writing the scaffold: none of them
   * leave the working copy, and a checkout pointing at the wrong repository is
   * exactly the situation where setting the changes aside is the useful move.
   */
  const remoteMismatch = status.remote_mismatch === true;

  const discard = () =>
    act("discard", async () => {
      // The one destructive action in the bar, and the second of the two
      // confirmations it needs — the backend requires the flag as well.
      if (
        !window.confirm(
          "Discard every uncommitted change in the working copy? Untracked files are removed too. This cannot be undone.",
        )
      ) {
        return undefined;
      }
      return api.repoDiscard();
    });

  /*
   * Everything ODE will refuse, or has already refused, and why.
   *
   * Collected into one list rather than rendered where each is computed, because
   * the bar shows the *count* and the panel shows the paragraphs: both need the
   * same set, and deriving it twice is how the two come to disagree.
   */
  const missingScopes = connection?.identity?.missing_scopes ?? [];
  const warnings: React.ReactNode[] = [];
  if (remoteMismatch) {
    warnings.push(
      <p className="warn text-foreground" key="remote">
        This checkout&apos;s origin is <code>{status.remote}</code>, which is not the repository
        ODE has linked. Commit and push are disabled: git would do both quite happily, and the
        work would end up in a repository you did not choose. Nothing has been changed — select
        the repository this checkout actually points at, or move the directory aside and let ODE
        clone the linked one. Stash and discard still work, because they stay in the working
        copy.
      </p>,
    );
  }
  if (!status.scaffold.complete) {
    warnings.push(
      <p className="warn text-foreground" key="scaffold">
        Missing from the operator template: {status.scaffold.missing.join(", ")}.{" "}
        <Button variant="link"
          className="link"
          onClick={() =>
            void act("scaffold", async () => {
              const result = await api.repoScaffold();
              setNote(
                result.written.length > 0
                  ? `Wrote ${result.written.join(", ")}. ${result.hint}`
                  : "Everything was already there.",
              );
              onStatus(await api.repoStatus());
            })
          }
          disabled={pending !== null}
        >
          Write the missing files
        </Button>
      </p>,
    );
  }
  if (status.diverged) {
    warnings.push(
      <p className="warn text-foreground" key="diverged">
        The branch has moved on both sides. ODE does not merge for you: pull in a terminal in
        your own pod, or push a branch of your own.
      </p>,
    );
  }
  if (missingScopes.length > 0) {
    warnings.push(
      <p className="warn text-foreground" key="scopes">
        The GitHub grant is missing {missingScopes.join(", ")}, so a push that touches the build
        workflow will be rejected.
      </p>,
    );
  }

  const closePanel = (restoreFocus: boolean) => {
    if (restoreFocus && panel) triggers.current[panel]?.focus();
    setPanel(null);
  };

  // A panel that survives the click that landed outside it would sit over the file
  // the developer just picked in the tree.
  useEffect(() => {
    if (!panel) return;
    const onPointerDown = (event: PointerEvent) => {
      if (bar.current && !bar.current.contains(event.target as Node)) setPanel(null);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [panel]);

  // Writing the scaffold empties the list the panel is showing. Closing it is the
  // honest end of that: an open panel with nothing in it reads as a fault.
  useEffect(() => {
    if (panel === "warnings" && warnings.length === 0) setPanel(null);
  }, [panel, warnings.length]);

  // Mount only, and once per browser session: see the note above on §5.11 item 6.
  useEffect(() => {
    if (status.dirty && !dirtyShownThisSession()) setPanel("changes");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  /** A segment that opens a panel. The caret says which way the panel goes. */
  const trigger = (
    which: Panel,
    label: React.ReactNode,
    modifier: string | undefined,
    title: string,
  ) => (
    <Button variant="outline"
      className={modifier ? `repo-bar-item ${modifier}` : "repo-bar-item"}
      ref={(element) => {
        triggers.current[which] = element;
      }}
      aria-expanded={panel === which}
      aria-controls={`repo-panel-${which}`}
      title={title}
      onClick={() => setPanel(panel === which ? null : which)}
    >
      {label}
      <span className="repo-bar-caret" aria-hidden="true">
        ▴
      </span>
    </Button>
  );

  return (
    <div
      className="repo-bar"
      ref={bar}
      onKeyDown={(event) => {
        if (event.key === "Escape" && panel) {
          event.preventDefault();
          closePanel(true);
        }
      }}
    >
      {/*
        An action's answer — a commit's sha, a push's output, a refusal — stays in
        the bar rather than in a panel. It is caused from a panel, and a panel is
        closed by the next click; the answer to "did that push" must not be closed
        with it. Dismissible rather than timed: the developer decides when they have
        read it.
      */}
      {(error ?? note) !== null && (
        <p className={error ? "repo-bar-notice error text-destructive" : "repo-bar-notice note"}>
          <span>{error ?? note}</span>
          <Button variant="outline"
            onClick={() => {
              setError(null);
              setNote(null);
            }}
          >
            Dismiss
          </Button>
        </p>
      )}

      <div className="repo-bar-row">
        <div className="repo-bar-group">
          {trigger(
            "repository",
            <span className="repo-bar-name">{status.link.full_name}</span>,
            undefined,
            `${status.workspace}/${status.link.path} on your own storage`,
          )}

          <span className="repo-bar-fact" title="The branch the working copy is on">
            {status.branch ?? "—"}
            {status.unborn && <span className="badge inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">no commits yet</span>}
            {status.detached && <span className="badge warn inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs text-foreground">detached</span>}
          </span>

          {/*
            "in sync" rather than "0 ahead, 0 behind": the pair of zeroes is four
            words to say nothing happened, and the bar's room is worth more than
            that. `unfetched` stays in front of it either way, because a stale zero
            is not agreement — the same reason the status route makes `fetched`
            explicit.
          */}
          <span
            className="repo-bar-fact"
            title={
              status.fetched
                ? "Against the remote, as of this fetch"
                : "Against the remote as ODE last saw it. Press Fetch to make it current."
            }
          >
            {!status.fetched && "unfetched · "}
            {status.ahead === 0 && status.behind === 0
              ? "in sync"
              : `${status.ahead} ahead, ${status.behind} behind`}
            {status.diverged && <span className="badge warn inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs text-foreground">diverged</span>}
          </span>
        </div>

        <div className="repo-bar-group">
          {warnings.length > 0 &&
            trigger(
              "warnings",
              <>
                <span aria-hidden="true">⚠ </span>
                {warnings.length === 1 ? "1 warning" : `${warnings.length} warnings`}
              </>,
              "warn",
              "What ODE will refuse, or has already refused, and why",
            )}

          {trigger(
            "changes",
            status.dirty ? `${status.changes.length} uncommitted` : "no changes",
            status.dirty ? "dirty" : undefined,
            status.dirty
              ? "Uncommitted work in the working copy: commit, stash or discard it"
              : "The working copy matches the last commit",
          )}

          <Button variant="outline" onClick={() => void onReload(true)} disabled={pending !== null}>
            Fetch
          </Button>
          <Button variant="outline"
            className={pending === "push" ? "busy animate-pulse" : undefined}
            onClick={() => void push()}
            disabled={pending !== null || status.unborn || remoteMismatch}
            title={
              remoteMismatch
                ? "Push is refused while this checkout points at another repository — see the warning."
                : status.unborn
                  ? "There is nothing to push until the first commit."
                  : "Pushing is what triggers the build workflow, which pushes the image to ghcr.io."
            }
          >
            {pending === "push" ? "Pushing…" : `Push${status.ahead ? ` (${status.ahead})` : ""}`}
          </Button>
        </div>
      </div>

      {panel === "repository" && (
        <div
          className="repo-pop start"
          id="repo-panel-repository"
          role="group"
          aria-label="Repository"
        >
          <dl className="kv">
            <dt>Working copy</dt>
            <dd>
              <code>
                {status.workspace}/{status.link.path}
              </code>
            </dd>
            <dt>Head</dt>
            <dd>
              {status.head ? (
                <>
                  <code>{shortId(status.head)}</code> {status.head_subject}
                  {status.head_date && (
                    <span className="muted text-muted-foreground"> · {dateTime(status.head_date)}</span>
                  )}
                </>
              ) : (
                "—"
              )}
            </dd>
            <dt>Remote</dt>
            <dd>{status.remote ?? "—"}</dd>
            {status.link.operator_lib_ref && (
              <>
                <dt>Operator Lib</dt>
                <dd>
                  pinned at <code>{status.link.operator_lib_ref}</code>
                </dd>
              </>
            )}
          </dl>
          <div className="repo-pop-actions">
            <a href={status.link.html_url} target="_blank" rel="noreferrer">
              Open on GitHub
            </a>
            <Button variant="outline"
              onClick={() =>
                void act("unlink", async () => {
                  await api.repoUnlink();
                })
              }
              className={pending === "unlink" ? "busy animate-pulse" : undefined}
              disabled={pending !== null}
            >
              {pending === "unlink" ? "Switching…" : "Switch repository"}
            </Button>
          </div>
        </div>
      )}

      {panel === "warnings" && warnings.length > 0 && (
        <div className="repo-pop end" id="repo-panel-warnings" role="group" aria-label="Warnings">
          {warnings}
        </div>
      )}

      {panel === "changes" && (
        <div
          className="repo-pop end"
          id="repo-panel-changes"
          role="group"
          aria-label="Uncommitted changes"
        >
          {status.dirty ? (
            <>
              <ul className="changes">
                {status.changes.map((change) => (
                  <li key={change.path}>
                    <span className={`change ${change.kind}`}>{change.kind}</span>
                    <code>{change.path}</code>
                    {change.renamed_from && (
                      <span className="muted text-muted-foreground"> from {change.renamed_from}</span>
                    )}
                    {change.staged && <span className="badge inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">staged</span>}
                  </li>
                ))}
              </ul>
              <div className="commit-box">
                <Input
                  placeholder="What this change does"
                  value={message}
                  onChange={(event) => setMessage(event.target.value)}
                />
                <Button variant="default"
                  className={pending === "commit" ? "primary busy animate-pulse" : "primary"}
                  onClick={() => void commit()}
                  disabled={!message.trim() || pending !== null || remoteMismatch}
                  title={
                    remoteMismatch
                      ? "The checkout points at a different repository than the linked one."
                      : undefined
                  }
                >
                  {pending === "commit" ? "Committing…" : "Commit"}
                </Button>
                <Button variant="outline"
                  onClick={() =>
                    void act("stash", async () => {
                      const next = await api.repoStash();
                      setNote("Stashed. Recover it with `git stash pop` in your pod.");
                      return next;
                    })
                  }
                  disabled={pending !== null}
                >
                  Stash
                </Button>
                <Button variant="outline"
                  className="danger"
                  onClick={() => void discard()}
                  disabled={pending !== null}
                >
                  Discard
                </Button>
              </div>
              <p className="muted text-muted-foreground">
                Saving a file writes the working copy; committing is a separate decision, and so
                is pushing — which is what triggers the build workflow.
              </p>
            </>
          ) : (
            <Muted>The working copy matches the last commit.</Muted>
          )}
        </div>
      )}
    </div>
  );
}

/** The file tree and the editor: read and write on every file (D14). */
function FilesPane({
  status,
  version,
  onChanged,
}: {
  status: RepoStatus;
  version: number;
  onChanged: () => void;
}) {
  const [tree, setTree] = useState<RepoNode | null>(null);
  const [treeError, setTreeError] = useState<string | null>(null);
  // The open file is the URL's rather than this pane's. It is the state a
  // developer would most resent losing on a reload — the editor is the half of the
  // workspace they came for — and restoring it is cheap: one read of the working
  // copy on their own pod, no platform call at all.
  const selected = useParam("file");
  // Which checkout this is a tree of. The pane does not switch workbenches, but it
  // has to re-read when the bar above it does: two workbenches are two repositories,
  // so a tree left standing across a switch lists files that are not in the working
  // copy on screen, and clicking one asks for a path that does not exist.
  const workbench = useParam("workbench");
  const [file, setFile] = useState<RepoFile | null>(null);
  const [draft, setDraft] = useState("");
  const [fileError, setFileError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const root = status.link.path;

  const loadTree = useCallback(async () => {
    try {
      const result = await api.repoFiles();
      setTree(result.tree);
      setTreeError(null);
    } catch (e: unknown) {
      setTreeError(describe(e));
    }
  }, []);

  // Which workbench the tree on screen was read from, so a switch can be told from
  // a re-read of the same one.
  const listed = useRef(workbench);

  useEffect(() => {
    if (listed.current !== workbench) {
      listed.current = workbench;
      // Emptied rather than left standing until the new one answers: those rows are
      // clickable, and clicking one would open a path from the other checkout. A
      // `version` bump is not a switch — a stash or a discard changes this same tree,
      // and blanking it there would make the pane flicker on every git action.
      setTree(null);
    }
    void loadTree();
  }, [loadTree, version, workbench]);

  // Opening a file is now only a URL write; the read below follows from it. That
  // way clicking the tree, restoring a reload and following a link all take the
  // same path through the code, instead of the first one being special.
  const open = useCallback((path: string) => setParam("file", path), []);

  useEffect(() => {
    if (!selected) {
      setFile(null);
      setDraft("");
      setFileError(null);
      return;
    }
    let cancelled = false;
    setFileError(null);
    api
      .repoFile(selected)
      .then((loaded) => {
        if (cancelled) return;
        setFile(loaded);
        setDraft(loaded.text);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        // A path in the address bar may name a file that has since been deleted or
        // was never there. The error says which, rather than an empty editor.
        setFile(null);
        setFileError(describe(e));
      });
    return () => {
      cancelled = true;
    };
    // The workbench is a dependency and not only a hint: a switch normally drops the
    // open file, but the back button can land on an address that names the same path
    // in the other checkout, and that is a different file.
  }, [selected, workbench]);

  const save = useCallback(async () => {
    if (!file || file.binary) return;
    setSaving(true);
    setFileError(null);
    try {
      await api.repoWriteFile(file.path, draft);
      setFile({ ...file, text: draft });
      onChanged();
    } catch (e: unknown) {
      setFileError(describe(e));
    } finally {
      setSaving(false);
    }
  }, [draft, file, onChanged]);

  const remove = async (path: string, directory: boolean) => {
    if (!window.confirm(`Delete ${path} from the working copy?`)) return;
    try {
      await api.repoDeleteFile(path, directory);
      if (selected === path) setParam("file", null);
      await loadTree();
      onChanged();
    } catch (e: unknown) {
      setFileError(describe(e));
    }
  };

  const create = async () => {
    const path = window.prompt("New file, relative to the repository root");
    if (!path) return;
    try {
      await api.repoWriteFile(path, "");
      await loadTree();
      open(path);
      onChanged();
    } catch (e: unknown) {
      setFileError(describe(e));
    }
  };

  const dirty = file !== null && !file.binary && draft !== file.text;

  return (
    <Pane
      title="Files"
      subtitle="Every file of the repository, editable — nothing hidden, nothing reserved"
      actions={
        <>
          <Button variant="outline" onClick={() => void create()}>New file</Button>
          <Button variant="outline" onClick={() => void loadTree()}>Refresh</Button>
        </>
      }
    >
      <div className="code-layout">
        <div className="file-tree">
          {treeError && <p className="error text-destructive">{treeError}</p>}
          {!tree && !treeError && <Busy>Reading the working copy…</Busy>}
          {tree && (
            <ul className="tree">
              {(tree.children ?? []).map((node) => (
                <TreeNode
                  key={node.path}
                  node={node}
                  root={root}
                  selected={selected}
                  onOpen={open}
                  onDelete={(path, directory) => void remove(path, directory)}
                />
              ))}
            </ul>
          )}
        </div>

        <div className="file-editor">
          {!file && !fileError && <Muted>Pick a file to edit it.</Muted>}
          {fileError && <p className="error text-destructive">{fileError}</p>}
          {file && (
            <>
              {/*
                The head is a toolbar: what the file is on the left, what can be
                done to it on the right. In one wrapping row the Save button sat
                between the file's size and a sentence about committing, which
                read as prose with a button in the middle of it — and the sentence,
                being the longest item, decided where the button landed. So the
                sentence is its own line below, where it explains the button
                without moving it.
              */}
              <div className="file-head">
                <code className="file-path">{file.path}</code>
                <span className="muted file-meta text-muted-foreground">
                  {bytes(file.size)}
                  {file.modified ? ` · ${dateTime(file.modified)}` : ""}
                </span>
                {dirty && <span className="badge warn inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs text-foreground">unsaved</span>}
                <Button variant="default"
                  className={saving ? "primary file-save busy animate-pulse" : "primary file-save"}
                  onClick={() => void save()}
                  disabled={!dirty || saving || file.binary}
                >
                  {saving ? "Saving…" : "Save"}
                </Button>
              </div>
              <p className="muted file-hint text-muted-foreground">
                Saving writes the working copy. It does not commit.
              </p>
              {file.binary && (
                <Muted>
                  This file is not text, so it is not shown. Editing it here would corrupt it.
                </Muted>
              )}
              {file.truncated && (
                <p className="warn text-foreground">
                  The file is longer than the editor limit and is shown truncated. Saving it
                  would write only what is shown, so it is read-only here.
                </p>
              )}
              {!file.binary && !file.truncated && (
                <Editor
                  path={file.path}
                  language={monacoLanguage(file.language, file.path)}
                  value={draft}
                  onChange={setDraft}
                  onSave={() => void save()}
                />
              )}
            </>
          )}
        </div>
      </div>
    </Pane>
  );
}

function TreeNode({
  node,
  root,
  selected,
  onOpen,
  onDelete,
}: {
  node: RepoNode;
  root: string;
  selected: string | null;
  onOpen: (path: string) => void;
  onDelete: (path: string, directory: boolean) => void;
}) {
  // The backend reports workspace-relative paths; everything the pane sends is
  // relative to the repository, so the prefix comes off exactly once, here.
  const relative = node.path.startsWith(`${root}/`) ? node.path.slice(root.length + 1) : node.path;
  // A file restored from the address bar is usually several directories down. A
  // tree that opened fully closed would leave the developer reading a file the
  // tree cannot show them. Initial state only: after the first render the twisty
  // belongs to the developer, not to the URL.
  const [open, setOpen] = useState(() => selected !== null && selected.startsWith(`${relative}/`));

  if (node.type === "directory") {
    return (
      <li>
        {/*
          The row is a box of its own rather than the `li` being the flex
          container, because the `li` also holds the children: made a row itself,
          it laid the nested list out beside the directory name instead of under
          it, and a highlight drawn on it covered the whole subtree.
        */}
        <div className="tree-row">
          <Button variant="ghost" size="sm"
            className="tree-dir h-auto flex-1 justify-start py-1 font-normal"
            title={node.name}
            onClick={() => setOpen(!open)}
            aria-expanded={open}
          >
            <span className="twisty inline-block w-3 shrink-0 text-center text-xs text-muted-foreground" aria-hidden="true">
              {open ? "▾" : "▸"}
            </span>
            <span className="tree-name">{node.name}</span>
          </Button>
          <Button variant="ghost" size="icon-xs"
            className="tree-delete"
            title={`Delete ${node.name}`}
            aria-label={`Delete ${node.name}`}
            onClick={() => onDelete(relative, true)}
          >
            ×
          </Button>
        </div>
        {open && (
          <ul>
            {(node.children ?? []).map((child) => (
              <TreeNode
                key={child.path}
                node={child}
                root={root}
                selected={selected}
                onOpen={onOpen}
                onDelete={onDelete}
              />
            ))}
            {node.elided ? <li className="muted text-muted-foreground">…{node.elided} more</li> : null}
          </ul>
        )}
      </li>
    );
  }

  return (
    <li>
      <div className={selected === relative ? "tree-row active" : "tree-row"}>
        <Button variant="ghost" size="sm"
          className="tree-file h-auto flex-1 justify-start py-1 font-normal"
          title={node.name}
          aria-current={selected === relative ? "true" : undefined}
          onClick={() => onOpen(relative)}
        >
          {/*
            An empty slot exactly as wide as a twisty. Without it a file name
            started where a directory's arrow starts and a directory name a
            twisty further in, so nothing in the tree lined up vertically and the
            indentation said nothing about the depth. Marked hidden from the
            reading order: it is alignment, not content.
          */}
          <span className="twisty leaf inline-block w-3 shrink-0 text-center text-xs text-muted-foreground" aria-hidden="true" />
          <span className="tree-name">{node.name}</span>
        </Button>
        <Button variant="ghost" size="icon-xs"
          className="tree-delete"
          title={`Delete ${node.name}`}
          aria-label={`Delete ${node.name}`}
          onClick={() => onDelete(relative, false)}
        >
          ×
        </Button>
      </div>
    </li>
  );
}

/**
 * Monaco, mounted by hand.
 *
 * A model per path rather than one model whose language and value are swapped:
 * that is what keeps the undo history of a file its own, so switching away and
 * back does not lose it. The save keybinding is registered on the editor rather
 * than on the window, so Ctrl+S outside the editor still belongs to the browser.
 */
function Editor({
  path,
  language,
  value,
  onChange,
  onSave,
}: {
  path: string;
  language: string;
  value: string;
  onChange: (value: string) => void;
  onSave: () => void;
}) {
  const host = useRef<HTMLDivElement | null>(null);
  const editor = useRef<monaco.editor.IStandaloneCodeEditor | null>(null);
  // Held in a ref so the keybinding, which is registered once, always calls the
  // current save rather than the one that existed when the editor was created.
  const save = useRef(onSave);
  save.current = onSave;

  const dark = useMemo(
    () => window.matchMedia?.("(prefers-color-scheme: dark)").matches ?? false,
    [],
  );

  useEffect(() => {
    if (!host.current) return;
    const instance = monaco.editor.create(host.current, {
      value,
      language,
      theme: dark ? "vs-dark" : "vs",
      automaticLayout: true,
      minimap: { enabled: false },
      scrollBeyondLastLine: false,
      fontSize: 13,
      tabSize: 4,
      renderWhitespace: "selection",
    });
    editor.current = instance;

    const changed = instance.onDidChangeModelContent(() => {
      onChange(instance.getValue());
    });
    instance.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => save.current());

    return () => {
      changed.dispose();
      instance.getModel()?.dispose();
      instance.dispose();
      editor.current = null;
    };
    // Deliberately not depending on `value`: the editor owns the text once it is
    // created, and re-creating it on every keystroke would fight the developer for
    // the cursor. `path` is in the list so a different file is a different editor.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, language, dark]);

  return <div className="monaco" ref={host} />;
}

/**
 * dirtyShownThisSession answers whether the changes panel has already opened
 * itself in this browser session, and records that it has.
 *
 * Same reading of "reopen" as `fetchedThisSession` below, and wrapped for the same
 * reason. Where the accessor throws — a browser configured to block site data — the
 * honest fallback is the opposite one: showing the work twice costs a keystroke,
 * not showing it is the thing §5.11 item 6 forbids.
 */
function dirtyShownThisSession(): boolean {
  const key = "ode.repo.dirty_shown";
  try {
    if (window.sessionStorage.getItem(key)) return true;
    window.sessionStorage.setItem(key, "1");
    return false;
  } catch {
    return false;
  }
}

/**
 * fetchedThisSession answers whether this browser session has already fetched the
 * remote, and records that it has.
 *
 * sessionStorage rather than a module-level flag: a reload discards the module
 * and would otherwise re-fetch, which is the case this exists for. It is wrapped
 * because the accessor itself throws in a browser configured to block site data —
 * and there the honest fallback is to fetch, which is what the pane did before.
 */
function fetchedThisSession(): boolean {
  const key = "ode.repo.fetched";
  try {
    if (window.sessionStorage.getItem(key)) return true;
    window.sessionStorage.setItem(key, "1");
    return false;
  } catch {
    return false;
  }
}
