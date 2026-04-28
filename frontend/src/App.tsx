import { useDeferredValue, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import * as Y from "yjs";
import * as syncProtocol from "y-protocols/sync.js";
import { Awareness, applyAwarenessUpdate, encodeAwarenessUpdate } from "y-protocols/awareness.js";
import * as encoding from "lib0/encoding";
import * as decoding from "lib0/decoding";
import {
  buildLineThreads,
  computeReplace,
  isFreshPresence,
  rebaseReplace,
  rebaseSelection,
  resolveWorkspaceLoad,
  summarizeAgentStatus,
} from "./app_logic";

type DocumentItem = {
  id: string;
  path: string;
  title: string;
  stateVector?: string;
  updateId?: number;
  updatedAt: string;
  clientIdSeed?: number;
};

type ThreadAnchor = {
  documentId: string;
  kind: string;
  relativeStart?: string;
  relativeEnd?: string;
  start: number;
  end: number;
  line: number;
  excerpt: string;
};

type ThreadMessage = {
  id: string;
  threadId: string;
  authorId: string;
  authorType: string;
  authorHandle: string;
  authorName: string;
  body: string;
  kind: string;
  createdAt: string;
};

type ThreadItem = {
  id: string;
  documentId: string;
  title: string;
  status: string;
  anchor: ThreadAnchor;
  participantIds: string[];
  participantHandles: string[];
  messages: ThreadMessage[];
  createdAt: string;
  updatedAt: string;
};

type UserItem = {
  id: string;
  handle: string;
  name: string;
  role: string;
  kind: string;
  status: string;
  updatedAt: string;
};

type Proposal = {
  id: string;
  documentId: string;
  title: string;
  author: string;
  proposedText: string;
  status: string;
  createdAt: string;
};

type Agent = {
  id: string;
  handle: string;
  name: string;
  role: string;
  kind: string;
  systemPrompt: string;
  workspaceRoot: string;
  status: string;
  currentTask: string;
  currentActivity: string;
  currentRunId: string;
  lastHeartbeatAt?: string;
};

type AgentRun = {
  id: string;
  agentId: string;
  agentHandle: string;
  agentName: string;
  agentKind: string;
  workingDirectory: string;
  prompt: string;
  status: string;
  desiredStatus: string;
  processId: number;
  lastHeartbeatAt?: string;
  lastMessage?: string;
  logTail?: string[];
  error?: string;
  updatedAt: string;
};

type PresenceItem = {
  actorId: string;
  actorType: string;
  documentId?: string;
  filePath?: string;
  mode?: string;
  selection?: number[];
  activity: string;
  updatedAt: string;
};

type Workspace = {
  name: string;
  documents: DocumentItem[];
  users: UserItem[];
  agents: Agent[];
  agentRuns: AgentRun[];
  presences: Record<string, PresenceItem>;
  threads: ThreadItem[];
  proposals: Record<string, Proposal>;
};

type DocumentUpdateEvent = {
  documentId: string;
  update: string;
  path: string;
  updatedAt: string;
  actorId: string;
};

type UserDraft = {
  name: string;
  handle: string;
  role: string;
};

type AgentDraft = {
  handle: string;
  name: string;
  role: string;
  taskPrompt: string;
};

type PresenceView = {
  actorId: string;
  actorName: string;
  activity: string;
};

const apiBase = import.meta.env.VITE_API_BASE ?? "http://localhost:8080";
const currentUserStorageKey = "notty-current-user";

export function App() {
  const [workspace, setWorkspace] = useState<Workspace | null>(null);
  const [workspaceConnected, setWorkspaceConnected] = useState(false);
  const [selectedId, setSelectedId] = useState<string>("");
  const [activeDocument, setActiveDocument] = useState<DocumentItem | null>(null);
  const [documentReady, setDocumentReady] = useState(false);
  const [draft, setDraft] = useState("");
  const [message, setMessage] = useState("");
  const [newFilePath, setNewFilePath] = useState("notes/untitled.md");
  const [pathEditor, setPathEditor] = useState("");
  const [proposalTitle, setProposalTitle] = useState("Escalated change");
  const [threadBody, setThreadBody] = useState("");
  const [threadReplyBody, setThreadReplyBody] = useState("");
  const [newUserName, setNewUserName] = useState("Workspace Collaborator");
  const [newUserHandle, setNewUserHandle] = useState("collaborator");
  const [newUserRole, setNewUserRole] = useState("Collaborates in the shared workspace");
  const [userDrafts, setUserDrafts] = useState<Record<string, UserDraft>>({});
  const [currentUserId, setCurrentUserId] = useState<string>(() => localStorage.getItem(currentUserStorageKey) ?? "");
  const [newAgentHandle, setNewAgentHandle] = useState("codex-agent");
  const [newAgentName, setNewAgentName] = useState("Codex Agent");
  const [newAgentRole, setNewAgentRole] = useState("Implement changes in the shared workspace");
  const [agentDrafts, setAgentDrafts] = useState<Record<string, AgentDraft>>({});
  const [isAgentModalOpen, setIsAgentModalOpen] = useState(false);
  const [agentDetailsId, setAgentDetailsId] = useState<string>("");
  const [selectedThreadId, setSelectedThreadId] = useState<string>("");
  const [editorScrollTop, setEditorScrollTop] = useState(0);
  const [selectionRange, setSelectionRange] = useState({ start: 0, end: 0 });
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const activeDocRef = useRef<{ id: string; ydoc: Y.Doc } | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const workspaceWsRef = useRef<WebSocket | null>(null);
  const awarenessRef = useRef<Awareness | null>(null);
  const selectedIdRef = useRef("");
  const workspaceLoadTokenRef = useRef(0);
  const workspaceReloadTimerRef = useRef<number | null>(null);
  const draftRef = useRef("");
  const selectionRangeRef = useRef({ start: 0, end: 0 });
  const currentUserRef = useRef<UserItem | null>(null);
  const selectedDocumentRef = useRef<DocumentItem | null>(null);
  const pendingPresenceTimerRef = useRef<number | null>(null);
  const lastPresenceSentAtRef = useRef(0);
  const pendingSelectionRef = useRef<{ start: number; end: number } | null>(null);
  const isComposingRef = useRef(false);
  const hasQueuedRemoteSyncRef = useRef(false);
  const remoteDraftSyncTimerRef = useRef<number | null>(null);

  useEffect(() => {
    void loadWorkspace();
    return () => {
      wsRef.current?.close();
      workspaceWsRef.current?.close();
      if (workspaceReloadTimerRef.current !== null) {
        window.clearTimeout(workspaceReloadTimerRef.current);
      }
      if (pendingPresenceTimerRef.current !== null) {
        window.clearTimeout(pendingPresenceTimerRef.current);
      }
      if (remoteDraftSyncTimerRef.current !== null) {
        window.clearTimeout(remoteDraftSyncTimerRef.current);
      }
      awarenessRef.current?.destroy();
      activeDocRef.current?.ydoc.destroy();
      activeDocRef.current = null;
    };
  }, []);

  useEffect(() => {
    selectedIdRef.current = selectedId;
  }, [selectedId]);

  useEffect(() => {
    draftRef.current = draft;
  }, [draft]);

  useEffect(() => {
    selectionRangeRef.current = selectionRange;
  }, [selectionRange]);

  useLayoutEffect(() => {
    const pendingSelection = pendingSelectionRef.current;
    if (!pendingSelection || !textareaRef.current) {
      return;
    }
    if (document.activeElement !== textareaRef.current) {
      pendingSelectionRef.current = null;
      return;
    }
    textareaRef.current.selectionStart = pendingSelection.start;
    textareaRef.current.selectionEnd = pendingSelection.end;
    pendingSelectionRef.current = null;
  }, [draft]);

  useEffect(() => {
    let disposed = false;
    let reconnectTimer: number | null = null;
    let attempt = 0;

    const scheduleWorkspaceReload = () => {
      if (workspaceReloadTimerRef.current !== null) {
        return;
      }
      workspaceReloadTimerRef.current = window.setTimeout(() => {
        workspaceReloadTimerRef.current = null;
        void loadWorkspace();
      }, 120);
    };

    const connect = () => {
      if (disposed) {
        return;
      }
      const ws = new WebSocket(`${apiBase.replace("http", "ws")}/ws`);
      workspaceWsRef.current = ws;
      ws.onopen = () => {
        attempt = 0;
        setWorkspaceConnected(true);
        void loadWorkspace();
      };
      ws.onmessage = (event) => {
        const envelope = JSON.parse(String(event.data)) as { type?: string; data?: unknown };
        if (envelope.type === "presence.updated") {
          const presence = envelope.data as PresenceItem;
          setWorkspace((current) =>
            current
              ? {
                  ...current,
                  presences: {
                    ...current.presences,
                    [presence.actorId]: presence,
                  },
                }
              : current
          );
          return;
        }
        if (envelope.type === "agent.updated") {
          const agent = envelope.data as Agent;
          setWorkspace((current) =>
            current
              ? {
                  ...current,
                  agents: current.agents.some((currentAgent) => currentAgent.id === agent.id)
                    ? current.agents.map((currentAgent) => (currentAgent.id === agent.id ? agent : currentAgent))
                    : [...current.agents, agent],
                }
              : current
          );
          return;
        }
        if (envelope.type === "agent.run.updated") {
          const run = envelope.data as AgentRun;
          setWorkspace((current) =>
            current
              ? {
                  ...current,
                  agentRuns: current.agentRuns.some((currentRun) => currentRun.id === run.id)
                    ? current.agentRuns.map((currentRun) => (currentRun.id === run.id ? run : currentRun))
                    : [...current.agentRuns, run],
                }
              : current
          );
          return;
        }
        if (envelope.type === "document.updated") {
          const documentEvent = envelope.data as DocumentUpdateEvent;
          setWorkspace((current) =>
            current
              ? {
                  ...current,
                  documents: current.documents.map((currentDoc) =>
                    currentDoc.id === documentEvent.documentId
                      ? {
                          ...currentDoc,
                          path: documentEvent.path,
                          updatedAt: documentEvent.updatedAt,
                        }
                      : currentDoc
                  ),
                }
              : current
          );
          setActiveDocument((current) =>
            current?.id === documentEvent.documentId
              ? { ...current, path: documentEvent.path, updatedAt: documentEvent.updatedAt }
              : current
          );
          return;
        }
        if (
          envelope.type === "workspace.snapshot" ||
          envelope.type === "document.created" ||
          envelope.type === "document.moved" ||
          envelope.type === "document.deleted" ||
          envelope.type === "thread.created" ||
          envelope.type === "thread.updated" ||
          envelope.type === "thread.message.created" ||
          envelope.type === "user.created" ||
          envelope.type === "user.updated" ||
          envelope.type === "user.deleted" ||
          envelope.type === "agent.created" ||
          envelope.type === "agent.deleted"
        ) {
          scheduleWorkspaceReload();
        }
      };
      ws.onerror = () => {
        ws.close();
      };
      ws.onclose = () => {
        if (workspaceWsRef.current === ws) {
          workspaceWsRef.current = null;
        }
        setWorkspaceConnected(false);
        if (disposed) {
          return;
        }
        const delay = Math.min(1000 * 2 ** attempt, 5000);
        attempt += 1;
        reconnectTimer = window.setTimeout(connect, delay);
      };
    };

    connect();
    return () => {
      disposed = true;
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer);
      }
      if (workspaceReloadTimerRef.current !== null) {
        window.clearTimeout(workspaceReloadTimerRef.current);
        workspaceReloadTimerRef.current = null;
      }
      workspaceWsRef.current?.close();
    };
  }, []);

  const selectedDocumentMeta = useMemo(
    () => workspace?.documents.find((doc) => doc.id === selectedId) ?? null,
    [selectedId, workspace]
  );
  const selectedDocument = activeDocument?.id === selectedId ? activeDocument : null;
  const selectedDocumentView = selectedDocument ?? selectedDocumentMeta;
  const deferredDraft = useDeferredValue(draft);
  const documentLines = useMemo(() => deferredDraft.split("\n"), [deferredDraft]);
  const lineStarts = useMemo(() => buildLineStarts(deferredDraft), [deferredDraft]);
  const currentUser = useMemo(
    () => workspace?.users.find((user) => user.id === currentUserId) ?? workspace?.users[0] ?? null,
    [currentUserId, workspace?.users]
  );
  const selectedThreads = workspace?.threads.filter((thread) => thread.documentId === selectedId) ?? [];
  const liveSelectedThreads = useMemo(() => {
    if (!selectedDocument) {
      return selectedThreads;
    }
    const ydoc = getOrCreateDoc(selectedDocument);
    return selectedThreads.map((thread) => ({
      ...thread,
      anchor: resolveThreadAnchorLive(thread.anchor, ydoc, deferredDraft, lineStarts),
    }));
  }, [deferredDraft, lineStarts, selectedDocument, selectedThreads]);
  const lineThreads = useMemo(
    () => buildLineThreads(liveSelectedThreads),
    [liveSelectedThreads]
  );
  const activeCollaborators = useMemo(
    () =>
      buildActiveCollaborators(
        workspace?.presences ?? {},
        workspace?.users ?? [],
        workspace?.agents ?? [],
        selectedDocumentView?.id ?? "",
        selectedDocumentView?.path ?? "",
        currentUser
          ? { actorId: currentUser.handle, actorName: currentUser.name, activity: "Editing this page" }
          : { actorId: "owner", actorName: "Workspace Owner", activity: "Editing this page" }
      ),
    [currentUser, selectedDocumentView?.id, selectedDocumentView?.path, workspace?.agents, workspace?.presences, workspace?.users]
  );
  const commentedLines = useMemo(
    () => new Set(lineThreads.map((thread) => thread.line)),
    [lineThreads]
  );
  const selectedThread = useMemo(
    () => (!selectedThreadId ? null : liveSelectedThreads.find((thread) => thread.id === selectedThreadId) ?? null),
    [liveSelectedThreads, selectedThreadId]
  );
  const selectedThreadGroup = useMemo(
    () =>
      !selectedThread
        ? null
        : lineThreads.find((threadGroup) => threadGroup.threads.some((thread) => thread.id === selectedThread.id)) ?? null,
    [lineThreads, selectedThread]
  );
  const currentSelectionLabel = useMemo(
    () => formatSelection(selectionRange.start, selectionRange.end, lineStarts),
    [lineStarts, selectionRange.end, selectionRange.start]
  );
  const wordCount = useMemo(() => countWords(deferredDraft), [deferredDraft]);
  const charCount = deferredDraft.length;
  const editorTrackHeight = Math.max(documentLines.length * EDITOR_LINE_HEIGHT + 120, 640);

  useEffect(() => {
    currentUserRef.current = currentUser;
  }, [currentUser]);

  useEffect(() => {
    selectedDocumentRef.current = selectedDocument;
  }, [selectedDocument]);

  useEffect(() => {
    setUserDrafts((current) => {
      const next: Record<string, UserDraft> = {};
      for (const user of workspace?.users ?? []) {
        const existing = current[user.id];
        next[user.id] = existing ?? {
          name: user.name,
          handle: user.handle,
          role: user.role,
        };
      }
      return next;
    });
  }, [workspace?.users]);

  useEffect(() => {
    setAgentDrafts((current) => {
      const next: Record<string, AgentDraft> = {};
      for (const agent of workspace?.agents ?? []) {
        const existing = current[agent.id];
        next[agent.id] = existing ?? {
          handle: agent.handle,
          name: agent.name,
          role: agent.role,
          taskPrompt: `Review the latest workspace changes and continue the assigned task as ${agent.name}.`,
        };
      }
      return next;
    });
  }, [workspace?.agents]);

  useEffect(() => {
    if (!selectedId && workspace?.documents.length) {
      selectedIdRef.current = workspace.documents[0].id;
      setSelectedId(workspace.documents[0].id);
    }
  }, [selectedId, workspace]);

  useEffect(() => {
    setDocumentReady(false);
    activeDocRef.current?.ydoc.destroy();
    activeDocRef.current = null;
    if (!selectedId || !selectedDocumentMeta) {
      setActiveDocument(null);
      resetEditorState();
      return;
    }
    setActiveDocument(selectedDocumentMeta);
    resetEditorState(selectedDocumentMeta.path);
  }, [selectedDocumentMeta?.id, selectedDocumentMeta?.path, selectedDocumentMeta?.title, selectedId]);

  useEffect(() => {
    if (!workspace?.users.length) {
      return;
    }
    if (!currentUserId || !workspace.users.some((user) => user.id === currentUserId)) {
      setCurrentUserId(workspace.users[0].id);
    }
  }, [currentUserId, workspace?.users]);

  useEffect(() => {
    if (currentUserId) {
      localStorage.setItem(currentUserStorageKey, currentUserId);
    }
  }, [currentUserId]);

  useEffect(() => {
    const publishPresence = async () => {
      const activeUser = currentUserRef.current;
      const activeDocument = selectedDocumentRef.current;
      if (!activeUser || !activeDocument) {
        return;
      }
      lastPresenceSentAtRef.current = Date.now();
      try {
        await fetch(`${apiBase}/api/presence`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            actorId: activeUser.handle,
            actorType: "human",
            documentId: activeDocument.id,
            filePath: activeDocument.path,
            mode: "editing",
            selection: [selectionRangeRef.current.start, selectionRangeRef.current.end],
            activity: `Editing ${activeDocument.title}`,
          }),
        });
      } catch {
        return;
      }
    };

    if (!currentUser || !selectedDocument) {
      return;
    }
    void publishPresence();
    const interval = window.setInterval(() => {
      void publishPresence();
    }, 8000);
    return () => {
      window.clearInterval(interval);
    };
  }, [currentUser, selectedDocument]);

  useEffect(() => {
    if (!currentUser || !selectedDocument) {
      return;
    }
    if (pendingPresenceTimerRef.current !== null) {
      window.clearTimeout(pendingPresenceTimerRef.current);
      pendingPresenceTimerRef.current = null;
    }
    const elapsed = Date.now() - lastPresenceSentAtRef.current;
    const delay = elapsed >= 600 ? 0 : 600 - elapsed;
    pendingPresenceTimerRef.current = window.setTimeout(() => {
      pendingPresenceTimerRef.current = null;
      const activeUser = currentUserRef.current;
      const activeDocument = selectedDocumentRef.current;
      if (!activeUser || !activeDocument) {
        return;
      }
      lastPresenceSentAtRef.current = Date.now();
      void fetch(`${apiBase}/api/presence`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          actorId: activeUser.handle,
          actorType: "human",
          documentId: activeDocument.id,
          filePath: activeDocument.path,
          mode: "editing",
          selection: [selectionRange.start, selectionRange.end],
          activity: `Editing ${activeDocument.title}`,
        }),
      });
    }, delay);
    return () => {
      if (pendingPresenceTimerRef.current !== null) {
        window.clearTimeout(pendingPresenceTimerRef.current);
        pendingPresenceTimerRef.current = null;
      }
    };
  }, [currentUser, selectedDocument, selectionRange.end, selectionRange.start]);

  useEffect(() => {
    if (!selectedDocument) {
      return;
    }
    const initialDraft = getOrCreateDoc(selectedDocument).getText("content").toString();
    draftRef.current = initialDraft;
    setDraft(initialDraft);
    setPathEditor(selectedDocument.path);
    setEditorScrollTop(0);
    selectionRangeRef.current = { start: 0, end: 0 };
    setSelectionRange({ start: 0, end: 0 });
    setSelectedThreadId("");
    pendingSelectionRef.current = null;
    isComposingRef.current = false;
    hasQueuedRemoteSyncRef.current = false;
    const actorHandle = currentUser?.handle ?? "owner";
    const actorName = currentUser?.name ?? "Workspace Owner";
    const ydoc = getOrCreateDoc(selectedDocument);
    const awareness = new Awareness(ydoc);
    awareness.setLocalState({
      actorId: actorHandle,
      actorName,
      actorType: "human",
      activity: `Editing ${selectedDocument.title}`,
      filePath: selectedDocument.path,
    });
    awarenessRef.current = awareness;

    const applyDraftFromYdoc = () => {
      const nextContent = ydoc.getText("content").toString();
      const previousContent = draftRef.current;
      if (nextContent === previousContent) {
        return;
      }
      const nextSelection = rebaseSelection(
        selectionRangeRef.current,
        computeReplace(previousContent, nextContent)
      );
      if (isComposingRef.current) {
        hasQueuedRemoteSyncRef.current = true;
        return;
      }
      draftRef.current = nextContent;
      selectionRangeRef.current = nextSelection;
      setDraft(nextContent);
      setSelectionRange(nextSelection);
      pendingSelectionRef.current = nextSelection;
    };

    const scheduleDraftSyncFromYdoc = () => {
      if (remoteDraftSyncTimerRef.current !== null) {
        return;
      }
      remoteDraftSyncTimerRef.current = window.setTimeout(() => {
        remoteDraftSyncTimerRef.current = null;
        applyDraftFromYdoc();
      }, 75);
    };

    const handleYdocUpdate = (update: Uint8Array, origin: unknown) => {
      if (origin === "remote" || origin === "bootstrap") {
        return;
      }
      if (wsRef.current?.readyState === WebSocket.OPEN) {
        wsRef.current.send(encodeSyncUpdate(update));
      }
    };
    ydoc.on("update", handleYdocUpdate);

    let disposed = false;
    let reconnectTimer: number | null = null;
    let attempt = 0;
    let receivedInitialSync = false;

    const connect = () => {
      if (disposed) {
        return;
      }
      const ws = new WebSocket(
        `${apiBase.replace("http", "ws")}/ws/documents/${selectedDocument.id}?client_id=${ydoc.clientID}&actor_id=${encodeURIComponent(actorHandle)}&actor_type=human`
      );
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;
      ws.onopen = () => {
        attempt = 0;
        ws.send(encodeSyncStep1(ydoc));
        ws.send(encodeAwarenessMessage(awareness, [ydoc.clientID]));
      };
      ws.onmessage = (event) => {
        const bytes = new Uint8Array(event.data as ArrayBuffer);
        const { value: messageType, offset } = decodeVarUint(bytes);
        const payload = bytes.slice(offset);
        if (messageType === 0) {
          const decoder = decoding.createDecoder(payload);
          const encoder = encoding.createEncoder();
          syncProtocol.readSyncMessage(decoder, encoder, ydoc, "remote");
          const reply = encoding.toUint8Array(encoder);
          if (reply.length > 0) {
            ws.send(encodeSyncReply(reply));
          }
          if (!receivedInitialSync) {
            receivedInitialSync = true;
            applyDraftFromYdoc();
            setDocumentReady(true);
          } else {
            scheduleDraftSyncFromYdoc();
          }
        }
        if (messageType === 1) {
          applyAwarenessUpdate(awareness, payload, "remote");
        }
      };
      ws.onerror = () => {
        ws.close();
      };
      ws.onclose = () => {
        if (wsRef.current === ws) {
          wsRef.current = null;
        }
        if (disposed) {
          return;
        }
        const delay = Math.min(1000 * 2 ** attempt, 5000);
        attempt += 1;
        reconnectTimer = window.setTimeout(connect, delay);
      };
    };

    connect();
    return () => {
      disposed = true;
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer);
      }
      if (remoteDraftSyncTimerRef.current !== null) {
        window.clearTimeout(remoteDraftSyncTimerRef.current);
        remoteDraftSyncTimerRef.current = null;
      }
      wsRef.current?.close();
      wsRef.current = null;
      ydoc.off("update", handleYdocUpdate);
      awarenessRef.current?.destroy();
      awarenessRef.current = null;
    };
  }, [currentUser?.id, selectedDocument?.id]);

  function resetEditorState(path = "") {
    draftRef.current = "";
    setDraft("");
    setPathEditor(path);
    setEditorScrollTop(0);
    selectionRangeRef.current = { start: 0, end: 0 };
    setSelectionRange({ start: 0, end: 0 });
    setSelectedThreadId("");
    pendingSelectionRef.current = null;
    isComposingRef.current = false;
    hasQueuedRemoteSyncRef.current = false;
  }

  function getOrCreateDoc(document: DocumentItem) {
    const existing = activeDocRef.current;
    if (existing?.id === document.id) {
      return existing.ydoc;
    }
    existing?.ydoc.destroy();
    const ydoc = new Y.Doc();
    activeDocRef.current = { id: document.id, ydoc };
    return ydoc;
  }


  async function loadWorkspace(nextSelectedId?: string) {
    const requestToken = workspaceLoadTokenRef.current + 1;
    workspaceLoadTokenRef.current = requestToken;
    const response = await fetch(`${apiBase}/api/workspace`);
    const data = (await response.json()) as Workspace;
    const selection = resolveWorkspaceLoad(
      requestToken,
      workspaceLoadTokenRef.current,
      data.documents,
      nextSelectedId ?? selectedIdRef.current
    );
    if (!selection.shouldApply) {
      return;
    }
    setWorkspace(data);
    selectedIdRef.current = selection.selectedId;
    setSelectedId((current) => (current === selection.selectedId ? current : selection.selectedId));
  }

  function handleEditorSelection(target: HTMLTextAreaElement) {
    const start = target.selectionStart;
    const end = target.selectionEnd;
    selectionRangeRef.current = { start, end };
    setSelectionRange({ start, end });
  }

  function handleDraftChange(nextDraft: string) {
    if (!selectedDocument) {
      return;
    }
    const ydoc = getOrCreateDoc(selectedDocument);
    const visibleCurrent = draftRef.current;
    const current = ydoc.getText("content").toString();
    let replace = computeReplace(visibleCurrent, nextDraft);
    if (current !== visibleCurrent) {
      replace = rebaseReplace(replace, computeReplace(visibleCurrent, current));
    }
    if (replace.start === replace.end && replace.text.length === 0) {
      return;
    }
    ydoc.transact(() => {
      const text = ydoc.getText("content");
      const deleteLength = replace.end - replace.start;
      if (deleteLength > 0) {
        text.delete(replace.start, deleteLength);
      }
      if (replace.text.length > 0) {
        text.insert(replace.start, replace.text);
      }
    }, "local");
    draftRef.current = nextDraft;
    setDraft(nextDraft);
    const nextSelection = textareaRef.current
      ? {
          start: textareaRef.current.selectionStart,
          end: textareaRef.current.selectionEnd,
        }
      : selectionRangeRef.current;
    selectionRangeRef.current = nextSelection;
    setSelectionRange(nextSelection);
  }

  async function createProposal() {
    if (!selectedDocument) {
      return;
    }
    const response = await fetch(`${apiBase}/api/proposals`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        documentId: selectedDocument.id,
        author: actorHandle,
        title: proposalTitle,
        proposedText: draft,
      }),
    });
    if (response.ok) {
      setMessage("Proposal created.");
      await loadWorkspace();
    }
  }

  async function mergeProposal(id: string) {
    const response = await fetch(`${apiBase}/api/proposals/${id}/merge?actor=${encodeURIComponent(actorHandle)}`, {
      method: "POST",
    });
    if (response.ok) {
      setMessage("Proposal merged.");
      await loadWorkspace();
    }
  }

  async function createThread() {
    if (!selectedDocument || !threadBody.trim()) {
      return;
    }
    const selectionStart = textareaRef.current?.selectionStart ?? 0;
    const selectionEnd = textareaRef.current?.selectionEnd ?? selectionStart;
    const response = await fetch(`${apiBase}/api/threads?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        documentId: selectedDocument.id,
        title: currentSelectionLabel,
        body: threadBody,
        start: selectionStart,
        end: selectionEnd,
      }),
    });
    if (response.ok) {
      const payload = (await response.json()) as { thread?: ThreadItem };
      setThreadBody("");
      setSelectedThreadId(payload.thread?.id ?? "");
      setMessage("Thread started.");
      await loadWorkspace(selectedDocument.id);
    }
  }

  async function replyToThread(threadId: string) {
    if (!threadReplyBody.trim()) {
      return;
    }
    const response = await fetch(`${apiBase}/api/threads/${threadId}/messages?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        body: threadReplyBody,
        kind: "comment",
      }),
    });
    if (response.ok) {
      setThreadReplyBody("");
      setMessage("Thread updated.");
      await loadWorkspace();
    }
  }

  async function createDocument() {
    const path = newFilePath.trim();
    if (!path) {
      return;
    }
    const response = await fetch(`${apiBase}/api/documents?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path, content: "" }),
    });
    if (!response.ok) {
      setMessage("Could not create file.");
      return;
    }
    const created = (await response.json()) as DocumentItem;
    setMessage("File created.");
    setNewFilePath(nextSuggestedPath(path));
    await loadWorkspace(created.id);
  }

  async function saveDocumentPath() {
    if (!selectedDocument) {
      return;
    }
    const nextPath = pathEditor.trim();
    if (!nextPath || nextPath === selectedDocument.path) {
      return;
    }
    const response = await fetch(
      `${apiBase}/api/documents/${selectedDocument.id}?actor=${encodeURIComponent(actorHandle)}&actor_type=human`,
      {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: nextPath }),
      }
    );
    if (!response.ok) {
      setMessage("Could not move file.");
      return;
    }
    const updated = (await response.json()) as DocumentItem;
    setActiveDocument((current) => (current?.id === updated.id ? { ...current, ...updated } : current));
    setMessage("File moved.");
    await loadWorkspace(selectedDocument.id);
  }

  async function deleteDocument() {
    if (!selectedDocument) {
      return;
    }
    const response = await fetch(
      `${apiBase}/api/documents/${selectedDocument.id}?actor=${encodeURIComponent(actorHandle)}&actor_type=human`,
      { method: "DELETE" }
    );
    if (!response.ok) {
      setMessage("Could not delete file.");
      return;
    }
    setMessage("File deleted.");
    await loadWorkspace();
  }

  const proposals = Object.values(workspace?.proposals ?? {}).filter(
    (proposal) => proposal.documentId === selectedId
  );
  const actorHandle = currentUser?.handle ?? "owner";
  const agentRuns = workspace?.agentRuns ?? [];
  const agents = workspace?.agents ?? [];
  const users = workspace?.users ?? [];
  const selectedAgentDetails = useMemo(
    () => agents.find((agent) => agent.id === agentDetailsId) ?? null,
    [agentDetailsId, agents]
  );

  function switchIdentity(userId: string) {
    setCurrentUserId(userId);
    const nextUser = users.find((user) => user.id === userId);
    if (nextUser) {
      setMessage(`Now acting as @${nextUser.handle}.`);
    }
  }

  function patchUserDraft(userId: string, patch: Partial<UserDraft>) {
    setUserDrafts((current) => ({
      ...current,
      [userId]: {
        ...current[userId],
        ...patch,
      },
    }));
  }

  function patchAgentDraft(agentId: string, patch: Partial<AgentDraft>) {
    setAgentDrafts((current) => ({
      ...current,
      [agentId]: {
        ...current[agentId],
        ...patch,
      },
    }));
  }

  async function createUser() {
    if (!newUserName.trim() || !newUserHandle.trim()) {
      return;
    }
    const response = await fetch(`${apiBase}/api/users?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: newUserName,
        handle: newUserHandle,
        role: newUserRole,
      }),
    });
    if (!response.ok) {
      setMessage("Could not create user.");
      return;
    }
    const created = (await response.json()) as UserItem;
    setMessage(`User @${created.handle} created.`);
    setCurrentUserId(created.id);
    setNewUserName("Workspace Collaborator");
    setNewUserHandle("collaborator");
    setNewUserRole("Collaborates in the shared workspace");
    await loadWorkspace();
  }

  async function saveUser(userId: string) {
    const draft = userDrafts[userId];
    if (!draft) {
      return;
    }
    const response = await fetch(`${apiBase}/api/users/${userId}?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(draft),
    });
    if (!response.ok) {
      setMessage("Could not save user.");
      return;
    }
    setMessage("User updated.");
    await loadWorkspace();
  }

  async function deleteUser(userId: string) {
    const response = await fetch(`${apiBase}/api/users/${userId}?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "DELETE",
    });
    if (!response.ok) {
      setMessage("Could not delete user.");
      return;
    }
    setMessage("User deleted.");
    await loadWorkspace();
  }

  async function createAgent() {
    if (!newAgentName.trim() || !newAgentHandle.trim()) {
      return;
    }
    const response = await fetch(`${apiBase}/api/agents?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        handle: newAgentHandle,
        name: newAgentName,
        role: newAgentRole,
        kind: "codex",
      }),
    });
    if (!response.ok) {
      setMessage("Could not create agent.");
      return;
    }
    setMessage("Agent created.");
    setNewAgentHandle("codex-agent");
    setNewAgentName("Codex Agent");
    setNewAgentRole("Implement changes in the shared workspace");
    setIsAgentModalOpen(false);
    await loadWorkspace();
  }

  async function saveAgent(agentId: string) {
    const draft = agentDrafts[agentId];
    if (!draft) {
      return;
    }
    const response = await fetch(`${apiBase}/api/agents/${agentId}?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        handle: draft.handle,
        name: draft.name,
        role: draft.role,
      }),
    });
    if (!response.ok) {
      setMessage("Could not save agent.");
      return;
    }
    setMessage("Agent updated.");
    await loadWorkspace();
  }

  async function deleteAgent(agentId: string) {
    const response = await fetch(`${apiBase}/api/agents/${agentId}?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "DELETE",
    });
    if (!response.ok) {
      setMessage("Could not delete agent.");
      return;
    }
    setMessage("Agent deleted.");
    await loadWorkspace();
  }

  async function startAgentRun(agent: Agent) {
    const draft = agentDrafts[agent.id];
    const prompt = draft?.taskPrompt?.trim();
    if (!prompt) {
      return;
    }
    const response = await fetch(`${apiBase}/api/agents/${agent.id}/runs?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt }),
    });
    if (!response.ok) {
      setMessage("Could not start agent run.");
      return;
    }
    setMessage(`${agent.name} queued in the daemon.`);
    await loadWorkspace();
  }

  async function stopAgentRun(runId: string) {
    const response = await fetch(`${apiBase}/api/agent-runs/${runId}/stop?actor=${encodeURIComponent(actorHandle)}&actor_type=human`, {
      method: "POST",
    });
    if (!response.ok) {
      setMessage("Could not stop Codex.");
      return;
    }
    setMessage("Stop requested.");
    await loadWorkspace();
  }

  return (
    <div className="appShell">
      <aside className="libraryRail">
        <div className="brandBlock">
          <p className="eyebrow">Collaborative workspace</p>
          <h1>notty</h1>
          <p className="railCopy">
            A calm writing surface for humans and agents, with Yjs sync underneath.
          </p>
        </div>

        <div className="railSection">
          <div className="railSectionHeader">
            <h2>Pages</h2>
            <span>{workspace?.documents.length ?? 0}</span>
          </div>
          <div className="newFileRow">
            <input
              value={newFilePath}
              name="new-file-path"
              aria-label="New file path"
              onChange={(event) => setNewFilePath(event.target.value)}
              className="railInput"
              placeholder="folder/file.md"
            />
            <button onClick={createDocument}>New</button>
          </div>
          <div className="pageList">
            {workspace?.documents.map((doc) => (
              <button
                key={doc.id}
                className={doc.id === selectedId ? "pageLink active" : "pageLink"}
                onClick={() => {
                  selectedIdRef.current = doc.id;
                  setSelectedId(doc.id);
                }}
              >
                <span className="pageEmoji">{doc.path.endsWith(".md") ? "M" : "D"}</span>
                <span className="pageMeta">
                  <strong>{doc.title}</strong>
                  <small>{doc.path}</small>
                  <small>{formatTimestamp(doc.updatedAt)}</small>
                </span>
              </button>
            ))}
          </div>
        </div>

        <div className="railSection">
          <div className="railSectionHeader">
            <div className="sectionHeadingGroup">
              <h2>People</h2>
              <span>{users.length + agents.length}</span>
            </div>
            <button className="secondary compactButton" onClick={() => setIsAgentModalOpen(true)}>
              New agent
            </button>
          </div>
          <details className="entityDetails">
            <summary>New user</summary>
            <div className="entityDetailsBody">
              <input
                value={newUserName}
                name="user-name"
                aria-label="User name"
                onChange={(event) => setNewUserName(event.target.value)}
                className="railInput"
                placeholder="Workspace collaborator"
              />
              <input
                value={newUserHandle}
                name="user-handle"
                aria-label="User handle"
                onChange={(event) => setNewUserHandle(event.target.value.toLowerCase())}
                className="railInput"
                placeholder="handle"
              />
              <input
                value={newUserRole}
                name="user-role"
                aria-label="User role"
                onChange={(event) => setNewUserRole(event.target.value)}
                className="railInput"
                placeholder="Role"
              />
              <button onClick={createUser}>Create user</button>
            </div>
          </details>
          <div className="agentRunList">
            {users.map((user) => {
              const draft = userDrafts[user.id];
              const isCurrent = currentUser?.id === user.id;
              return (
                <div key={user.id} className={isCurrent ? "agentRunCard activeUserCard" : "agentRunCard"}>
                  <div className="agentRunHeader">
                    <strong>{user.name}</strong>
                    <span className={isCurrent ? "statusPill status-running" : "statusPill status-idle"}>
                      {isCurrent ? "active" : user.status}
                    </span>
                  </div>
                  <small>@{user.handle}</small>
                  <small>{user.role}</small>
                  <div className="agentActionRow">
                    {isCurrent ? <span className="identityBadge">Current identity</span> : null}
                    <details className="entityDetails">
                      <summary>Edit</summary>
                      <div className="entityDetailsBody">
                        <input
                          value={draft?.name ?? user.name}
                          onChange={(event) => patchUserDraft(user.id, { name: event.target.value })}
                          className="railInput"
                          placeholder="User name"
                        />
                        <input
                          value={draft?.handle ?? user.handle}
                          onChange={(event) => patchUserDraft(user.id, { handle: event.target.value.toLowerCase() })}
                          className="railInput"
                          placeholder="handle"
                        />
                        <input
                          value={draft?.role ?? user.role}
                          onChange={(event) => patchUserDraft(user.id, { role: event.target.value })}
                          className="railInput"
                          placeholder="Role"
                        />
                        <button className="secondary" onClick={() => saveUser(user.id)}>Save</button>
                      </div>
                    </details>
                    {users.length > 1 ? (
                      <button className="dangerButton" onClick={() => deleteUser(user.id)}>Delete</button>
                    ) : null}
                  </div>
                </div>
              );
            })}
          </div>
        </div>

        <div className="railSection">
          <div className="railSectionHeader">
            <h2>Agents</h2>
            <span>{agents.length}</span>
          </div>
          <div className="agentRunList">
            {agents.map((agent) => {
              const draft = agentDrafts[agent.id];
              const currentRun = agentRuns.find((run) => run.id === agent.currentRunId) ?? null;
              const agentPresence = workspace?.presences?.[agent.handle] ?? workspace?.presences?.[agent.id] ?? null;
              const status = summarizeAgentStatus(workspaceConnected, agent, currentRun, agentPresence);
              return (
                <div key={agent.id} className="agentRunCard">
                  <div className="agentRunHeader">
                    <strong>{agent.name}</strong>
                    <span className={`statusPill status-${status.tone}`}>{status.label}</span>
                  </div>
                  <small>@{agent.handle}</small>
                  <small>{agent.role}</small>
                  <small>{status.copy}</small>
                  <div className="agentActionRow">
                    <button className="secondary" onClick={() => setAgentDetailsId(agent.id)}>Details</button>
                    <button className="dangerButton" onClick={() => deleteAgent(agent.id)}>Delete</button>
                  </div>
                </div>
              );
            })}
          </div>
        </div>

        <div className="railSection compact">
          <div className="miniStat">
            <strong>{activeCollaborators.length || 1}</strong>
            <span>active editors</span>
          </div>
          <div className="miniStat">
            <strong>{workspace?.threads.length ?? 0}</strong>
            <span>open threads</span>
          </div>
          <div className="miniStat">
            <strong>{Object.keys(workspace?.proposals ?? {}).length}</strong>
            <span>change proposals</span>
          </div>
        </div>
      </aside>

      {isAgentModalOpen ? (
        <div className="modalScrim" onClick={() => setIsAgentModalOpen(false)}>
          <div
            className="modalCard"
            role="dialog"
            aria-modal="true"
            aria-labelledby="new-agent-title"
            onClick={(event) => event.stopPropagation()}
          >
            <div className="modalHeader">
              <div>
                <p className="eyebrow">People</p>
                <h2 id="new-agent-title">Create agent</h2>
              </div>
              <button className="secondary compactButton" onClick={() => setIsAgentModalOpen(false)}>
                Close
              </button>
            </div>
            <div className="modalBody">
              <label className="fieldLabel">
                <span>Handle</span>
                <input
                  value={newAgentHandle}
                  name="agent-handle"
                  aria-label="Agent handle"
                  onChange={(event) => setNewAgentHandle(event.target.value.toLowerCase())}
                  className="railInput"
                  placeholder="agent-handle"
                />
              </label>
              <label className="fieldLabel">
                <span>Name</span>
                <input
                  value={newAgentName}
                  name="agent-name"
                  aria-label="Agent name"
                  onChange={(event) => setNewAgentName(event.target.value)}
                  className="railInput"
                  placeholder="Codex Agent"
                />
              </label>
              <label className="fieldLabel">
                <span>Role</span>
                <input
                  value={newAgentRole}
                  name="agent-role"
                  aria-label="Agent role"
                  onChange={(event) => setNewAgentRole(event.target.value)}
                  className="railInput"
                  placeholder="Agent role"
                />
              </label>
            </div>
            <div className="modalActions">
              <button className="secondary" onClick={() => setIsAgentModalOpen(false)}>
                Cancel
              </button>
              <button onClick={createAgent}>Create agent</button>
            </div>
          </div>
        </div>
      ) : null}

      {selectedAgentDetails ? (
        <div className="modalScrim" onClick={() => setAgentDetailsId("")}>
          <div
            className="modalCard"
            role="dialog"
            aria-modal="true"
            aria-labelledby="agent-details-title"
            onClick={(event) => event.stopPropagation()}
          >
            {(() => {
              const agent = selectedAgentDetails;
              const draft = agentDrafts[agent.id];
              const currentRun = agentRuns.find((run) => run.id === agent.currentRunId) ?? null;
              const agentPresence = workspace?.presences?.[agent.handle] ?? workspace?.presences?.[agent.id] ?? null;
              const status = summarizeAgentStatus(workspaceConnected, agent, currentRun, agentPresence);
              return (
                <>
                  <div className="modalHeader">
                    <div>
                      <p className="eyebrow">Agent</p>
                      <h2 id="agent-details-title">{agent.name}</h2>
                      <p className="modalSubcopy">@{agent.handle} · {status.label}</p>
                    </div>
                    <button className="secondary compactButton" onClick={() => setAgentDetailsId("")}>
                      Close
                    </button>
                  </div>
                  <div className="modalBody">
                    <div className="agentDetailGrid">
                      <div className="detailCard">
                        <span className="detailLabel">Role</span>
                        <p>{agent.role}</p>
                      </div>
                      <div className="detailCard">
                        <span className="detailLabel">Current activity</span>
                        <p>{agentPresence?.activity || agent.currentActivity || "Idle"}</p>
                      </div>
                      <div className="detailCard">
                        <span className="detailLabel">Workspace</span>
                        <p>{agent.workspaceRoot}</p>
                      </div>
                      <div className="detailCard">
                        <span className="detailLabel">Last heartbeat</span>
                        <p>{formatTimestamp(agentPresence?.updatedAt || agent.lastHeartbeatAt || currentRun?.lastHeartbeatAt || currentRun?.updatedAt)}</p>
                      </div>
                    </div>
                    <label className="fieldLabel">
                      <span>Handle</span>
                      <input
                        value={draft?.handle ?? agent.handle}
                        onChange={(event) => patchAgentDraft(agent.id, { handle: event.target.value.toLowerCase() })}
                        className="railInput"
                        placeholder="Agent handle"
                      />
                    </label>
                    <label className="fieldLabel">
                      <span>Name</span>
                      <input
                        value={draft?.name ?? agent.name}
                        onChange={(event) => patchAgentDraft(agent.id, { name: event.target.value })}
                        className="railInput"
                        placeholder="Agent name"
                      />
                    </label>
                    <label className="fieldLabel">
                      <span>Role</span>
                      <input
                        value={draft?.role ?? agent.role}
                        onChange={(event) => patchAgentDraft(agent.id, { role: event.target.value })}
                        className="railInput"
                        placeholder="Agent role"
                      />
                    </label>
                    <div className="detailCard">
                      <span className="detailLabel">System behavior</span>
                      <p>Managed by notty. Agents share a built-in collaboration prompt and cannot be customized here.</p>
                    </div>
                    {currentRun ? (
                      <details className="entityDetails modalDetails">
                        <summary>Run details</summary>
                        <div className="entityDetailsBody">
                          <div className="agentDetailGrid">
                            <div className="detailCard">
                              <span className="detailLabel">Run status</span>
                              <p>{currentRun.status}</p>
                            </div>
                            <div className="detailCard">
                              <span className="detailLabel">Last update</span>
                              <p>{formatTimestamp(currentRun.lastHeartbeatAt || currentRun.updatedAt)}</p>
                            </div>
                          </div>
                          <p>{currentRun.lastMessage || summarizeDocument(currentRun.prompt)}</p>
                          {currentRun.logTail?.length ? (
                            <code className="agentLogSnippet">{currentRun.logTail.join("\n")}</code>
                          ) : null}
                          {currentRun.error ? <small className="runError">{currentRun.error}</small> : null}
                        </div>
                      </details>
                    ) : null}
                  </div>
                  <div className="modalActions">
                    <button className="secondary" onClick={() => saveAgent(agent.id)}>Save</button>
                  </div>
                </>
              );
            })()}
          </div>
        </div>
      ) : null}

      <main className="workspaceMain">
        <div className="workspaceHeader">
          <div>
            <p className="eyebrow">Markdown workspace</p>
            <p className="topBarTitle">{workspace?.name ?? "Shared Workspace"}</p>
            <p className="railCopy">Signed in as @{currentUser?.handle ?? "owner"}</p>
          </div>
          <div className="workspaceTools">
            <label className="identitySwitcher">
              <span className="identityLabel">Identity</span>
              <select
                value={currentUser?.id ?? ""}
                onChange={(event) => switchIdentity(event.target.value)}
                className="identitySelect"
                aria-label="Switch user identity"
              >
                {users.map((user) => (
                  <option key={user.id} value={user.id}>
                    @{user.handle} · {user.name}
                  </option>
                ))}
              </select>
            </label>
            <button className="secondary" onClick={() => setMessage("Y-protocol sync is active.")}>
              Sync status
            </button>
            <div className="avatarStack" aria-label="People in this page">
              {activeCollaborators.map((presence) => (
                <div
                  key={presence.actorId}
                  className="avatarPill"
                  title={`${presence.actorName} · ${presence.activity}`}
                >
                  {presence.actorName.slice(0, 1).toUpperCase()}
                </div>
              ))}
            </div>
          </div>
        </div>

        <section className="documentStack">
          <div className="documentSurface">
            <header className="documentHeader">
              <div className="documentIcon" aria-hidden="true">
                {selectedDocumentView?.path.endsWith(".md") ? "M" : "D"}
              </div>
              <div className="documentHeading">
                <p className="eyebrow">Page</p>
                <h2>{selectedDocumentView?.title ?? "Untitled"}</h2>
                <div className="documentFacts">
                  <span>{selectedDocumentView?.path}</span>
                  <span>{formatTimestamp(selectedDocumentView?.updatedAt)}</span>
                  <span>{documentLines.length} lines</span>
                  <span>{wordCount} words</span>
                  <span>{charCount} chars</span>
                </div>
                <div className="pathEditorRow">
                  <input
                    value={pathEditor}
                    name="document-path"
                    aria-label="Document path"
                    onChange={(event) => setPathEditor(event.target.value)}
                    className="pathEditorInput"
                    placeholder="folder/file.md"
                  />
                  <button className="secondary" onClick={saveDocumentPath}>Move / rename</button>
                  <button className="dangerButton" onClick={deleteDocument}>Delete</button>
                </div>
              </div>
            </header>

            <div className="editorShell">
              <div className="lineNumberRail" aria-hidden="true">
                <div
                  className="lineTrack"
                  style={{
                    height: `${editorTrackHeight}px`,
                    transform: `translateY(-${editorScrollTop}px)`,
                  }}
                >
                  {documentLines.map((_, index) => (
                    <div
                      key={`line-${index + 1}`}
                      className={commentedLines.has(index + 1) ? "lineNumber hasComment" : "lineNumber"}
                    >
                      {index + 1}
                    </div>
                  ))}
                </div>
              </div>

              <div className="editorBody">
                <textarea
                  ref={textareaRef}
                  value={draft}
                  name="document-editor"
                  aria-label="Document editor"
                  spellCheck={false}
                  disabled={!selectedDocument || !documentReady}
                  onChange={(event) => handleDraftChange(event.target.value)}
                  onCompositionStart={() => {
                    isComposingRef.current = true;
                  }}
                  onCompositionEnd={(event) => {
                    isComposingRef.current = false;
                    handleEditorSelection(event.currentTarget);
                    if (!selectedDocument) {
                      return;
                    }
                    const ydoc = getOrCreateDoc(selectedDocument);
                    const nextContent = ydoc.getText("content").toString();
                    if (!hasQueuedRemoteSyncRef.current && nextContent === draftRef.current) {
                      return;
                    }
                    hasQueuedRemoteSyncRef.current = false;
                    const nextSelection = rebaseSelection(
                      selectionRangeRef.current,
                      computeReplace(draftRef.current, nextContent)
                    );
                    draftRef.current = nextContent;
                    selectionRangeRef.current = nextSelection;
                    setDraft(nextContent);
                    setSelectionRange(nextSelection);
                    pendingSelectionRef.current = nextSelection;
                  }}
                  onScroll={(event) => setEditorScrollTop(event.currentTarget.scrollTop)}
                  onClick={(event) => handleEditorSelection(event.currentTarget)}
                  onKeyUp={(event) => handleEditorSelection(event.currentTarget)}
                  onSelect={(event) => handleEditorSelection(event.currentTarget)}
                  className="markdownEditor"
                  placeholder={selectedDocumentView && !documentReady ? "Loading document..." : "Start writing"}
                />
              </div>

              <div className="commentRail" aria-label="Threads on document lines">
                <div
                  className="commentTrack"
                  style={{
                    height: `${editorTrackHeight}px`,
                    transform: `translateY(-${editorScrollTop}px)`,
                  }}
                >
                  {lineThreads.map((threadGroup) => (
                    (() => {
                      const isGroupActive = threadGroup.threads.some((thread) => thread.id === selectedThreadId);
                      const previewThread = isGroupActive
                        ? threadGroup.threads.find((thread) => thread.id === selectedThreadId) ?? threadGroup.threads[0]
                        : threadGroup.threads[0];
                      return (
                        <div
                          key={`thread-line-${threadGroup.line}`}
                          className="commentAnchor"
                          style={{ top: `${(threadGroup.line - 1) * EDITOR_LINE_HEIGHT + 12}px` }}
                        >
                          <div className="commentConnector" />
                          <div className={isGroupActive ? "lineCommentStack activeThreadCard" : "lineCommentStack"}>
                            <button
                              type="button"
                              className="lineCommentCard"
                              onClick={() => setSelectedThreadId(previewThread?.id ?? "")}
                            >
                              <small>Line {threadGroup.line}</small>
                              <strong>{previewThread?.title || "Thread"}</strong>
                              <span>{previewThread?.messages[previewThread.messages.length - 1]?.body || previewThread?.anchor.excerpt}</span>
                              {threadGroup.threads.length > 1 ? (
                                <small>{threadGroup.threads.length} threads on this line</small>
                              ) : null}
                            </button>
                            {isGroupActive && threadGroup.threads.length > 1 ? (
                              <div className="lineThreadList" aria-label={`Threads on line ${threadGroup.line}`}>
                                {threadGroup.threads.map((thread, index) => (
                                  <button
                                    key={thread.id}
                                    type="button"
                                    className={thread.id === selectedThreadId ? "threadChoice activeThreadChoice" : "threadChoice"}
                                    onClick={() => setSelectedThreadId(thread.id)}
                                  >
                                    <strong>{thread.title || `Thread ${index + 1}`}</strong>
                                    <span>{thread.messages[thread.messages.length - 1]?.body || thread.anchor.excerpt}</span>
                                  </button>
                                ))}
                              </div>
                            ) : null}
                          </div>
                        </div>
                      );
                    })()
                  ))}
                </div>
              </div>
            </div>

            <footer className="documentComposer">
              <div className="composerStatus">
                <span>{message || "Markdown edits and thread activity sync across frontend, backend, and daemon."}</span>
                <span>{currentSelectionLabel}</span>
              </div>
              {selectedThread ? (
                <div className="threadPanel">
                  <div className="threadPanelHeader">
                    <div>
                      <strong>{selectedThread.title}</strong>
                      <small>Line {selectedThread.anchor.line} · {selectedThread.status}</small>
                    </div>
                    <button className="secondary" onClick={() => setSelectedThreadId("")}>Close thread</button>
                  </div>
                  {selectedThreadGroup && selectedThreadGroup.threads.length > 1 ? (
                    <div className="threadSwitcher" aria-label={`All threads on line ${selectedThreadGroup.line}`}>
                      {selectedThreadGroup.threads.map((thread, index) => (
                        <button
                          key={thread.id}
                          type="button"
                          className={thread.id === selectedThreadId ? "threadChoice activeThreadChoice" : "threadChoice"}
                          onClick={() => setSelectedThreadId(thread.id)}
                        >
                          <strong>{thread.title || `Thread ${index + 1}`}</strong>
                          <span>{thread.messages[thread.messages.length - 1]?.body || thread.anchor.excerpt}</span>
                        </button>
                      ))}
                    </div>
                  ) : null}
                  <div className="threadMessageList">
                    {selectedThread.messages.map((threadMessage) => (
                      <div key={threadMessage.id} className="threadMessageCard">
                        <div className="threadMessageMeta">
                          <strong>@{threadMessage.authorHandle}</strong>
                          <span>{formatTimestamp(threadMessage.createdAt)}</span>
                        </div>
                        <p>{threadMessage.body}</p>
                      </div>
                    ))}
                  </div>
                  <div className="composerRow">
                    <textarea
                      value={threadReplyBody}
                      name="thread-reply-body"
                      aria-label="Thread reply"
                      onChange={(event) => setThreadReplyBody(event.target.value)}
                      className="inlineCommentInput"
                      placeholder="Reply in this thread"
                    />
                    <button onClick={() => replyToThread(selectedThread.id)}>Reply</button>
                  </div>
                </div>
              ) : (
                <div className="composerRow">
                  <textarea
                    value={threadBody}
                    name="thread-body"
                    aria-label="Thread body"
                    onChange={(event) => setThreadBody(event.target.value)}
                    className="inlineCommentInput"
                    placeholder="Start a thread on the current selection"
                  />
                  <button onClick={createThread}>Start thread</button>
                </div>
              )}
            </footer>
          </div>

          <div className="proposalDock">
            <div className="proposalDockHeader">
              <div>
                <p className="eyebrow">Proposals</p>
                <h3>Change requests for this page</h3>
              </div>
              <span>{proposals.length}</span>
            </div>
            <div className="proposalComposer">
              <input
                value={proposalTitle}
                name="proposal-title"
                aria-label="Proposal title"
                onChange={(event) => setProposalTitle(event.target.value)}
                className="proposalInputInline"
                placeholder="Name this change"
              />
              <button onClick={createProposal}>Create proposal</button>
            </div>
            <div className="proposalListInline">
              {proposals.map((proposal) => (
                <div key={proposal.id} className="proposalPill">
                  <div>
                    <strong>{proposal.title}</strong>
                    <span>{proposal.author} · {proposal.status}</span>
                  </div>
                  {proposal.status === "open" ? (
                    <button className="secondary" onClick={() => mergeProposal(proposal.id)}>
                      Merge
                    </button>
                  ) : null}
                </div>
              ))}
            </div>
          </div>
        </section>
      </main>
    </div>
  );
}

function encodeSyncStep1(doc: Y.Doc) {
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, 0);
  syncProtocol.writeSyncStep1(encoder, doc);
  return encoding.toUint8Array(encoder);
}

function encodeSyncUpdate(update: Uint8Array) {
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, 0);
  syncProtocol.writeUpdate(encoder, update);
  return encoding.toUint8Array(encoder);
}

function encodeSyncReply(reply: Uint8Array) {
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, 0);
  encoding.writeUint8Array(encoder, reply);
  return encoding.toUint8Array(encoder);
}

function encodeAwarenessMessage(awareness: Awareness, clients: number[]) {
  const update = encodeAwarenessUpdate(awareness, clients);
  const encoder = encoding.createEncoder();
  encoding.writeVarUint(encoder, 1);
  encoding.writeUint8Array(encoder, update);
  return encoding.toUint8Array(encoder);
}

function decodeVarUint(bytes: Uint8Array) {
  let value = 0;
  let shift = 0;
  let offset = 0;
  while (offset < bytes.length) {
    const current = bytes[offset];
    value |= (current & 0x7f) << shift;
    offset += 1;
    if ((current & 0x80) === 0) {
      return { value, offset };
    }
    shift += 7;
  }
  throw new Error("Unexpected end of array");
}

function summarizeDocument(content: string) {
  const collapsed = content.replace(/\s+/g, " ").trim();
  if (!collapsed) {
    return "Empty page";
  }
  return collapsed.length > 42 ? `${collapsed.slice(0, 42)}...` : collapsed;
}

function nextSuggestedPath(path: string) {
  const trimmed = path.trim();
  if (!trimmed) {
    return "notes/untitled.md";
  }
  const parts = trimmed.split("/");
  const filename = parts.pop() ?? "untitled.md";
  const dot = filename.lastIndexOf(".");
  const stem = dot > 0 ? filename.slice(0, dot) : filename;
  const ext = dot > 0 ? filename.slice(dot) : ".md";
  parts.push(`${stem}-new${ext}`);
  return parts.join("/");
}

function countWords(value: string) {
  const parts = value.trim().split(/\s+/).filter(Boolean);
  return parts.length;
}

function formatTimestamp(value?: string) {
  if (!value) {
    return "Not synced yet";
  }
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  }).format(new Date(value));
}

function buildLineStarts(content: string) {
  const starts = [0];
  for (let index = 0; index < content.length; index += 1) {
    if (content[index] === "\n") {
      starts.push(index + 1);
    }
  }
  return starts;
}

function getLineForOffset(lineStarts: number[], offset: number) {
  let line = 1;
  for (let index = 0; index < lineStarts.length; index += 1) {
    if (lineStarts[index] > offset) {
      break;
    }
    line = index + 1;
  }
  return line;
}

function resolveThreadAnchorLive(anchor: ThreadAnchor, ydoc: Y.Doc | null, content: string, lineStarts: number[]) {
  if (!ydoc || !anchor.relativeStart || !anchor.relativeEnd) {
    return anchor;
  }
  try {
    const startPosition = Y.createAbsolutePositionFromRelativePosition(
      Y.decodeRelativePosition(base64ToUint8Array(anchor.relativeStart)),
      ydoc
    );
    const endPosition = Y.createAbsolutePositionFromRelativePosition(
      Y.decodeRelativePosition(base64ToUint8Array(anchor.relativeEnd)),
      ydoc
    );
    if (!startPosition || !endPosition) {
      return anchor;
    }
    const start = Math.max(0, startPosition.index);
    const end = Math.max(start, endPosition.index);
    const previewEnd = end === start ? Math.min(content.length, start + 80) : end;
    const excerpt = (content.slice(start, previewEnd).trim() || anchor.excerpt).slice(0, 140);
    return {
      ...anchor,
      start,
      end,
      line: getLineForOffset(lineStarts, start),
      excerpt,
    };
  } catch {
    return anchor;
  }
}

function base64ToUint8Array(value: string) {
  const binary = window.atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}

function formatSelection(start: number, end: number, lineStarts: number[]) {
  if (start === end) {
    const line = getLineForOffset(lineStarts, start);
    return `Cursor on line ${line}`;
  }
  const startLine = getLineForOffset(lineStarts, start);
  const endLine = getLineForOffset(lineStarts, end);
  if (startLine === endLine) {
    return `Selection on line ${startLine}`;
  }
  return `Selection across lines ${startLine}-${endLine}`;
}

function buildActiveCollaborators(
  presences: Record<string, PresenceItem>,
  users: UserItem[],
  agents: Agent[],
  documentID: string,
  filePath: string,
  fallback: PresenceView
) {
  const people = new Map<string, { name: string }>();
  for (const user of users) {
    people.set(user.handle, { name: user.name });
  }
  for (const agent of agents) {
    people.set(agent.handle, { name: agent.name });
  }

  const collaborators = Object.values(presences)
    .filter((presence) => isFreshPresence(presence.updatedAt))
    .filter((presence) => presence.documentId === documentID || presence.filePath === filePath)
    .map((presence) => ({
      actorId: presence.actorId,
      actorName: people.get(presence.actorId)?.name ?? presence.actorId,
      activity: presence.activity || "Editing this page",
    }))
    .sort((left, right) => left.actorName.localeCompare(right.actorName));

  if (collaborators.length === 0) {
    return [fallback];
  }
  return collaborators;
}

const EDITOR_LINE_HEIGHT = 34;
