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
  type RepoChange,
  type RepoConnection,
  type RepoFile,
  type RepoNode,
  type RepoStatus,
  type RepoVerification,
  type Session,
} from "./api";
import { Abandoned, reconnect } from "./github";
import { monaco, monacoLanguage } from "./monaco";
import { setParam, useParam } from "./router";
import { Busy, Muted, Pane, bytes, clock, dateTime, describe, shortSHA } from "./ui";
import { WorkbenchBar } from "./workbench";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Checkbox } from "@/components/ui/checkbox";
import {
  File,
  FileArchive,
  FileBraces,
  FileCode,
  FileCog,
  FileImage,
  FileLock,
  FileSpreadsheet,
  FileTerminal,
  FileText,
  Sparkles,
  type LucideIcon,
} from "lucide-react";

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
  /*
   * When the remote was last actually contacted, as epoch milliseconds, or null
   * for a session that has never reached it.
   *
   * Held here rather than read off the status, because `fetched` on a status is a
   * fact about *that request*: it says this call contacted the remote, not that
   * the distance beside it is fresh. Every status that follows — saving a file,
   * committing, pushing, opening a panel — comes back with it false, and the bar
   * would go straight back to calling a distance "unfetched" that a fetch a second
   * earlier had made current. The distance itself stays current, because it is
   * measured against the remote-tracking ref that the fetch moved; what the
   * developer is missing is when that was, and that is what this remembers.
   */
  const [fetchedAt, setFetchedAt] = useState<number | null>(() => lastFetchAt());
  const [needs, setNeeds] = useState<"github_connection" | "repository" | null>(null);
  /** Connected, and GitHub has stopped accepting the credential. */
  const [lapsed, setLapsed] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(true);

  // Every status the pane accepts arrives through here, so that a fetch is recorded
  // once wherever it came from: the fetch on open, the Fetch button, or the status
  // a push reads back.
  const receive = useCallback((next: RepoStatus) => {
    setStatus(next);
    if (next.fetched) setFetchedAt(recordFetch());
  }, []);

  // One loader for both, because the answer to "what should this pane show" is the
  // pair: a connection without a repository is the picker, and a repository always
  // implies a connection.
  const reload = useCallback(async (fetchRemote = false) => {
    setBusy(true);
    setError(null);
    setLapsed(false);
    try {
      const current = await api.repoConnection();
      setConnection(current);
      if (!current.connected) {
        setStatus(null);
        setNeeds("github_connection");
        return;
      }
      try {
        receive(await api.repoStatus(fetchRemote));
        setNeeds(null);
      } catch (e: unknown) {
        // The `needs` is read before the status code, because both of these are 409
        // and they are opposite answers: one means pick a repository, the other means
        // the credential ODE holds has stopped working. Read the other way round, a
        // lapsed credential put the developer in front of a repository picker that
        // could not list anything.
        if (isCredentialLapse(e)) {
          setStatus(null);
          setLapsed(true);
          setNeeds("github_connection");
          return;
        }
        if (e instanceof ApiError && e.status === 409) {
          setStatus(null);
          setNeeds("repository");
          return;
        }
        throw e;
      }
    } catch (e: unknown) {
      if (isCredentialLapse(e)) {
        setStatus(null);
        setLapsed(true);
        setNeeds("github_connection");
        return;
      }
      setError(describe(e));
    } finally {
      setBusy(false);
    }
  }, [receive]);

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
      {/*
        The wait for `/repo`, which is the only part of this pane that is slow.

        The guard above only holds until the connection is in, and that answer costs
        nothing; the status behind it reads the checkout and, once per browser
        session, fetches from GitHub. Between the two the pane had a bar and nothing
        under it — a workbench that looks like it holds no repository rather than one
        still being read. Same animated line the shell shows while the session loads,
        for the same reason: a slow answer has to say it is coming.
      */}
      {busy && !error && !status && needs === null && (
        <Pane title="Code" subtitle="The operator repository">
          <Busy>Reading the checkout…</Busy>
        </Pane>
      )}
      {error && (
        <Pane title="Code" subtitle="The operator repository">
          <p className="error text-destructive">{error}</p>
          <Button variant="outline" onClick={() => void reload()}>Try again</Button>
        </Pane>
      )}
      {!error && needs === "github_connection" && (
        <ConnectPane
          session={session}
          connection={connection}
          lapsed={lapsed}
          onReconnected={() => void reload()}
        />
      )}
      {!error && needs === "repository" && connection?.connected && (
        <>
          <RepositoryPicker onSelected={() => void reload(true)} />
          <ConnectedPane
            connection={connection}
            onDisconnected={() => void reload()}
            onReconnected={() => void reload()}
          />
        </>
      )}
      {!error && status && (
        <WorkingCopy
          status={status}
          connection={connection}
          // Read from /session rather than probed: whether this deployment has an
          // LLM provider is a deployment fact, and the pane should not learn it from
          // a failed request.
          canDraft={session.repo?.commit_message_draft === true}
          fetchedAt={fetchedAt}
          onStatus={receive}
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
  lapsed = false,
  onReconnected,
}: {
  session: Session;
  connection: RepoConnection | null;
  /** The account is connected and GitHub has stopped accepting the credential. */
  lapsed?: boolean;
  onReconnected?: () => void;
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

  if (lapsed) {
    return (
      <Pane title="GitHub" subtitle="The stored credential has stopped working">
        <p>
          GitHub is no longer accepting the token ODE holds for{" "}
          {connection?.identity?.login ?? "this account"} — it was revoked, it expired, or
          the authorisation was withdrawn. Nothing has been lost: the working copy is on
          your own storage, and reconnecting replaces the token in place.
        </p>
        <ReconnectButton onDone={() => onReconnected?.()} />
      </Pane>
    );
  }

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
  onReconnected,
}: {
  connection: RepoConnection;
  onDisconnected: () => void;
  onReconnected: () => void;
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
        Reconnecting replaces the stored token, which is what a credential GitHub has
        stopped accepting needs — the date above is when this one was stored, not proof
        that it still works. Disconnecting forgets it instead. Either way your working
        copy stays where it is: it is on your own storage, and ODE does not delete your
        work.
      </p>
      <div className="account-actions">
        <ReconnectButton onDone={onReconnected} />
        <Button variant="outline" onClick={() => void disconnect()} disabled={pending}>
          Disconnect
        </Button>
      </div>
    </Pane>
  );
}

/**
 * The button that was missing everywhere except the bar.
 *
 * A credential that lapses mid-session is not a rare state, and until now only one
 * of the four places that meet it could do anything about it: the account card
 * offered Disconnect and nothing else, the repository list printed GitHub's 401
 * beside a spinner that never stopped, and the pane's own loader had no case for it.
 * Disconnect-then-connect *is* a repair, but it asks a developer to throw the
 * connection away in order to get it back, which reads like losing something.
 *
 * The popup flow rather than the connect card's full-page one, for the reason
 * github.ts gives: the tab keeps its state. A click is what opens it, which is the
 * arrangement the browser requires anyway.
 */
function ReconnectButton({
  label = "Reconnect GitHub",
  onDone,
}: {
  label?: string;
  onDone: () => void;
}) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = async () => {
    setPending(true);
    setError(null);
    try {
      await reconnect();
      onDone();
    } catch (e: unknown) {
      // Closing the window is a decision, not a failure.
      setError(e instanceof Abandoned ? e.message : describe(e));
    } finally {
      setPending(false);
    }
  };

  return (
    <>
      <Button variant="default"
        className={pending ? "primary busy animate-pulse" : "primary"}
        onClick={() => void run()}
        disabled={pending}
        title="Opens GitHub in a small window. Nothing on this page is lost."
      >
        {pending ? "Connecting to GitHub…" : label}
      </Button>
      {error && <p className="muted text-muted-foreground">{error}</p>}
    </>
  );
}

/** Whether a refusal is the stored GitHub credential having lapsed. */
function isCredentialLapse(e: unknown): boolean {
  return e instanceof ApiError && e.needs === "github_connection";
}

/** Pick an existing repository, or create one and have it scaffolded. */
function RepositoryPicker({ onSelected }: { onSelected: () => void }) {
  const [repositories, setRepositories] = useState<GitHubRepository[] | null>(null);
  const [filter, setFilter] = useState("");
  const [error, setError] = useState<string | null>(null);
  /** Whether the error above is the credential rather than anything about a repository. */
  const [lapsed, setLapsed] = useState(false);
  const [pending, setPending] = useState<string | null>(null);

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [isPrivate, setPrivate] = useState(false);

  /*
   * Held so that reconnecting can retry it, and so that a failure ends the wait.
   *
   * `repositories` staying null was read as "still loading" no matter what had
   * happened, so a 401 rendered GitHub's message *above* a spinner that ran for as
   * long as the pane was open. An empty list is a different answer from no answer,
   * and both are different from a refusal.
   */
  const load = useCallback(async () => {
    setError(null);
    setRepositories(null);
    try {
      const result = await api.repoRepositories();
      setRepositories(result.repositories);
    } catch (e: unknown) {
      setError(describe(e));
      setLapsed(isCredentialLapse(e));
      setRepositories([]);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const select = async (fullName: string) => {
    setPending(fullName);
    setError(null);
    try {
      await api.repoSelect(fullName);
      onSelected();
    } catch (e: unknown) {
      setError(describe(e));
      setLapsed(isCredentialLapse(e));
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
      setLapsed(isCredentialLapse(e));
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
      {/*
        The repair, beside the refusal that needs it. A developer looking at "401: Bad
        credentials" over an empty list could previously only disconnect the account
        and start again.
      */}
      {lapsed && (
        <div className="repo-lapsed">
          <ReconnectButton onDone={() => void load()} />
        </div>
      )}

      <div className="repo-filter">
        <Input
          placeholder="Filter your repositories"
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
        />
      </div>
      {!repositories && !error && <Busy>Reading your repositories…</Busy>}
      {repositories && shown.length === 0 && !error && <Muted>No repository matches.</Muted>}
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
  canDraft,
  fetchedAt,
  onStatus,
  onReload,
}: {
  status: RepoStatus;
  connection: RepoConnection | null;
  canDraft: boolean;
  fetchedAt: number | null;
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
        canDraft={canDraft}
        fetchedAt={fetchedAt}
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
/**
 * A file's icon and its hue, from its name.
 *
 * VS Code's arrangement without VS Code's icon font: the recognisable part of that
 * list is not the glyph, it is that the same kind of file is always the same colour
 * — so a developer finds the Python file among fifteen paths by hue before they
 * have read a single name. The hues are the five families below rather than one per
 * extension, because a per-extension palette is a legend nobody learns.
 *
 * The mapping is by extension, then by whole filename for the handful that carry
 * their type in the name rather than after a dot — a Dockerfile is not a file with
 * no type.
 */
const FILE_ICONS: Record<string, [LucideIcon, IconFamily]> = {
  py: [FileCode, "code"],
  ipynb: [FileCode, "code"],
  ts: [FileCode, "code"],
  tsx: [FileCode, "code"],
  js: [FileCode, "code"],
  jsx: [FileCode, "code"],
  go: [FileCode, "code"],
  rs: [FileCode, "code"],
  java: [FileCode, "code"],
  sql: [FileCode, "code"],
  json: [FileBraces, "data"],
  yaml: [FileCog, "config"],
  yml: [FileCog, "config"],
  toml: [FileCog, "config"],
  ini: [FileCog, "config"],
  cfg: [FileCog, "config"],
  conf: [FileCog, "config"],
  env: [FileCog, "config"],
  sh: [FileTerminal, "code"],
  bash: [FileTerminal, "code"],
  csv: [FileSpreadsheet, "data"],
  tsv: [FileSpreadsheet, "data"],
  parquet: [FileSpreadsheet, "data"],
  md: [FileText, "doc"],
  rst: [FileText, "doc"],
  txt: [FileText, "doc"],
  png: [FileImage, "media"],
  jpg: [FileImage, "media"],
  jpeg: [FileImage, "media"],
  svg: [FileImage, "media"],
  gif: [FileImage, "media"],
  zip: [FileArchive, "media"],
  gz: [FileArchive, "media"],
  tar: [FileArchive, "media"],
  whl: [FileArchive, "media"],
  lock: [FileLock, "config"],
};

const NAMED_FILES: Record<string, [LucideIcon, IconFamily]> = {
  dockerfile: [FileCog, "config"],
  makefile: [FileTerminal, "code"],
  license: [FileText, "doc"],
  ".gitignore": [FileCog, "config"],
  ".dockerignore": [FileCog, "config"],
};

type IconFamily = "code" | "data" | "config" | "doc" | "media";

function fileIcon(path: string): { Icon: LucideIcon; family: IconFamily } {
  const name = (path.split("/").pop() ?? path).toLowerCase();
  const named = NAMED_FILES[name];
  if (named) return { Icon: named[0], family: named[1] };
  const extension = name.includes(".") ? (name.split(".").pop() ?? "") : "";
  const known = FILE_ICONS[extension];
  if (known) return { Icon: known[0], family: known[1] };
  return { Icon: File, family: "doc" };
}

/**
 * git's own letters, which is the point: `M`, `A`, `D`, `R` mean the same thing
 * here as in `git status`, in a terminal, and in VS Code's own list. The one
 * departure is the conflict — VS Code's `C` collides with a copy, and a conflict is
 * the change a developer must not misread — so it gets `!`.
 *
 * The word itself is on the row as a tooltip and as the accessible label, because a
 * single letter is a reminder for someone who already knows and no help at all to
 * anyone else.
 */
const CHANGE_CODES: Record<RepoChange["kind"], string> = {
  modified: "M",
  added: "A",
  deleted: "D",
  renamed: "R",
  copied: "C",
  untracked: "U",
  unmerged: "!",
  typechange: "T",
};

/**
 * GitHub's own verdict on the stored credential, as a sentence.
 *
 * Shown under a refusal that blamed the credential, because ODE's error text can only
 * be as specific as what ODE checked, and "reconnect the account" is worth nothing to
 * a developer who has just reconnected the account. This says which of the two sides
 * is refusing, and names the property that most often explains it: the token's kind.
 */
function verdictOf(verification: RepoVerification): string {
  // How old the credential is, which is what says whether a reconnection happened at
  // all: a refused credential that ODE stored yesterday is the one that was never
  // replaced, and pressing the same button again is exactly the wrong move.
  const stored = verification.age ? ` ODE has held it for ${verification.age}.` : "";
  if (!verification.valid) {
    return (
      `GitHub refuses this credential: ${verification.code ?? ""} ${verification.message ?? ""}`.trim() +
      `. Token kind: ${verification.kind}, ${verification.length} characters.${stored}`
    );
  }
  const scopes = !verification.scopes_reported
    ? "GitHub reports no scopes for it at all, which is what a GitHub App's user token looks like"
    : verification.scopes.length > 0
      ? `scopes: ${verification.scopes.join(", ")}`
      : "the grant carries no scopes";
  // Two accounts, one of which cannot see the repositories: a working credential for
  // the wrong login is invisible otherwise, and it is a plausible outcome of
  // reconnecting in a browser signed in to a second GitHub account.
  const mismatch =
    verification.stored_login && verification.login && verification.stored_login !== verification.login
      ? ` The row says ${verification.stored_login}, so the account was switched.`
      : "";
  return (
    `GitHub still accepts this credential as ${verification.login ?? "this account"} — ` +
    `${scopes}. Token kind: ${verification.kind}.${stored}${mismatch}`
  );
}

/**
 * A failed git action, with the repair the backend named.
 *
 * The bar is where every refusal of a repository action lands, and several of them
 * are refusals whose answer is a step the developer takes rather than a retry: the
 * credential has gone stale, the checkout points at another repository. The
 * backend has always sent that step as `hint`; this is what puts it on screen.
 */
function explain(e: unknown): string {
  const hint = e instanceof ApiError ? e.hint : undefined;
  return hint ? `${describe(e)} — ${hint}` : describe(e);
}

/** One row of the changes panel: what changed, where it is, and how. */
function ChangeRow({ change }: { change: RepoChange }) {
  const { Icon, family } = fileIcon(change.path);
  const cut = change.path.lastIndexOf("/");
  const directory = cut > 0 ? change.path.slice(0, cut) : "";
  return (
    <li className={`change-row ${change.kind}`}>
      <Icon className={`change-icon icon-${family}`} aria-hidden="true" />
      <span className="change-name">{change.path.slice(cut + 1)}</span>
      {/*
        The directory dimmed and beside the name rather than in front of it, so a
        column of paths reads as a column of *files*. Deep in a package the useful
        half of `pkg/simulation/solar.py` is the last segment, and it was the half
        that used to fall off the end of the row.
      */}
      <span className="change-dir">
        {directory}
        {change.renamed_from && <> ← {change.renamed_from}</>}
      </span>
      {change.staged && (
        <span className="badge inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">
          staged
        </span>
      )}
      <span className="change-status" title={change.kind} aria-label={change.kind}>
        {CHANGE_CODES[change.kind] ?? "?"}
      </span>
    </li>
  );
}

function RepoBar({
  status,
  connection,
  canDraft,
  onStatus,
  fetchedAt,
  onReload,
  onChanged,
}: {
  status: RepoStatus;
  connection: RepoConnection | null;
  /** Whether this deployment has an LLM provider to draft a commit message with. */
  canDraft: boolean;
  /** When the remote was last contacted, epoch milliseconds, or null for never. */
  fetchedAt: number | null;
  onStatus: (status: RepoStatus) => void;
  onReload: (fetchRemote?: boolean) => Promise<void>;
  onChanged: () => void;
}) {
  const [message, setMessage] = useState("");
  // The last draft, verbatim. Kept so the Draft button can tell a message the
  // developer wrote from one it produced itself: replacing the first without asking
  // throws away typing, and asking about the second is a pointless question.
  const [drafted, setDrafted] = useState("");
  const [pending, setPending] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [panel, setPanel] = useState<Panel | null>(null);
  /*
   * The action that refused for want of a working GitHub credential, kept whole.
   *
   * A stale credential is the one refusal in this bar whose repair is a round trip
   * through GitHub and then *the same action again*. Holding the closure is what
   * turns that into one button: the developer does not have to remember they were
   * pushing, find the button again, and press it a second time. It is cleared on the
   * next action, so a button offering to finish a push cannot outlive the push.
   */
  const [stalled, setStalled] = useState<{
    label: string;
    run: () => Promise<RepoStatus | void>;
  } | null>(null);
  /** GitHub's verdict on the credential, fetched only when one is blamed. */
  const [verdict, setVerdict] = useState<string | null>(null);
  const bar = useRef<HTMLDivElement | null>(null);
  const triggers = useRef<Partial<Record<Panel, HTMLButtonElement | null>>>({});

  /*
   * One action, with its refusal handled in one place.
   *
   * `run` returning a status is what saves the round trip after it: an action that
   * already read the working copy back hands that status over, and only one that
   * did not costs a reload. That is not only economy — the reload does not fetch,
   * so a push handing its status to `onStatus` itself and then leaving `act` to
   * reload would have replaced the fetched status it had just produced with an
   * unfetched one.
   *
   * `moved` is whether the files under the tree can have changed. False for the
   * actions that only ask the remote a question: re-listing the tree after a fetch
   * is a command in the developer's pod for an answer that cannot have changed.
   */
  const act = async (
    label: string,
    run: () => Promise<RepoStatus | void>,
    moved = true,
  ) => {
    setPending(label);
    setError(null);
    setNote(null);
    setStalled(null);
    setVerdict(null);
    try {
      const next = await run();
      if (next) onStatus(next);
      else await onReload();
      if (moved) onChanged();
    } catch (e: unknown) {
      setError(explain(e));
      // 409 `needs: "github_connection"` — either there is no credential or GitHub
      // has stopped accepting the one ODE holds. Both are repaired the same way, and
      // the action itself is still worth finishing.
      if (e instanceof ApiError && e.needs === "github_connection") {
        setStalled({ label, run });
        // And ask GitHub directly, because the developer's next question is whether
        // it is really the credential — especially if they just replaced it. Fired
        // rather than awaited: the refusal is already on screen, and this only adds
        // to it.
        void api
          .repoConnection(true)
          .then((current) => {
            if (current.verification) setVerdict(verdictOf(current.verification));
          })
          .catch(() => {
            // The verdict is a courtesy. Failing to get one must not replace the
            // refusal the developer is reading.
          });
      }
    } finally {
      setPending(null);
    }
  };

  /*
   * Reconnect, then finish what was asked.
   *
   * From a click, deliberately: the flow needs a popup, and a browser grants a popup
   * to a gesture and refuses one to the catch block of a request that already failed.
   * That is also the honest arrangement — reconnecting can mean GitHub asking the
   * developer to consent again, and a consent screen nobody asked for is not
   * something to open behind their back.
   */
  const reconnectAndRetry = async () => {
    const held = stalled;
    if (!held) return;
    setPending("reconnect");
    setError(null);
    setNote(null);
    try {
      await reconnect();
    } catch (e: unknown) {
      // A closed window or a refused consent is the developer's decision, not a
      // failure to report as one.
      if (e instanceof Abandoned) setNote(e.message);
      else setError(explain(e));
      setPending(null);
      return;
    }
    setStalled(null);
    setPending(null);
    await act(held.label, held.run);
  };

  const commit = () =>
    act("commit", async () => {
      const committed = await api.repoCommit(message);
      setMessage("");
      setDrafted("");
      setNote(
        `Committed ${shortSHA(committed.sha)} on ${committed.branch}: ${committed.files} file(s). Nothing is pushed yet.`,
      );
      return api.repoStatus();
    });

  /*
   * The draft (§5.11 item 5, and §5.7 for what answers it).
   *
   * Not routed through `act`, and that is the whole point of it: `act` reloads the
   * status and tells the tree the working copy moved, because everything else in
   * this bar changes the working copy. Drafting changes nothing — it reads the diff
   * and fills in a text box — so it reloads nothing, and the developer's next
   * decision is still theirs.
   */
  const draft = async () => {
    if (message.trim() !== "" && message !== drafted) {
      if (!window.confirm("Replace the commit message you have written with a drafted one?")) {
        return;
      }
    }
    setPending("draft");
    setError(null);
    setNote(null);
    try {
      const proposal = await api.repoCommitMessage();
      setMessage(proposal.message);
      setDrafted(proposal.message);
      setNote(
        proposal.truncated
          ? `Drafted from ${proposal.files} file(s), but the diff was too large to send whole — read the message before committing.`
          : `Drafted from ${proposal.files} file(s). It is a suggestion: edit it, and commit when you mean to.`,
      );
    } catch (e: unknown) {
      setError(explain(e));
    } finally {
      setPending(null);
    }
  };

  const push = () =>
    act("push", async () => {
      const pushed = await api.repoPush();
      setNote(pushed.output || `Pushed ${pushed.branch} to ${pushed.remote}.`);
      // Fetched, because a push is the moment the distance to the remote changes,
      // and the bar is about to be read for exactly that.
      return api.repoStatus(true);
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
              return api.repoStatus();
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
          <span>
            {error ?? note}
            {verdict && (
              <span className="repo-bar-verdict muted text-muted-foreground">{verdict}</span>
            )}
          </span>
          {stalled && (
            <Button variant="default"
              className={pending === "reconnect" ? "primary busy animate-pulse" : "primary"}
              onClick={() => void reconnectAndRetry()}
              disabled={pending !== null}
              title="Opens GitHub in a small window. This tab, and anything typed in it, stays as it is."
            >
              {pending === "reconnect"
                ? "Connecting to GitHub…"
                : `Reconnect GitHub and ${stalled.label}`}
            </Button>
          )}
          <Button variant="outline"
            onClick={() => {
              setError(null);
              setNote(null);
              setStalled(null);
              setVerdict(null);
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

          {/*
            The commit, beside the branch that points at it.

            Seven characters and not the subject: what the bar is answering is
            "which commit am I on", which is the question a developer asks when
            they are about to compare it with a build, a tag or a run elsewhere —
            and that comparison is done on the sha. The subject and the full sha
            are on the hover, and the panel above carries both in full.
          */}
          <span className="repo-bar-fact" title="The branch the working copy is on">
            {status.branch ?? "—"}
            {status.head && (
              <code
                className="repo-bar-sha"
                title={status.head_subject ? `${status.head}\n${status.head_subject}` : status.head}
              >
                {shortSHA(status.head)}
              </code>
            )}
            {status.unborn && <span className="badge inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">no commits yet</span>}
            {status.detached && <span className="badge warn inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs text-foreground">detached</span>}
          </span>

          {/*
            "in sync" rather than "0 ahead, 0 behind": the pair of zeroes is four
            words to say nothing happened, and the bar's room is worth more than
            that. What is in front of it is the age of the answer, because a stale
            zero is not agreement — the same reason the status route makes
            `fetched` explicit.

            The age comes from the pane and not from `status.fetched`, which is a
            fact about one request rather than about the distance: see the note on
            `fetchedAt` where it is held.
          */}
          <span
            className="repo-bar-fact"
            title={
              fetchedAt === null
                ? "Against the remote as ODE last saw it. Press Fetch to make it current."
                : `Against the remote as it was at the fetch at ${clock(fetchedAt)}`
            }
          >
            {fetchedAt === null ? "unfetched · " : `fetched ${clock(fetchedAt)} · `}
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

          {/*
            Through `act` rather than through the pane's reload, for the two things
            `act` adds: the button says it is working, and a fetch refused for want
            of a credential offers the reconnect-and-retry the rest of the bar
            offers. `moved` is false — a fetch writes remote-tracking refs and
            nothing the tree lists.
          */}
          <Button variant="outline"
            className={pending === "fetch" ? "busy animate-pulse" : undefined}
            onClick={() => void act("fetch", () => api.repoStatus(true), false)}
            disabled={pending !== null}
            title="Ask GitHub where the branch stands. The working copy is not touched."
          >
            {pending === "fetch" ? "Fetching…" : "Fetch"}
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
                  <code>{shortSHA(status.head)}</code> {status.head_subject}
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
                  <ChangeRow key={change.path} change={change} />
                ))}
              </ul>
              <div className="commit-box">
                {/*
                  A textarea rather than a single line, because a commit message is a
                  subject *and* a body: the reason a change was made is the half a
                  reader of the history cannot recover from the diff, and a field one
                  line tall says not to bother writing it.
                */}
                <Textarea
                  className="commit-message"
                  rows={3}
                  placeholder={"What this change does\n\nAnd why, if the diff does not say it."}
                  value={message}
                  onChange={(event) => setMessage(event.target.value)}
                />
                <div className="commit-actions">
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
                {/*
                  Only where a provider is configured. The repo routes are served
                  without one, and a button that could only ever answer "unavailable"
                  is worse than no button.
                */}
                {canDraft && (
                  <Button variant="outline"
                    className={pending === "draft" ? "busy animate-pulse" : undefined}
                    onClick={() => void draft()}
                    disabled={pending !== null}
                    title="Read the diff and propose a message. Nothing is committed."
                  >
                    <Sparkles aria-hidden="true" />
                    {pending === "draft" ? "Drafting…" : "Draft"}
                  </Button>
                )}
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

/** Where the moment of the last fetch is kept, in epoch milliseconds. */
const fetchedAtKey = "ode.repo.fetched_at";

/**
 * recordFetch and lastFetchAt are when the remote was last actually contacted.
 *
 * Beside fetchedThisSession and in the same storage for the same reason: a page
 * reload discards the module, and it is precisely the reload that does not fetch
 * again — so a module-level moment would be lost by the one event that leaves the
 * distance on screen unchanged.
 *
 * Separate from that flag rather than folded into it, because the two answer
 * different questions. The flag is claimed on mount, *before* the fetch, so that
 * a second mount does not fetch again; the moment is recorded when a fetched
 * status has actually come back. Merging them would put a time on the bar for a
 * fetch that had not happened yet.
 */
function recordFetch(): number {
  const at = Date.now();
  try {
    window.sessionStorage.setItem(fetchedAtKey, String(at));
  } catch {
    // A browser blocking site data costs the reading across a page reload, and
    // nothing else: the moment is in React state for as long as the pane lives.
  }
  return at;
}

function lastFetchAt(): number | null {
  try {
    const raw = window.sessionStorage.getItem(fetchedAtKey);
    if (!raw) return null;
    const at = Number(raw);
    return Number.isFinite(at) && at > 0 ? at : null;
  } catch {
    return null;
  }
}
