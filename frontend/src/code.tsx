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
import { Muted, Pane, bytes, dateTime, describe, shortId } from "./ui";

/**
 * The Code view (SPEC §5.11, M7).
 *
 * Four states, in the order a developer meets them: connect GitHub, pick or create
 * a repository, then the working copy — a full file tree with Monaco beside it —
 * and the git actions.
 *
 * Three things on screen are load-bearing rather than decorative.
 *
 *   - **Where the checkout is.** The path on the per-user PVC is shown, because
 *     "somewhere in your pod" is not something a developer can act on, and because
 *     it is the same directory their kernel runs in.
 *
 *   - **What is uncommitted, always.** §5.11 item 6: work found on reopen is
 *     surfaced with the three answers beside it — commit, stash, discard — and
 *     never silently reset. Discard asks again before it runs.
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
  }, [reload]);

  if (busy && !connection) return <Pane title="Code" subtitle="Loading…"><Muted>Loading…</Muted></Pane>;

  return (
    <main className="panes code">
      {error && (
        <Pane title="Code" subtitle="SPEC §5.11 — the operator repository">
          <p className="error">{error}</p>
          <button onClick={() => void reload()}>Try again</button>
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
    <Pane title="GitHub" subtitle="The operator lives in a repository of yours (SPEC §5.11, D9)">
      <p>
        ODE clones the repository into your own workspace, writes files there when you
        or the assistant ask it to, and commits and pushes only when you say so.
      </p>
      <p className="muted">
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
      {error && <p className="error">{error}</p>}
      <button className="primary" onClick={() => void connect()} disabled={pending}>
        {pending ? "Opening GitHub…" : "Connect GitHub"}
      </button>
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
        <p className="warn">
          The grant is missing {identity.missing_scopes.join(", ")}. A push that touches{" "}
          <code>.github/workflows/</code> will be rejected until you reconnect and allow it.
        </p>
      )}
      <p className="muted">
        Disconnecting forgets the token. Your working copy stays where it is — it is on
        your own storage, and ODE does not delete your work.
      </p>
      <button onClick={() => void disconnect()} disabled={pending}>
        Disconnect
      </button>
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
        <input
          placeholder="new-operator-name"
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
        <input
          placeholder="What it does (optional)"
          value={description}
          onChange={(event) => setDescription(event.target.value)}
        />
        <label className="checkbox">
          <input
            type="checkbox"
            checked={isPrivate}
            onChange={(event) => setPrivate(event.target.checked)}
          />
          Private
        </label>
        <button className="primary" type="submit" disabled={!name.trim() || pending === "create"}>
          {pending === "create" ? "Creating…" : "Create and scaffold"}
        </button>
      </form>
      <p className="muted">
        A created repository starts empty and the template is written into your working
        copy — the first commit is yours to make and review.
      </p>

      {error && <p className="error">{error}</p>}

      <div className="repo-filter">
        <input
          placeholder="Filter your repositories"
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
        />
      </div>
      {!repositories && <Muted>Reading your repositories…</Muted>}
      {repositories && shown.length === 0 && <Muted>No repository matches.</Muted>}
      <ul className="repo-list">
        {shown.map((repository) => (
          <li key={repository.full_name}>
            <div>
              <span className="repo-name">{repository.full_name}</span>
              {repository.private && <span className="badge">private</span>}
              {repository.empty && <span className="badge">empty</span>}
              {!repository.can_push && <span className="badge warn">read-only</span>}
              {repository.description && <p className="muted">{repository.description}</p>}
            </div>
            <button
              onClick={() => void select(repository.full_name)}
              disabled={pending === repository.full_name}
            >
              {pending === repository.full_name ? "Cloning…" : "Work on this"}
            </button>
          </li>
        ))}
      </ul>
    </Pane>
  );
}

/** The working copy: git state, the actions, the tree and the editor. */
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
  const [message, setMessage] = useState("");
  const [pending, setPending] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [treeVersion, setTreeVersion] = useState(0);

  const act = async (label: string, run: () => Promise<RepoStatus | void>) => {
    setPending(label);
    setError(null);
    setNote(null);
    try {
      const next = await run();
      if (next) onStatus(next);
      else await onReload();
      setTreeVersion((version) => version + 1);
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
   * fact ODE is already holding and already rendering two paragraphs up, so
   * letting the button stay live makes the developer write a commit message and
   * press it in order to be told something the screen knew before they started.
   * Worse for push, where the thing being confirmed is whether their work is about
   * to land in a repository they did not choose.
   *
   * Stash and discard stay live, and so does writing the scaffold: none of them
   * leave the working copy, and a checkout pointing at the wrong repository is
   * exactly the situation where setting the changes aside is the useful move.
   */
  const remoteMismatch = status.remote_mismatch === true;

  const discard = () =>
    act("discard", async () => {
      // The one destructive action in the pane, and the second of the two
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

  return (
    <>
      <Pane
        title={status.link.full_name}
        subtitle={`${status.workspace}/${status.link.path} on your own storage`}
        actions={
          <>
            <a href={status.link.html_url} target="_blank" rel="noreferrer">
              Open on GitHub
            </a>
            <button onClick={() => void onReload(true)} disabled={pending !== null}>
              Fetch
            </button>
            <button
              onClick={() =>
                void act("unlink", async () => {
                  await api.repoUnlink();
                })
              }
              disabled={pending !== null}
            >
              Switch repository
            </button>
          </>
        }
      >
        <dl className="kv">
          <dt>Branch</dt>
          <dd>
            {status.branch ?? "—"}
            {status.unborn && <span className="badge">no commits yet</span>}
            {status.detached && <span className="badge warn">detached</span>}
          </dd>
          <dt>Remote</dt>
          <dd>
            {status.fetched ? "" : "(not fetched) "}
            {status.ahead} ahead, {status.behind} behind
            {status.diverged && <span className="badge warn">diverged</span>}
          </dd>
          <dt>Head</dt>
          <dd>
            {status.head ? (
              <>
                <code>{shortId(status.head)}</code> {status.head_subject}
                {status.head_date && <span className="muted"> · {dateTime(status.head_date)}</span>}
              </>
            ) : (
              "—"
            )}
          </dd>
          {status.link.operator_lib_ref && (
            <>
              <dt>Operator Lib</dt>
              <dd>
                pinned at <code>{status.link.operator_lib_ref}</code> (D15)
              </dd>
            </>
          )}
        </dl>

        {status.remote_mismatch && (
          <p className="warn">
            This checkout&apos;s origin is <code>{status.remote}</code>, which is not the
            repository ODE has linked. Commit and push are disabled: git would do both quite
            happily, and the work would end up in a repository you did not choose. Nothing has
            been changed — select the repository this checkout actually points at, or move the
            directory aside and let ODE clone the linked one. Stash and discard still work,
            because they stay in the working copy.
          </p>
        )}
        {!status.scaffold.complete && (
          <p className="warn">
            Missing from the operator template: {status.scaffold.missing.join(", ")}.{" "}
            <button
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
            </button>
          </p>
        )}
        {status.diverged && (
          <p className="warn">
            The branch has moved on both sides. ODE does not merge for you: pull in a
            terminal in your own pod, or push a branch of your own.
          </p>
        )}
        {connection?.identity?.missing_scopes?.length ? (
          <p className="warn">
            The GitHub grant is missing {connection.identity.missing_scopes.join(", ")}, so a
            push that touches the build workflow will be rejected.
          </p>
        ) : null}

        {error && <p className="error">{error}</p>}
        {note && <p className="note">{note}</p>}

        <h3>
          Uncommitted changes{" "}
          <span className="muted">{status.dirty ? `(${status.changes.length})` : "(none)"}</span>
        </h3>
        {status.dirty ? (
          <>
            <ul className="changes">
              {status.changes.map((change) => (
                <li key={change.path}>
                  <span className={`change ${change.kind}`}>{change.kind}</span>
                  <code>{change.path}</code>
                  {change.renamed_from && <span className="muted"> from {change.renamed_from}</span>}
                  {change.staged && <span className="badge">staged</span>}
                </li>
              ))}
            </ul>
            <div className="commit-box">
              <input
                placeholder="What this change does"
                value={message}
                onChange={(event) => setMessage(event.target.value)}
              />
              <button
                className="primary"
                onClick={() => void commit()}
                disabled={!message.trim() || pending !== null || remoteMismatch}
                title={
                  remoteMismatch
                    ? "The checkout points at a different repository than the linked one."
                    : undefined
                }
              >
                {pending === "commit" ? "Committing…" : "Commit"}
              </button>
              <button
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
              </button>
              <button className="danger" onClick={() => void discard()} disabled={pending !== null}>
                Discard
              </button>
            </div>
          </>
        ) : (
          <Muted>The working copy matches the last commit.</Muted>
        )}

        <div className="push-row">
          <button
            onClick={() => void push()}
            disabled={pending !== null || status.unborn || remoteMismatch}
            title={
              remoteMismatch
                ? "The checkout points at a different repository than the linked one."
                : undefined
            }
          >
            {pending === "push" ? "Pushing…" : `Push${status.ahead ? ` (${status.ahead})` : ""}`}
          </button>
          <span className="muted">
            {remoteMismatch
              ? "Push is refused while this checkout points at another repository — see above."
              : status.unborn
                ? "There is nothing to push until the first commit."
                : "Pushing is what triggers the build workflow, which pushes the image to ghcr.io."}
          </span>
        </div>
      </Pane>

      <FilesPane status={status} version={treeVersion} onChanged={() => void onReload()} />
    </>
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

  useEffect(() => {
    void loadTree();
  }, [loadTree, version]);

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
  }, [selected]);

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
      subtitle="Every file of the repository, editable — nothing hidden, nothing reserved (D14)"
      actions={
        <>
          <button onClick={() => void create()}>New file</button>
          <button onClick={() => void loadTree()}>Refresh</button>
        </>
      }
    >
      <div className="code-layout">
        <div className="file-tree">
          {treeError && <p className="error">{treeError}</p>}
          {!tree && !treeError && <Muted>Reading the working copy…</Muted>}
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
          {fileError && <p className="error">{fileError}</p>}
          {file && (
            <>
              <div className="file-head">
                <code>{file.path}</code>
                <span className="muted">
                  {bytes(file.size)}
                  {file.modified ? ` · ${dateTime(file.modified)}` : ""}
                </span>
                {dirty && <span className="badge warn">unsaved</span>}
                <button
                  className="primary"
                  onClick={() => void save()}
                  disabled={!dirty || saving || file.binary}
                >
                  {saving ? "Saving…" : "Save"}
                </button>
                <span className="muted">Saving writes the working copy. It does not commit.</span>
              </div>
              {file.binary && (
                <Muted>
                  This file is not text, so it is not shown. Editing it here would corrupt it.
                </Muted>
              )}
              {file.truncated && (
                <p className="warn">
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
        <button className="tree-dir" onClick={() => setOpen(!open)}>
          <span className="twisty">{open ? "▾" : "▸"}</span>
          {node.name}
        </button>
        <button className="tree-delete" title="Delete" onClick={() => onDelete(relative, true)}>
          ×
        </button>
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
            {node.elided ? <li className="muted">…{node.elided} more</li> : null}
          </ul>
        )}
      </li>
    );
  }

  return (
    <li>
      <button
        className={selected === relative ? "tree-file active" : "tree-file"}
        onClick={() => onOpen(relative)}
      >
        {node.name}
      </button>
      <button className="tree-delete" title="Delete" onClick={() => onDelete(relative, false)}>
        ×
      </button>
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
