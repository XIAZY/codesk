import { useEffect, useRef, useState } from "react";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { markdown } from "@codemirror/lang-markdown";
import {
  bracketMatching,
  defaultHighlightStyle,
  foldGutter,
  foldKeymap,
  syntaxHighlighting,
} from "@codemirror/language";
import {
  Annotation,
  EditorState,
  StateEffect,
  StateField,
  type ChangeSpec,
  type Text,
} from "@codemirror/state";
import {
  Decoration,
  type DecorationSet,
  drawSelection,
  EditorView,
  highlightActiveLine,
  keymap,
  lineNumbers,
  type ViewUpdate,
} from "@codemirror/view";
import { highlightSelectionMatches, searchKeymap } from "@codemirror/search";
import * as Y from "yjs";
import {
  base64ToUint8Array,
  encodeRelativeAnchor,
  type LineThreadGroup,
  type ResolvedThreadAnchor,
} from "./logic";
import type { ThreadItem } from "./types";

export type LiveThread = ThreadItem & { anchor: ResolvedThreadAnchor };

export type SurfaceSelection = {
  start: number;
  end: number;
  title: string;
  excerpt: string;
  relativeStart: string;
  relativeEnd: string;
  point: { x: number; y: number };
};

type DocumentSurfaceProps = {
  documentId: string;
  ydoc: Y.Doc;
  ytext: Y.Text;
  ready: boolean;
  threads: ThreadItem[];
  focusThreadId: string;
  onFocusThreadHandled: () => void;
  onSelectionChange: (selection: SurfaceSelection | null) => void;
  onLineThreadsOpen: (group: LineThreadGroup<LiveThread>, point: { x: number; y: number }) => void;
};

type ThreadRailMarker = LineThreadGroup<LiveThread> & {
  top: number;
};

const remoteYjsAnnotation = Annotation.define<boolean>();
const setThreadDecorations = StateEffect.define<DecorationSet>();

const threadDecorationField = StateField.define<DecorationSet>({
  create() {
    return Decoration.none;
  },
  update(value, transaction) {
    for (const effect of transaction.effects) {
      if (effect.is(setThreadDecorations)) {
        return effect.value;
      }
    }
    return value.map(transaction.changes);
  },
  provide: (field) => EditorView.decorations.from(field),
});

const editorTheme = EditorView.theme({
  "&": {
    minHeight: "560px",
    width: "100%",
    background: "transparent",
    color: "var(--ink)",
    fontFamily: "var(--font-mono)",
    fontSize: "14px",
  },
  ".cm-scroller": {
    minHeight: "560px",
    maxHeight: "calc(100vh - 220px)",
    overflow: "auto",
    fontFamily: "var(--font-mono)",
    lineHeight: "1.65",
  },
  ".cm-content": {
    padding: "28px 30px 64px 18px",
  },
  ".cm-line": {
    padding: "0 4px",
  },
  ".cm-gutters": {
    background: "rgba(255,255,255,0.62)",
    borderRight: "1px solid var(--line)",
    color: "var(--muted)",
  },
  ".cm-activeLine": {
    background: "rgba(38, 82, 68, 0.06)",
  },
  ".cm-activeLineGutter": {
    background: "rgba(38, 82, 68, 0.08)",
  },
  ".cm-selectionBackground": {
    background: "rgba(215, 138, 75, 0.28) !important",
  },
  ".cm-thread-highlight": {
    background: "rgba(215, 138, 75, 0.24)",
    borderBottom: "1px solid rgba(143, 81, 43, 0.42)",
    borderRadius: "5px",
  },
});

export function DocumentSurface({
  documentId,
  ydoc,
  ytext,
  ready,
  threads,
  focusThreadId,
  onFocusThreadHandled,
  onSelectionChange,
  onLineThreadsOpen,
}: DocumentSurfaceProps) {
  const shellRef = useRef<HTMLDivElement | null>(null);
  const hostRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const threadsRef = useRef(threads);
  const onSelectionChangeRef = useRef(onSelectionChange);
  const onLineThreadsOpenRef = useRef(onLineThreadsOpen);
  const refreshTimerRef = useRef<number | null>(null);
  const localOriginRef = useRef({});
  const [railMarkers, setRailMarkers] = useState<ThreadRailMarker[]>([]);

  threadsRef.current = threads;
  onSelectionChangeRef.current = onSelectionChange;
  onLineThreadsOpenRef.current = onLineThreadsOpen;

  useEffect(() => {
    const shell = shellRef.current;
    const host = hostRef.current;
    if (!shell || !host) {
      return;
    }

    const scheduleThreadRefresh = (view: EditorView) => {
      if (refreshTimerRef.current !== null) {
        window.clearTimeout(refreshTimerRef.current);
      }
      refreshTimerRef.current = window.setTimeout(() => {
        refreshTimerRef.current = null;
        if (viewRef.current === view) {
          refreshThreadProjection(view, shell, ydoc, threadsRef.current, setRailMarkers);
        }
      }, 0);
    };

    const view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: ytext.toString(),
        extensions: [
          lineNumbers(),
          foldGutter(),
          history(),
          drawSelection(),
          bracketMatching(),
          highlightActiveLine(),
          highlightSelectionMatches(),
          markdown(),
          syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
          threadDecorationField,
          editorTheme,
          EditorView.lineWrapping,
          EditorView.updateListener.of((update) => {
            if (update.docChanged && !update.transactions.some((transaction) => transaction.annotation(remoteYjsAnnotation))) {
              applyCodeMirrorChangesToYText(ydoc, ytext, update, localOriginRef.current);
            }
            if (update.docChanged) {
              scheduleThreadRefresh(update.view);
            }
            if (update.viewportChanged || update.geometryChanged) {
              scheduleThreadRefresh(update.view);
            }
            if (update.selectionSet || update.docChanged || update.focusChanged) {
              onSelectionChangeRef.current(selectionFromView(update.view, ytext));
            }
          }),
          keymap.of([indentWithTab, ...defaultKeymap, ...historyKeymap, ...foldKeymap, ...searchKeymap]),
        ],
      }),
    });

    viewRef.current = view;
    refreshThreadProjection(view, shell, ydoc, threadsRef.current, setRailMarkers);

    const handleScroll = () => scheduleThreadRefresh(view);
    const handleResize = () => scheduleThreadRefresh(view);
    view.scrollDOM.addEventListener("scroll", handleScroll, { passive: true });
    window.addEventListener("resize", handleResize);

    const observer = (event: Y.YTextEvent) => {
      if (event.transaction.origin !== localOriginRef.current) {
        const changes = yTextDeltaToCodeMirrorChanges(event.delta);
        if (changes.length) {
          view.dispatch({
            changes,
            annotations: remoteYjsAnnotation.of(true),
          });
        }
      }
      scheduleThreadRefresh(view);
    };
    ytext.observe(observer);

    return () => {
      ytext.unobserve(observer);
      view.scrollDOM.removeEventListener("scroll", handleScroll);
      window.removeEventListener("resize", handleResize);
      if (refreshTimerRef.current !== null) {
        window.clearTimeout(refreshTimerRef.current);
        refreshTimerRef.current = null;
      }
      view.destroy();
      if (viewRef.current === view) {
        viewRef.current = null;
      }
    };
  }, [documentId, ydoc, ytext]);

  useEffect(() => {
    const view = viewRef.current;
    const shell = shellRef.current;
    if (!view || !shell) {
      return;
    }
    refreshThreadProjection(view, shell, ydoc, threads, setRailMarkers);
  }, [threads, ydoc, onLineThreadsOpen]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view || !focusThreadId) {
      return;
    }
    const thread = threads.find((item) => item.id === focusThreadId);
    if (!thread) {
      onFocusThreadHandled();
      return;
    }
    const resolved = resolveThreadAnchorForEditor(thread, ydoc, view.state.doc);
    if (resolved.anchor.resolved) {
      const start = Math.max(0, Math.min(view.state.doc.length, resolved.anchor.start));
      const end = Math.max(start, Math.min(view.state.doc.length, resolved.anchor.end));
      view.dispatch({
        selection: { anchor: start, head: end },
        effects: EditorView.scrollIntoView(start, { y: "center" }),
      });
      view.focus();
    }
    onFocusThreadHandled();
  }, [focusThreadId, onFocusThreadHandled, threads, ydoc]);

  return (
    <div ref={shellRef} className="document-surface-shell">
      {!ready ? <div className="document-loading">Opening document…</div> : null}
      <div ref={hostRef} className="document-surface" aria-label="Document editor" />
      <div className="thread-anchor-rail" aria-label="Thread anchors">
        {railMarkers.map((marker) => {
          const count = marker.threads.length;
          return (
            <button
              key={`${marker.line}:${marker.threads.map((thread) => thread.id).join("|")}`}
              className="thread-rail-marker"
              type="button"
              style={{ top: marker.top }}
              aria-label={`${count} thread${count === 1 ? "" : "s"} on line ${marker.line}`}
              title={`${count} thread${count === 1 ? "" : "s"} on line ${marker.line}`}
              onMouseDown={(event) => event.preventDefault()}
              onClick={(event) => {
                event.preventDefault();
                event.stopPropagation();
                const rect = event.currentTarget.getBoundingClientRect();
                onLineThreadsOpenRef.current(marker, {
                  x: Math.min(Math.max(12, rect.left), Math.max(12, window.innerWidth - 340)),
                  y: Math.min(rect.bottom + 8, Math.max(12, window.innerHeight - 260)),
                });
              }}
            >
              <span className="thread-rail-dot" />
              <span>{count}</span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

function applyCodeMirrorChangesToYText(ydoc: Y.Doc, ytext: Y.Text, update: ViewUpdate, origin: object) {
  let offset = 0;
  ydoc.transact(() => {
    update.changes.iterChanges((fromA, toA, _fromB, _toB, inserted) => {
      const from = fromA + offset;
      const deleteLength = Math.max(0, toA - fromA);
      if (deleteLength > 0) {
        ytext.delete(from, deleteLength);
      }
      const insertedText = inserted.toString();
      if (insertedText.length > 0) {
        ytext.insert(from, insertedText);
      }
      offset += insertedText.length - deleteLength;
    });
  }, origin);
}

function yTextDeltaToCodeMirrorChanges(delta: Y.YTextEvent["delta"]): ChangeSpec[] {
  const changes: ChangeSpec[] = [];
  let index = 0;
  for (const part of delta) {
    if (part.retain) {
      index += part.retain;
    }
    if (typeof part.insert === "string" && part.insert.length > 0) {
      changes.push({ from: index, to: index, insert: part.insert });
    }
    if (part.delete) {
      changes.push({ from: index, to: index + part.delete });
      index += part.delete;
    }
  }
  return changes;
}

function selectionFromView(view: EditorView, ytext: Y.Text): SurfaceSelection | null {
  const range = view.state.selection.main;
  const start = Math.min(range.from, range.to);
  const end = Math.max(range.from, range.to);
  if (end - start < 2) {
    return null;
  }
  const title = selectionLabelForDoc(view.state.doc, start, end);
  const excerptEnd = end === start ? Math.min(view.state.doc.length, start + 80) : end;
  const excerpt = (view.state.doc.sliceString(start, excerptEnd).trim() || title).slice(0, 220);
  const coords = view.coordsAtPos(end) || view.coordsAtPos(start);
  return {
    start,
    end,
    title,
    excerpt,
    relativeStart: encodeRelativeAnchor(ytext, start),
    relativeEnd: encodeRelativeAnchor(ytext, end),
    point: {
      x: coords ? coords.left : 24,
      y: coords ? coords.bottom : 120,
    },
  };
}

function selectionLabelForDoc(doc: Text, start: number, end: number) {
  const startLine = doc.lineAt(Math.max(0, Math.min(doc.length, start))).number;
  const endLine = doc.lineAt(Math.max(0, Math.min(doc.length, Math.max(start, end - 1)))).number;
  if (start === end) {
    return `Cursor on line ${startLine}`;
  }
  if (startLine === endLine) {
    return `Selection on line ${startLine}`;
  }
  return `Selection across lines ${startLine}-${endLine}`;
}

function refreshThreadProjection(
  view: EditorView,
  shell: HTMLElement,
  ydoc: Y.Doc,
  threads: ThreadItem[],
  setRailMarkers: (markers: ThreadRailMarker[]) => void
) {
  applyThreadDecorations(view, ydoc, threads);
  setRailMarkers(computeThreadRailMarkers(view, shell, ydoc, threads));
}

function applyThreadDecorations(view: EditorView, ydoc: Y.Doc, threads: ThreadItem[]) {
  const ranges: ReturnType<Decoration["range"]>[] = [];

  for (const thread of threads) {
    const liveThread = resolveThreadAnchorForEditor(thread, ydoc, view.state.doc);
    if (!liveThread.anchor.resolved || liveThread.anchor.kind === "document") {
      continue;
    }
    const start = Math.max(0, Math.min(view.state.doc.length, liveThread.anchor.start));
    const end = Math.max(start, Math.min(view.state.doc.length, liveThread.anchor.end));
    if (end > start) {
      ranges.push(Decoration.mark({ class: "cm-thread-highlight" }).range(start, end));
    }
  }

  view.dispatch({ effects: setThreadDecorations.of(Decoration.set(ranges, true)) });
}

function computeThreadRailMarkers(view: EditorView, shell: HTMLElement, ydoc: Y.Doc, threads: ThreadItem[]): ThreadRailMarker[] {
  const shellRect = shell.getBoundingClientRect();
  const groups = new Map<number, LiveThread[]>();

  for (const thread of threads) {
    const liveThread = resolveThreadAnchorForEditor(thread, ydoc, view.state.doc);
    if (!liveThread.anchor.resolved || liveThread.anchor.kind === "document") {
      continue;
    }
    groups.set(liveThread.anchor.line, [...(groups.get(liveThread.anchor.line) ?? []), liveThread]);
  }

  return Array.from(groups.entries())
    .sort(([left], [right]) => left - right)
    .flatMap(([lineNumber, groupThreads]) => {
      const safeLine = Math.max(1, Math.min(view.state.doc.lines, lineNumber));
      const line = view.state.doc.line(safeLine);
      if (line.to < view.viewport.from || line.from > view.viewport.to) {
        return [];
      }
      const coords = view.coordsAtPos(line.from);
      const block = view.lineBlockAt(line.from);
      const top = coords ? coords.top - shellRect.top + 1 : view.documentTop + block.top - shellRect.top + 1;
      return [
        {
          line: safeLine,
          threads: groupThreads,
          top: Math.max(0, top),
        },
      ];
    });
}

function resolveThreadAnchorForEditor(thread: ThreadItem, ydoc: Y.Doc, doc: Text): LiveThread {
  const fallback: LiveThread = {
    ...thread,
    anchor: {
      ...thread.anchor,
      start: 0,
      end: 0,
      line: 1,
      excerpt: (thread.anchor.excerpt || thread.title || "").slice(0, 140),
      resolved: thread.anchor.kind === "document",
    },
  };
  if (!thread.anchor.relativeStart || !thread.anchor.relativeEnd) {
    return fallback;
  }
  try {
    const startPosition = Y.createAbsolutePositionFromRelativePosition(
      Y.decodeRelativePosition(base64ToUint8Array(thread.anchor.relativeStart)),
      ydoc
    );
    const endPosition = Y.createAbsolutePositionFromRelativePosition(
      Y.decodeRelativePosition(base64ToUint8Array(thread.anchor.relativeEnd)),
      ydoc
    );
    if (!startPosition || !endPosition) {
      return fallback;
    }
    const start = Math.max(0, Math.min(doc.length, startPosition.index));
    const end = Math.max(start, Math.min(doc.length, endPosition.index));
    const line = doc.lineAt(start).number;
    const previewEnd = end === start ? Math.min(doc.length, start + 80) : end;
    return {
      ...thread,
      anchor: {
        ...thread.anchor,
        start,
        end,
        line,
        excerpt: (doc.sliceString(start, previewEnd).trim() || thread.anchor.excerpt || thread.title || "").slice(0, 140),
        resolved: true,
      },
    };
  } catch {
    return fallback;
  }
}
