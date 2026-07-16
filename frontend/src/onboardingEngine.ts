// Onboarding engine — a pure module (no React), mirroring logic.ts. It owns the
// data-driven node model (plan §4): the node config, and the eligibility / trigger
// / completion derivation. The overlay (Onboarding.tsx) and the hook
// (useOnboarding.ts) consume these; WorkspaceApp supplies the live context.
//
// Completion is DERIVED from live state wherever the data exists (plan §6.1) — we
// never show "create a document" incomplete when a document exists. localStorage
// holds only the non-derivable seen/dismissed flags, delivered here as recorded
// `events`. The stable event keys (§4.2) are a contract — never `step_N`.

// ---- Event keys (stable contract, plan §4.2) ---------------------------------

export const ONBOARDING_EVENT_KEYS = [
  "account_intro_seen",
  "workspace_created",
  "first_document_created",
  "first_document_edited",
  "first_thread_created",
  "first_thread_replied",
  "local_environment_connected",
  "first_agent_created",
  "first_agent_run_started",
  "first_document_watcher_added",
  "member_invited",
] as const;

export type OnboardingEventKey = (typeof ONBOARDING_EVENT_KEYS)[number];

// ---- Schema (plan §4.1) ------------------------------------------------------

export type OnboardingScope = "account" | "workspace";

// The user role a node is scoped to. Empty `eligibleRoles` means "all roles" — the
// node is removed (not disabled) for anyone outside a non-empty list. `eligibleRoles`
// is AUTHZ-PURE (who the backend permits to perform the node's action) — never a
// presentation-variant switch. The #57 config↔authz guard depends on that meaning.
export type OnboardingRole = "owner" | "admin" | "member";

// Which role's VARIANT of a chapter card this is — a presentation dimension, distinct
// from `eligibleRoles` (authz). The owner/admin work step and the member work card
// both resolve to start-run (all-roles authz) but are different surfaces; `audience`
// picks between them. A deliberately different shape from OnboardingRole[] so the
// config never reads as if it were authz. `undefined` = not a variant (any audience).
export type OnboardingAudience = "owner-admin" | "member";

export type OnboardingTrigger =
  | { type: "workspace-empty" } // no documents yet
  | { type: "document-open" } // a document view is active (its spotlight target exists)
  | { type: "first-text-select" } // the editor has a live text selection
  | { type: "manual" }; // never auto-triggers — shown only when its flow is opened (chapter cards)

// A completion condition. `derived` reads live signals (the source of truth);
// `flag` reads a recorded seen/dismissed/event key; `any` is satisfied if any child
// is (e.g. "dismissed OR a thread exists").
export type OnboardingCondition =
  | { via: "derived"; signal: DerivedSignal }
  | { via: "flag"; key: string }
  | { via: "acknowledge" } // completed once the node's VERSIONED seen flag is recorded
  | { via: "any"; of: OnboardingCondition[] };

export type DerivedSignal =
  | "document-exists"
  | "thread-exists"
  | "live-environment"
  | "agent-exists"
  | "agent-at-work";

// The UI actions a node's buttons can fire (distinct from the §4.2 completion event
// keys). A closed union so a typo dies at compile time and OnboardingNode stays
// structurally assignable to P2's OnboardingStep by construction — no cast/parser.
// The three `open-*` events route the "Add an AI teammate" chapter CTAs to real
// controls (CreateDaemonModal / CreateAgentModal / the shared agent-work destination).
export type OnboardingActionEvent =
  | "advance"
  | "back"
  | "complete"
  | "dismiss"
  | "open-thread-draft"
  | "open-create-environment"
  | "open-create-agent"
  | "open-agent-work";

export type OnboardingAction = { label: string; event: OnboardingActionEvent };

// `spotlight` nodes form the guided sequence (step counter); `tip` nodes are
// standalone contextual callouts (no counter, one-time); `chapter` nodes form the
// optional, non-blocking "Add an AI teammate" flow (its own step sequence + a
// terminal done card), never auto-triggered — opened deliberately by the user.
export type OnboardingPresentation = "spotlight" | "tip" | "chapter";

export type OnboardingNode = {
  id: string;
  version: number; // bump to re-show after a redesign
  scope: OnboardingScope;
  presentation: OnboardingPresentation;
  eligibleRoles: OnboardingRole[]; // empty = all — AUTHZ-PURE (see OnboardingRole)
  // Presentation variant (which role's version of a chapter card) — NOT authz. Chapter
  // cards use this to pick owner/admin vs member surfaces without lying via eligibleRoles.
  audience?: OnboardingAudience;
  trigger: OnboardingTrigger;
  // Extra live-signal preconditions ANDed into the trigger — the node is inert until
  // ALL are true (e.g. the watchers hint needs an agent to exist; the member chapter
  // card needs an agent to exist). A real gate on live state, not a fake trigger type.
  requiresSignals?: DerivedSignal[];
  completion: OnboardingCondition;
  // Marks the terminal "done" card of a chapter (shown once the chapter's steps are all
  // complete, gated by its requiresSignals) — not itself a step in the sequence.
  chapterTerminal?: boolean;
  targetOnboardingId?: string; // data-onboarding-id of the control to spotlight
  title: string;
  body: string;
  eyebrow?: string; // small label above the title (chapter cards: "ADD AN AI TEAMMATE · OPTIONAL")
  caption?: string; // reassurance line, not an action (e.g. member card "No setup needed")
  primaryAction?: OnboardingAction;
  secondaryAction?: OnboardingAction;
  skippable: boolean;
  fallback: "page-card" | "skip"; // when the target element is missing
};

export type OnboardingChecklistItem = {
  id: string;
  label: string;
  eligibleRoles: OnboardingRole[]; // empty = all — AUTHZ-PURE (see OnboardingRole)
  completion: OnboardingCondition;
  // The "Add an AI teammate" / "Work with an agent" entry is a resumable launcher for the
  // chapter (replace-not-coexist, #56) — these fields describe that entry row. Plain items
  // leave them unset. `audience` picks the owner/admin vs member variant (NOT authz);
  // `requiresSignals` hides the member entry until an agent exists; `opensChapter` marks
  // the row as a chapter launcher (subtitle + derived chapter progress + chevron).
  audience?: OnboardingAudience;
  requiresSignals?: DerivedSignal[];
  subtitle?: string;
  opensChapter?: boolean;
  // Historical copy shown on the entry ROW once complete (agent-at-work) — past tense,
  // still resumable. Falls back to label/subtitle when unset (e.g. the member entry).
  doneLabel?: string;
  doneSubtitle?: string;
};

// ---- Live context ------------------------------------------------------------

// Signals derived by the host (WorkspaceApp / useOnboarding) from live data. NOTE:
// `liveEnvironmentCount` MUST be derived via daemonLiveStatus (receipt-elapsed
// liveness), never a raw `status:'online'` field — a stale daemon that still reads
// online must not satisfy `local_environment_connected`.
export type OnboardingLiveSignals = {
  documentCount: number;
  threadCount: number; // any thread created (a reply implies a thread)
  liveEnvironmentCount: number; // daemons live by receipt-elapsed liveness only
  agentCount: number;
  agentRunCount: number;
  agentThreadCount: number; // threads an agent participates in
};

export type OnboardingContext = {
  roles: OnboardingRole[];
  events: ReadonlySet<string>; // recorded seen/dismissed/event flags (localStorage)
  route: string; // active view, e.g. "document"
  selectionActive: boolean; // editor has a live text selection
  signals: OnboardingLiveSignals;
};

// ---- Node config (authoritative copy from onboarding-nodes-spec.md — locked) --

const created = (key: OnboardingEventKey, signal: DerivedSignal): OnboardingCondition => ({
  via: "any",
  of: [
    { via: "derived", signal }, // live state is the source of truth
    { via: "flag", key }, // belt-and-suspenders: the recorded event
  ],
});

const ACKNOWLEDGE: OnboardingCondition = { via: "acknowledge" };

// The versioned seen flag the host records when a node is acknowledged (Next/Done)
// or dismissed. The version is embedded so bumping OnboardingNode.version re-shows
// the node after a redesign — a v1 flag no longer satisfies the v2 node.
export function acknowledgeFlag(node: OnboardingNode): string {
  return `seen:${node.id}@v${node.version}`;
}

export const NODES: OnboardingNode[] = [
  // A. Guided spotlight sequence — scope workspace, all roles, 2 steps every role can
  // complete (create a document, learn threads). Watchers left this sequence in #56 —
  // it now teaches as a contextual tip once an agent exists (see B).
  {
    id: "create-first-document",
    version: 1,
    scope: "workspace",
    presentation: "spotlight",
    eligibleRoles: [],
    trigger: { type: "workspace-empty" },
    completion: created("first_document_created", "document-exists"),
    targetOnboardingId: "create-document",
    title: "These are real files",
    body: "What you write syncs to your computer as an actual file — open the same document in the browser or your local editor.",
    primaryAction: { label: "Next", event: "advance" },
    skippable: true,
    fallback: "page-card",
  },
  {
    id: "threads-intro",
    version: 1,
    scope: "workspace",
    presentation: "spotlight",
    eligibleRoles: [],
    trigger: { type: "document-open" },
    completion: ACKNOWLEDGE,
    targetOnboardingId: "document-threads",
    title: "Every discussion has a home",
    body: "Select any text to open a thread anchored to it. Discussion stays out of the document — open threads here anytime.",
    primaryAction: { label: "Next", event: "advance" },
    secondaryAction: { label: "Back", event: "back" },
    skippable: true,
    fallback: "skip",
  },
  // B. Contextual tips — standalone callouts, not in the guided sequence.
  // watchers-intro RELOCATED here from the guided sequence (#56): SAME id + version 1,
  // so the existing `seen:watchers-intro@v1` ack carries — users who saw the old
  // spotlight step 3/3 never see it again (zero migration). Only presentation (tip),
  // trigger (document-open AND agent-exists), and sequence membership changed. Watchers
  // only make sense once an agent exists — hence the agent-exists precondition.
  {
    id: "watchers-intro",
    version: 1,
    scope: "workspace",
    presentation: "tip",
    eligibleRoles: [],
    trigger: { type: "document-open" },
    requiresSignals: ["agent-exists"],
    completion: ACKNOWLEDGE,
    targetOnboardingId: "document-watchers",
    title: "Let an agent keep watch",
    body: "Watchers are the agents following this document. When something relevant changes, it lands in their work queue — no need to ping them.",
    primaryAction: { label: "Got it", event: "dismiss" },
    skippable: true,
    fallback: "skip",
  },
  {
    id: "tip-first-selection",
    version: 1,
    scope: "account",
    presentation: "tip",
    eligibleRoles: [],
    trigger: { type: "first-text-select" },
    completion: {
      via: "any",
      of: [
        ACKNOWLEDGE, // dismissed with "Got it" (versioned)
        // Account-durable: recorded at ACCOUNT scope on thread creation, so once the
        // user has started a thread anywhere the tip never nags again after a switch
        // (plan §4.3 `has-used-thread` is an account-level fact). This is the account
        // leg that makes an account-scoped node honestly account-complete.
        { via: "flag", key: "first_thread_created" },
        // Convenience leg for workspaces that already had a thread before this shipped.
        { via: "derived", signal: "thread-exists" },
      ],
    },
    targetOnboardingId: "selection-thread",
    title: "Talk about this exact line",
    body: "Select any text to open a thread anchored right here — no need to write feedback into the document.",
    primaryAction: { label: "Start thread", event: "open-thread-draft" },
    secondaryAction: { label: "Got it", event: "dismiss" },
    skippable: true,
    fallback: "skip",
  },
  // D. "Add an AI teammate" chapter — optional, non-blocking, resumable. Trigger is
  // `manual`: never auto-spotlighted, opened deliberately (after the core guide / from
  // the checklist). Each card renders as a quiet page card (no spotlight target). The
  // owner/admin path is a 3-step sequence (connect → create → work) that advances on
  // live signals + a terminal "done" card; a member gets a single "work with an agent"
  // card only once an agent exists. Completion is live-derived (agent-at-work) — no new
  // stored flag.
  //
  // eligibleRoles vs audience (ruled by Anton/Tom/Juan, #56): `eligibleRoles` is
  // AUTHZ-PURE — connect/create carry ["owner","admin"] because the backend genuinely
  // gates them (ManageDaemons/ManageAgents); the work step, done card, and member card
  // carry [] because their action (start-run / close) is all-roles. The owner-vs-member
  // presentation split lives in `audience`, NOT eligibleRoles, so the #57 authz guard
  // stays strict and honest everywhere (it keys on the action→permission table, and
  // never sees a variant switch smuggled into eligibleRoles).
  {
    id: "add-teammate-connect",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eligibleRoles: ["owner", "admin"], // authz: ManageDaemons
    audience: "owner-admin",
    trigger: { type: "manual" },
    completion: { via: "derived", signal: "live-environment" },
    eyebrow: "ADD AN AI TEAMMATE · OPTIONAL",
    title: "Connect a local environment",
    body: "A local environment is the computer where your agents run.",
    primaryAction: { label: "Connect environment", event: "open-create-environment" },
    secondaryAction: { label: "Not now", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  },
  {
    id: "add-teammate-create",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eligibleRoles: ["owner", "admin"], // authz: ManageAgents
    audience: "owner-admin",
    trigger: { type: "manual" },
    completion: { via: "derived", signal: "agent-exists" },
    eyebrow: "ADD AN AI TEAMMATE · OPTIONAL",
    title: "Create your first agent",
    body: "An agent has a name, a role, and its own runtime.",
    primaryAction: { label: "Create agent", event: "open-create-agent" },
    secondaryAction: { label: "Not now", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  },
  {
    id: "add-teammate-work",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eligibleRoles: [], // authz: start-run is all-roles — owner/admin see this variant via `audience`
    audience: "owner-admin",
    trigger: { type: "manual" },
    completion: { via: "derived", signal: "agent-at-work" },
    eyebrow: "ADD AN AI TEAMMATE · OPTIONAL",
    title: "Put your agent to work",
    body: "Choose an agent and start a run.",
    // The label is DYNAMIC (Anton): the host relabels via resolveAgentWorkDestination —
    // exactly 1 agent → "Start a run" (direct to its Start run); 2+ → "Choose an agent"
    // (Agents list). The static label here is the 1-agent default and must not ship as-is.
    primaryAction: { label: "Start a run", event: "open-agent-work" },
    secondaryAction: { label: "Not now", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  },
  {
    id: "add-teammate-done",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eligibleRoles: [], // authz: closing a done card is universal — audience picks the owner/admin variant
    audience: "owner-admin",
    trigger: { type: "manual" },
    requiresSignals: ["agent-at-work"],
    chapterTerminal: true,
    completion: { via: "derived", signal: "agent-at-work" },
    eyebrow: "STARTED",
    title: "You've started working with an agent",
    body: "You can start more work anytime from Agents.",
    caption: "Chapter complete",
    primaryAction: { label: "Close", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  },
  {
    id: "add-teammate-member",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eligibleRoles: [], // authz: start-run is all-roles — the member variant is chosen via `audience`
    audience: "member",
    trigger: { type: "manual" },
    requiresSignals: ["agent-exists"],
    completion: { via: "derived", signal: "agent-at-work" },
    eyebrow: "WORK WITH AN AGENT",
    title: "Put an agent to work",
    body: "This workspace has agents. Choose one and start a run.",
    caption: "No setup needed", // reassurance, not an action (Anton: caption, not a dismiss label)
    // Dynamic label, same rule as the owner/admin work step (host relabels via
    // resolveAgentWorkDestination: 1 agent → "Start a run", 2+ → "Choose an agent").
    primaryAction: { label: "Start a run", event: "open-agent-work" },
    secondaryAction: { label: "Not now", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  },
  // Member terminal card (#56, Anton's behavioral-gate fix): once a member has put an
  // agent to work, reopening the entry must land on a real completion card — not nothing.
  // Mirrors the owner/admin done card for the member audience; live-derived (agent-at-work).
  {
    id: "add-teammate-member-done",
    version: 1,
    scope: "workspace",
    presentation: "chapter",
    eligibleRoles: [],
    audience: "member",
    trigger: { type: "manual" },
    requiresSignals: ["agent-at-work"],
    chapterTerminal: true,
    completion: { via: "derived", signal: "agent-at-work" },
    eyebrow: "STARTED",
    title: "You've started working with an agent",
    body: "You can start more work anytime from Agents.",
    caption: "Chapter complete", // mirror the owner/admin terminal footer (Anton parity note)
    primaryAction: { label: "Close", event: "dismiss" },
    skippable: true,
    fallback: "page-card",
  },
];

// C. Getting-started checklist — scope workspace. Completion is derived from live
// data (never a stored "done") for every item EXCEPT `invite-team`: an invitation
// sent is not derivable from the member list until the invitee accepts, so deriving
// it would falsely read incomplete. It is the one flag-based item, by design.
//
// #56 replace-not-coexist: the old connect-environment / create-agent / agent-at-work
// rows are GONE — they collapse into ONE resumable chapter entry that COUNTS in the
// progress denominator (Anton) and opens the chapter. Owner/admin get the "Add an AI
// teammate" variant; a member with agents gets "Work with an agent"; a member with no
// agents gets no AI entry (requiresSignals hides it). Both complete from agent-at-work.
export const CHECKLIST_ITEMS: OnboardingChecklistItem[] = [
  { id: "create-document", label: "Create your first document", eligibleRoles: [], completion: { via: "derived", signal: "document-exists" } },
  { id: "start-discussion", label: "Start a discussion", eligibleRoles: [], completion: { via: "derived", signal: "thread-exists" } },
  {
    id: "add-teammate-entry",
    label: "Add an AI teammate",
    eligibleRoles: [], // authz-pure: opening the chapter is not gated (its STEPS are); variant via audience
    audience: "owner-admin",
    completion: { via: "derived", signal: "agent-at-work" },
    subtitle: "Connect an environment, create an agent, put it to work",
    opensChapter: true,
    doneLabel: "You've started working with an agent",
    doneSubtitle: "Reopen to add more or hand over new work",
  },
  {
    id: "work-with-agent-entry",
    label: "Work with an agent",
    eligibleRoles: [],
    audience: "member",
    requiresSignals: ["agent-exists"], // member sees this only once an agent exists — else no AI entry
    completion: { via: "derived", signal: "agent-at-work" },
    subtitle: "Choose an agent and start a run",
    opensChapter: true,
    doneLabel: "You've started working with an agent",
    doneSubtitle: "Reopen to start more work",
  },
  { id: "invite-team", label: "Invite your team", eligibleRoles: ["owner", "admin"], completion: { via: "flag", key: "member_invited" } },
];

// ---- Derivation --------------------------------------------------------------

function derivedSignal(signal: DerivedSignal, s: OnboardingLiveSignals): boolean {
  switch (signal) {
    case "document-exists":
      return s.documentCount > 0;
    case "thread-exists":
      return s.threadCount > 0;
    case "live-environment":
      return s.liveEnvironmentCount > 0;
    case "agent-exists":
      return s.agentCount > 0;
    case "agent-at-work":
      // Ratified #56 oracle: a run started OR an agent in a thread. NOT "watching" — a
      // watcher hasn't done work, so it must not mark the chapter Done (that was a Batch-1
      // definition; folded out here). If "agent is watching" is ever wanted it needs its
      // own named signal + product ruling — one signal, one meaning.
      return s.agentRunCount > 0 || s.agentThreadCount > 0;
  }
}

function isSatisfied(
  cond: OnboardingCondition,
  ctx: OnboardingContext,
  node: OnboardingNode | OnboardingChecklistItem,
): boolean {
  switch (cond.via) {
    case "flag":
      return ctx.events.has(cond.key);
    case "acknowledge":
      // Only nodes (which carry a version) use acknowledge; checklist items never do.
      return "version" in node && ctx.events.has(acknowledgeFlag(node));
    case "derived":
      return derivedSignal(cond.signal, ctx.signals);
    case "any":
      return cond.of.some((c) => isSatisfied(c, ctx, node));
  }
}

function roleEligible(eligibleRoles: OnboardingRole[], roles: OnboardingRole[]): boolean {
  return eligibleRoles.length === 0 || eligibleRoles.some((r) => roles.includes(r));
}

// Every live-signal precondition is met (empty = trivially true). Works for nodes and
// for the chapter-entry checklist item (both may carry requiresSignals).
function requiresSignalsMet(item: { requiresSignals?: DerivedSignal[] }, ctx: OnboardingContext): boolean {
  return (item.requiresSignals ?? []).every((sig) => derivedSignal(sig, ctx.signals));
}

function isTriggered(node: OnboardingNode, ctx: OnboardingContext): boolean {
  if (!requiresSignalsMet(node, ctx)) return false;
  switch (node.trigger.type) {
    case "workspace-empty":
      return ctx.signals.documentCount === 0;
    case "document-open":
      return ctx.route === "document";
    case "first-text-select":
      return ctx.selectionActive;
    case "manual":
      // Chapter cards never auto-trigger — they surface only through the chapter API
      // (activeChapter) when their flow is opened, not via activeNode/activeTip.
      return false;
  }
}

// ---- Public API (plan §5.1) --------------------------------------------------

// Nodes the user's role can ever see (removed, not disabled, for others).
export function eligibleNodes(ctx: OnboardingContext): OnboardingNode[] {
  return NODES.filter((n) => roleEligible(n.eligibleRoles, ctx.roles));
}

// The ordered guided-spotlight sequence for this role.
export function guideSteps(ctx: OnboardingContext): OnboardingNode[] {
  return eligibleNodes(ctx).filter((n) => n.presentation === "spotlight");
}

export function isComplete(node: OnboardingNode | OnboardingChecklistItem, ctx: OnboardingContext): boolean {
  return isSatisfied(node.completion, ctx, node);
}

// The next incomplete step in the guided sequence (drives the step counter), or
// null when the guide is finished. Does not consider triggers — it is "what's left".
export function nextIncomplete(ctx: OnboardingContext): OnboardingNode | null {
  return guideSteps(ctx).find((n) => !isComplete(n, ctx)) ?? null;
}

// The spotlight step to show right now: the first incomplete step, but only once
// its own trigger is met — so we wait (return null) for the document to open rather
// than skipping past step 2. Never jumps to a later step over an earlier incomplete
// one (the sequence is ordered).
export function activeNode(ctx: OnboardingContext): OnboardingNode | null {
  const step = nextIncomplete(ctx);
  if (!step) return null;
  return isTriggered(step, ctx) ? step : null;
}

// The standalone contextual tip to show, if any (independent of the guided
// sequence): the first eligible tip that is triggered and not yet complete.
export function activeTip(ctx: OnboardingContext): OnboardingNode | null {
  return (
    eligibleNodes(ctx).find((n) => n.presentation === "tip" && isTriggered(n, ctx) && !isComplete(n, ctx)) ?? null
  );
}

// The checklist for this role, each item with its live-derived done state. Filtered by
// authz (eligibleRoles), presentation audience (owner/admin vs member entry variant),
// and signal preconditions (the member entry appears only once an agent exists). The
// chapter entry COUNTS as an item (Anton) — so a member with agents sees 3 items, a
// member with none sees 2, and owner/admin see 4.
export function checklistProgress(ctx: OnboardingContext): Array<{ item: OnboardingChecklistItem; done: boolean }> {
  return CHECKLIST_ITEMS.filter(
    (i) => roleEligible(i.eligibleRoles, ctx.roles) && audienceMatches(i, ctx) && requiresSignalsMet(i, ctx),
  ).map((item) => ({
    item,
    done: isComplete(item, ctx),
  }));
}

// ---- "Add an AI teammate" chapter (#56) --------------------------------------
// The chapter is opt-in and non-blocking: it is NEVER surfaced by activeNode/activeTip
// (its cards carry a `manual` trigger). The host opens it deliberately — after the core
// guide or from the checklist — and asks the engine which card to show for the current
// live state. All completion is live-derived (no new stored flag).

// Whether a card/entry's presentation VARIANT is meant for this role (distinct from
// authz eligibility). `undefined` audience = any. Permission is still enforced upstream
// by eligibleRoles; this only picks owner/admin-vs-member surfaces. Works for chapter
// nodes and the chapter-entry checklist item.
function audienceMatches(item: { audience?: OnboardingAudience }, ctx: OnboardingContext): boolean {
  switch (item.audience) {
    case undefined:
      return true;
    case "owner-admin":
      return ctx.roles.includes("owner") || ctx.roles.includes("admin");
    case "member":
      return ctx.roles.includes("member");
  }
}

// The ordered chapter STEPS for this role (excludes the terminal done card). Owner/admin
// see connect → create → work; a member sees only the single "work with an agent" card.
// Filtered by authz (eligibleNodes) AND presentation audience.
export function chapterSteps(ctx: OnboardingContext): OnboardingNode[] {
  return eligibleNodes(ctx).filter(
    (n) => n.presentation === "chapter" && !n.chapterTerminal && audienceMatches(n, ctx),
  );
}

// The terminal done card for this role, if the chapter defines one (owner/admin only).
function chapterTerminalNode(ctx: OnboardingContext): OnboardingNode | null {
  return (
    eligibleNodes(ctx).find(
      (n) => n.presentation === "chapter" && n.chapterTerminal === true && audienceMatches(n, ctx),
    ) ?? null
  );
}

// The next incomplete chapter step whose signal preconditions hold — the user's true
// next action. Ordered: an earlier incomplete step is never skipped for a later one.
// For a member the single card is inert until an agent exists (requiresSignals), so
// this returns null for a member with no agents.
export function nextChapterStep(ctx: OnboardingContext): OnboardingNode | null {
  return chapterSteps(ctx).find((n) => !isComplete(n, ctx) && requiresSignalsMet(n, ctx)) ?? null;
}

// The chapter card to show when the chapter is open: the next incomplete step, or the
// terminal done card once every step is complete (and its preconditions hold), or null
// when there is nothing to show (a member with no agents; a member who has already
// worked with an agent). A pure function of role + live state — the host owns WHEN the
// chapter is open, the engine owns WHICH card.
export function activeChapter(ctx: OnboardingContext): OnboardingNode | null {
  // History-first (Juan/Anton/Deniz): once the chapter is complete — its terminal's
  // preconditions hold, i.e. an agent was put to work — reopen the DONE card even if setup
  // signals later regress (environment offline, an agent removed). Activation history
  // doesn't un-happen because a live signal decayed; the chapter is history, not a health
  // dashboard. So the terminal is checked BEFORE the setup frontier.
  const done = chapterTerminalNode(ctx);
  if (done && requiresSignalsMet(done, ctx)) return done;
  return nextChapterStep(ctx) ?? null;
}

// Whether this role has any chapter card to show right now — drives whether an entry
// point (e.g. a checklist row) is offered. Owner/admin always do (setup → done); a
// member only once an agent exists and before they've put one to work.
export function hasChapter(ctx: OnboardingContext): boolean {
  return activeChapter(ctx) !== null;
}
