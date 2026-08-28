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

/**
 * The working contexts a developer has open.
 *
 * One workbench is one repository checkout and one kernel, so two operators can be
 * developed at once without either one's working copy moving under the other. This
 * module is what the panes share: which one is on screen, what the others are, and
 * how to open or close one.
 *
 * Which one is on screen lives in `?workbench=`, like everything else this SPA
 * keeps in the URL — a reload, a bookmark and a link to a colleague then mean the
 * same thing. It is mirrored into `api.ts` during *render* rather than in an
 * effect, because the panes below fire their first requests from their own effects,
 * and those run before a parent's. Setting it a moment later would send the Code
 * pane's opening reads to the wrong workbench.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { api, setActiveWorkbench, workbenchLabel, type Workbench } from "./api";
import { getParam, setParam, useParam } from "./router";
import { describe } from "./ui";
import { Button } from "@/components/ui/button";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { XIcon } from "lucide-react";

/**
 * The workbench this SPA chose on the developer's behalf, for an address that named
 * none — and null as soon as anything confirms or replaces that choice.
 *
 * A choice the SPA made is not a choice the developer made, and the difference
 * shows up on exactly one path: a reload of `/?session=S&file=op.py`. The provider
 * below has to name a workbench once it sees two are open, and it makes that choice
 * from the conversation the address names — but that conversation is the chat
 * pane's to publish, and on a reload the chat pane's own request may still be in
 * flight when the workbench list answers. The first workbench is then written into
 * the address, and the pairing corrects it a moment later when the list does
 * arrive.
 *
 * That correction is not a switch away from a workbench the developer was working
 * in. It is the first answer to a question the address left open, and the file the
 * address named was never the guessed workbench's — so it survives, exactly as it
 * survives an address that still named no workbench at all. Without this, whether a
 * reload kept the open file came down to which of two requests answered first.
 */
let assumed: string | null = null;

/**
 * switchWorkbench puts a different workbench on screen, and takes the state that
 * belonged to the last one with it: the open file, and the open conversation.
 *
 * `?file=` is a path inside one checkout, and no two workbenches hold the same
 * repository — the unique index on `(user_sub, full_name)` sees to that. So a path
 * carried across a switch names a file the workbench being opened does not have,
 * and the Files pane answers a switch the developer made with a 404 for a file they
 * never asked to open. Worse, until it is re-read the editor still shows the other
 * checkout's content under that path, and Save would write it into this one.
 *
 * `?session=` is carried the other way round — moved to this workbench's own
 * conversation rather than dropped — because a conversation is about one operator.
 * Leaving the previous one open would put a conversation about another checkout
 * beside this code, which is the confusion two workbenches exist to prevent, and
 * the assistant would then be asked to write files it was never told about.
 *
 * Losing the open file is the honest outcome: nothing here can know which file of
 * the other operator the developer wants. It happens only on an actual change, so
 * re-opening the workbench already on screen — clicking its own tab, or a chat
 * session about it — leaves the editor and the conversation alone.
 */
export function switchWorkbench(id: string | null): void {
  if (id === getParam("workbench")) return;
  // A switch is a claim, so nothing here is the SPA's assumption any more.
  assumed = null;
  setParam("workbench", id);
  setParam("file", null);
  setParam("session", pairedConversation(id));
}

/**
 * A conversation, as pairing needs to see it: an id, and the operator it is about.
 */
export interface PairedConversation {
  id: string;
  /** Empty or absent when the conversation names no workbench. */
  workbench_id?: string;
}

/**
 * The conversations, newest first, as the chat pane last published them.
 *
 * `switchWorkbench` has to answer "which conversation is this workbench's?", and it
 * is a plain function — the tab bar's click handler and this provider's own
 * callbacks reach it directly, from outside any render. The list itself belongs to
 * the chat pane, which is the Code pane's sibling rather than its ancestor, so
 * rather than lift that pane's whole state — the drafts, the marks, the socket a
 * turn runs on — above both, the pane publishes the one projection of it switching
 * needs. Module-level for the reason `activeWorkbench` in api.ts is: there is one
 * of these on screen, and this is what "the open conversation" means to all of it.
 */
let conversations: PairedConversation[] = [];

/**
 * claimedWorkbench is the workbench the open conversation is about, when that is a
 * workbench the developer has open.
 *
 * Null in every case where the conversation cannot answer: `?session=` naming
 * nothing, a conversation the chat pane has not published — a stale link, or a list
 * still being read — one that names no workbench, and one whose workbench has since
 * been closed.
 */
function claimedWorkbench(all: Workbench[]): string | null {
  const open = getParam("session");
  const about = conversations.find((entry) => entry.id === open)?.workbench_id;
  if (!about) return null;
  return all.some((bench) => bench.id === about) ? about : null;
}

/**
 * pairedConversation says which conversation belongs beside a workbench.
 *
 * Three cases keep the open one rather than replacing it. A conversation that names
 * no workbench makes no claim on the code pane — the backend reads an unnamed
 * workbench as "my only one" — so a switch does not move it either, the same way an
 * address that named no workbench keeps its file below. One that is not in the list
 * at all is either a stale link, which the chat pane reports as one, or a session
 * created a moment ago and not yet published; neither is something a workbench
 * switch should silently throw away. And one already about this workbench is the
 * ordinary case: following a conversation to its own operator must not then move
 * the conversation.
 *
 * Otherwise the workbench's own newest conversation, and null when it has none. A
 * workbench with no conversation yet leaves the chat pane asking for one, which is
 * the truthful state — there is nothing to show beside this code.
 */
function pairedConversation(workbench: string | null): string | null {
  const open = getParam("session");
  const current = conversations.find((entry) => entry.id === open);
  if (!current) return open;
  if (!current.workbench_id) return open;
  const target = workbench ?? "";
  if (current.workbench_id === target) return open;
  return conversations.find((entry) => (entry.workbench_id ?? "") === target)?.id ?? null;
}

/**
 * useConversationPairing keeps the code pane on the workbench the open conversation
 * is about, whichever way that conversation was opened.
 *
 * Clicking a session in the list is only one of them. `?session=` is sticky and
 * restored by a reload, it is written by the links from an experiment back to the
 * conversation that launched it, and the back button restores an older pair of
 * parameters wholesale — and until this existed, each of those left `?workbench=`
 * saying whatever it happened to say, which after a view change is "the first one".
 * So the pairing is reconciled here, once, rather than at every place that opens a
 * conversation.
 *
 * Only towards a workbench that is actually open. A conversation may name one that
 * has since been closed — nothing clears the column, and the checkout outlives the
 * workbench — and following that would fight the provider's own repair of a stale
 * `?workbench=`, each writing the other's value back for as long as the pane is on
 * screen. There is no honest thing to show for it: the working copy that
 * conversation was about is not open, and the code pane stays where it is.
 */
export function useConversationPairing(
  listed: PairedConversation[] | null,
  openId: string | null,
): void {
  // Published during render rather than from an effect, for the reason
  // `setActiveWorkbench` is called during render below: the provider's own default
  // reads this list from its effect, and a parent's effects run after its
  // children's, so a list published from an effect here would arrive after the
  // choice it is meant to inform. It is a plain assignment and notifies nobody, so
  // there is nothing about it that a render cannot do.
  conversations = listed ?? [];

  const { all, current } = useWorkbenches();
  // From the list this pane holds rather than through `claimedWorkbench`, because
  // these three are what the effect below watches: derived during render, they
  // change when the pane's own state does. The module-level list is a snapshot for
  // the callers that are not in a render at all.
  const about = listed?.find((entry) => entry.id === openId)?.workbench_id ?? "";
  const opened = all.some((bench) => bench.id === about);
  // Already what the panes are acting in, which is not the same as already named in
  // the address: a developer with one workbench has `?workbench=` absent, and the
  // backend and this provider both read that as their only one. Writing it anyway
  // would put a parameter into every address they copy, and — through
  // switchWorkbench — close the file they had open, on every reload.
  const showing = current?.id === about;

  useEffect(() => {
    if (!about || !opened || showing) return;
    const named = getParam("workbench");
    if (named === null || named === assumed) {
      // An address that named no workbench never claimed one, so the file it names
      // is not another checkout's and it stays. The same rule the provider's own
      // default follows below, for the same link: `/?session=S&file=op.py` — and
      // the same rule again when that default has already run and written its
      // guess, which is the reload race `assumed` exists for.
      //
      // Cleared rather than moved to `about`: this workbench comes from the
      // conversation, which is a claim. The next conversation that names a
      // different one is a switch, and takes the open file with it.
      assumed = null;
      setParam("workbench", about);
      return;
    }
    switchWorkbench(about);
  }, [about, opened, showing]);
}

interface WorkbenchState {
  /** Every workbench the developer has open, oldest first. */
  all: Workbench[];
  /** The one on screen, or null while the list is still loading or empty. */
  current: Workbench | null;
  /** How many this deployment allows, so the button can say why it is disabled. */
  max: number;
  loading: boolean;
  error: string | null;
  /** Puts a workbench on screen. */
  open: (id: string) => void;
  /** Opens a new empty one and switches to it. */
  create: () => Promise<Workbench | null>;
  /** Closes one. The checkout stays on the PVC. */
  close: (id: string) => Promise<void>;
  rename: (id: string, title: string) => Promise<void>;
  /** Re-reads the list, after something changed a link. */
  refresh: () => Promise<void>;
}

const WorkbenchContext = createContext<WorkbenchState | null>(null);

/**
 * useWorkbenches is how a pane reaches the shared state.
 *
 * It answers with a stub outside a provider rather than throwing, because a
 * deployment without a GitHub app has no repository surface and therefore no
 * workbenches, and every pane below still has to render.
 */
export function useWorkbenches(): WorkbenchState {
  const held = useContext(WorkbenchContext);
  return held ?? EMPTY;
}

const EMPTY: WorkbenchState = {
  all: [],
  current: null,
  max: 0,
  loading: false,
  error: null,
  open: () => {},
  create: async () => null,
  close: async () => {},
  rename: async () => {},
  refresh: async () => {},
};

export function WorkbenchProvider({ children }: { children: ReactNode }) {
  const selected = useParam("workbench");
  const [all, setAll] = useState<Workbench[]>([]);
  const [max, setMax] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // During render, not in an effect: see the note at the top of this file. An empty
  // value is legitimate and means "my only workbench", which is what the backend
  // assumes for a request that names none — so a developer who has one never has to
  // have this resolved at all.
  setActiveWorkbench(selected);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const answer = await api.workbenches();
      setAll(answer.workbenches);
      setMax(answer.max);
      setError(null);
    } catch (e: unknown) {
      setError(describe(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  /*
   * One is chosen for the developer as soon as there is more than one.
   *
   * With a single workbench the parameter can stay absent: the backend reads that
   * as "my only one" and everything behaves as it did before workbenches existed.
   * With two it cannot — a request that names none is refused rather than guessed,
   * because choosing between two working copies on the developer's behalf is the
   * failure this whole thing exists to prevent. So the SPA makes the choice
   * visibly, in the URL, where the developer can see and change it.
   *
   * The same effect repairs a stale link: a `?workbench=` naming one that has since
   * been closed falls back to the first rather than leaving every pane on a 404.
   */
  useEffect(() => {
    if (loading || all.length === 0) return;
    const known = all.some((bench) => bench.id === selected);
    if (known) return;
    if (selected === null && all.length === 1) return;
    // The open conversation's workbench first, and the first workbench only when
    // there is no conversation to ask. Not just politeness towards the pairing
    // below: both write in the same commit — a parent's effects run after its
    // children's, so this one writes last — and the pairing would not run again to
    // correct it, because nothing in its dependencies changed. Choosing here is
    // also what stops the code pane showing another operator's repository for the
    // moment between the two writes.
    const chosen = claimedWorkbench(all) ?? all[0].id;
    // Recorded before the write, because a setParam is a re-render and the pairing
    // reads this during one. An address that named a workbench itself was claiming
    // one — this is repairing a stale claim, not filling a gap — so nothing is
    // assumed on that path.
    assumed = selected === null ? chosen : null;
    setParam("workbench", chosen);
    // A `?workbench=` naming one that is gone makes the whole address stale, so the
    // open file goes with it: `?file=` was a path in that checkout. An address that
    // named no workbench never claimed one, so a link written as `/?file=op.py`
    // still opens that file in whichever workbench is chosen here.
    if (selected !== null) setParam("file", null);
  }, [all, loading, selected]);

  const current = useMemo(() => {
    if (selected) return all.find((bench) => bench.id === selected) ?? null;
    return all.length === 1 ? all[0] : null;
  }, [all, selected]);

  const open = useCallback((id: string) => switchWorkbench(id), []);

  const create = useCallback(async () => {
    try {
      const opened = await api.createWorkbench();
      setAll((existing) => [...existing, opened]);
      switchWorkbench(opened.id);
      setError(null);
      return opened;
    } catch (e: unknown) {
      setError(describe(e));
      return null;
    }
  }, []);

  const close = useCallback(
    async (id: string) => {
      try {
        await api.deleteWorkbench(id);
        setError(null);
      } catch (e: unknown) {
        setError(describe(e));
        return;
      }
      // Cleared before the list is re-read, so nothing renders against an id that
      // is gone. The effect above then picks whichever is left. It goes through
      // switchWorkbench so that the conversations about this workbench close with
      // it: they name a working copy that is no longer open, and the assistant
      // cannot act in one.
      if (selected === id) switchWorkbench(null);
      await load();
    },
    [load, selected],
  );

  const rename = useCallback(async (id: string, title: string) => {
    try {
      const renamed = await api.renameWorkbench(id, title);
      setAll((existing) => existing.map((bench) => (bench.id === id ? renamed : bench)));
      setError(null);
    } catch (e: unknown) {
      setError(describe(e));
    }
  }, []);

  const state = useMemo<WorkbenchState>(
    () => ({ all, current, max, loading, error, open, create, close, rename, refresh: load }),
    [all, current, max, loading, error, open, create, close, rename, load],
  );

  return <WorkbenchContext.Provider value={state}>{children}</WorkbenchContext.Provider>;
}

/**
 * The bar above the Code pane: which operator is on screen, and the others.
 *
 * A row of workbenches rather than a dropdown, because the whole point is that
 * more than one is open — a control that hides the others behind a click makes two
 * operators feel like one with a setting. It renders nothing at all when there is
 * a single workbench and nothing has been named: there is no choice to present,
 * and a chrome bar over a pane with one entry is noise.
 */
export function WorkbenchBar({ onSwitched }: { onSwitched?: () => void }) {
  const { all, current, max, create, close, error } = useWorkbenches();

  if (all.length === 0) return null;
  if (all.length === 1 && all[0].link.full_name === "") return null;

  const full = max > 0 && all.length >= max;

  return (
    <div className="workbench-bar">
      {/*
        Tabs, because that is what these are: one of them is the workbench every
        file tool and every cell currently acts in, and picking another switches
        the whole working context. They were a row of chips, which said "filter"
        or "label" rather than "you are in exactly one of these".

        There is no `TabsContent` — the panel is the rest of the application, not
        a sibling of the list — so the tabs are a control over the router rather
        than over local state, and `value` comes from the workbench that is open.
      */}
      <Tabs
        className="min-w-0"
        value={current?.id ?? ""}
        onValueChange={(value) => {
          if (typeof value !== "string" || value === "" || value === current?.id) return;
          switchWorkbench(value);
          onSwitched?.();
        }}
      >
        <TabsList variant="line" className="workbench-list flex-wrap">
          {all.map((bench) => (
            <TabsTrigger key={bench.id} value={bench.id} className="workbench max-w-56">
              <span className="truncate">{workbenchLabel(bench)}</span>
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      {/*
        Closing acts on the workbench that is open, and sits outside the tab strip
        rather than as a cross on the tab itself — a button inside a tab is a
        button inside a button, which no browser agrees about. A cross per tab
        would also be how a developer closes the wrong operator's workbench: the
        checkout survives, but whatever the assistant was told about it does not.
      */}
      {current && all.length > 1 && (
        <Button
          variant="ghost"
          size="icon-xs"
          className="workbench-close"
          title="Close this workbench. The checkout stays in your workspace."
          aria-label={`Close ${workbenchLabel(current)}`}
          onClick={() => void close(current.id)}
        >
          <XIcon />
        </Button>
      )}
      <Button variant="ghost" size="sm"
        className="workbench-new ml-auto"
        disabled={full}
        title={
          full
            ? `This deployment allows ${max} workbenches at once. Each one is a kernel in your pod.`
            : "Open another workbench, to work on a second operator"
        }
        onClick={() => void create()}
      >
        New workbench
      </Button>
      {error && <span className="error workbench-error text-destructive">{error}</span>}
    </div>
  );
}
