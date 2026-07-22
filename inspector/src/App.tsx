import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent, type PointerEvent } from "react";
import { FileUp, FlaskConical, Radio, ShieldCheck, Trash2, X } from "lucide-react";
import type { HarEntry, NEntry } from "./types/har";
import { HarParseError, groupByTrace, parseHar, type LoadedHar } from "./lib/parse";
import { sampleHar } from "./sampleHar";
import { applyFilters, emptyFilters, Filters, sortEntries, type FilterState, type SortKey } from "./components/Filters";
import { EntryList } from "./components/EntryList";
import { DetailPanel } from "./components/DetailPanel";
import { TooltipLayer } from "./components/Shared";
import { AppearanceControls } from "./components/AppearanceControls";
import { TraceGroupPanel } from "./components/TraceGroupPanel";
import { fetchRemoteHar } from "./lib/remoteHar";
import { liveReconnectDelay, parseLiveEntry, validateDebugStreamURL } from "./lib/liveStream";
import { decryptLiveEntry, protectedOccurrences } from "./lib/protection";
import { clampSidebarWidth, sidebarDefaultWidth, sidebarMaxWidth, sidebarMinWidth } from "./lib/layout";

interface Doc {
  name: string;
  loaded: LoadedHar;
}

const liveEntryLimit = 2_000;
const sidebarStorageKey = "recorder.inspector.sidebarWidth";

function replaceDeepLink(mode: "sample" | null) {
  const url = new URL(window.location.href);
  url.searchParams.delete("har");
  url.searchParams.delete("sample");
  const remaining = url.searchParams.toString();
  const search = mode === "sample" ? `${remaining ? `${remaining}&` : ""}sample` : remaining;
  window.history.replaceState(null, "", `${url.pathname}${search ? `?${search}` : ""}${url.hash}`);
}

export default function App() {
  const [doc, setDoc] = useState<Doc | null>(null);
  const [loadError, setLoadError] = useState<{ message: string; detail?: string } | null>(null);
  const [filters, setFilters] = useState<FilterState>(emptyFilters);
  const [sortKey, setSortKey] = useState<SortKey>("start");
  const [sortDesc, setSortDesc] = useState(false);
  const [grouped, setGrouped] = useState(true);
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [selectedTraceId, setSelectedTraceId] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const [loadingRemote, setLoadingRemote] = useState(false);
  const [protectionKeys, setProtectionKeys] = useState<ReadonlyMap<string, string>>(new Map());
  const [resolvedValues, setResolvedValues] = useState<ReadonlyMap<string, string>>(new Map());
  const [protectionClearEpoch, setProtectionClearEpoch] = useState(0);
  const [liveOpen, setLiveOpen] = useState(false);
  const [liveURL, setLiveURL] = useState("http://127.0.0.1:7070/entries");
  const [liveState, setLiveState] = useState<"idle" | "connecting" | "connected" | "reconnecting">("idle");
  const [liveDropped, setLiveDropped] = useState(0);
  const [liveEvicted, setLiveEvicted] = useState(0);
  const [liveProtectionFailures, setLiveProtectionFailures] = useState(0);
  const [liveError, setLiveError] = useState<string | null>(null);
  const [sidebarWidth, setSidebarWidth] = useState(() => {
    const stored = Number.parseFloat(window.localStorage.getItem(sidebarStorageKey) ?? "");
    return Number.isFinite(stored) ? clampSidebarWidth(stored) : sidebarDefaultWidth;
  });
  const fileRef = useRef<HTMLInputElement>(null);
  const dragDepth = useRef(0);
  const liveSource = useRef<EventSource | null>(null);
  const liveReconnectTimer = useRef<number | null>(null);
  const liveConnectionGeneration = useRef(0);
  const nextLiveId = useRef(0);
  const protectionKeysRef = useRef<ReadonlyMap<string, string>>(new Map());
  const activeProtectionKeysRef = useRef<Map<string, string>>(new Map());
  const protectionSessionEpoch = useRef(0);
  const liveEntryTokensRef = useRef<Map<number, ReadonlySet<string>>>(new Map());
  const liveTokenReferencesRef = useRef<Map<string, number>>(new Map());

  useEffect(() => {
    window.localStorage.setItem(sidebarStorageKey, String(sidebarWidth));
  }, [sidebarWidth]);

  const resizeSidebar = useCallback((event: PointerEvent<HTMLDivElement>) => {
    if (event.type === "pointerdown") {
      if (event.pointerType === "mouse" && event.button !== 0) return;
      event.currentTarget.setPointerCapture(event.pointerId);
    }
    setSidebarWidth(clampSidebarWidth(event.clientX));
  }, []);

  const resizeSidebarWithKeyboard = useCallback((event: KeyboardEvent<HTMLDivElement>) => {
    let next: number | null = null;
    if (event.key === "ArrowLeft") next = sidebarWidth - 24;
    if (event.key === "ArrowRight") next = sidebarWidth + 24;
    if (event.key === "Home") next = sidebarMinWidth;
    if (event.key === "End") next = sidebarMaxWidth;
    if (next == null) return;

    event.preventDefault();
    setSidebarWidth(clampSidebarWidth(next));
  }, [sidebarWidth]);

  const clearDragging = useCallback(() => {
    dragDepth.current = 0;
    setDragging(false);
  }, []);

  const resetProtectionData = useCallback(() => {
    protectionSessionEpoch.current += 1;
    protectionKeysRef.current = new Map();
    activeProtectionKeysRef.current.clear();
    setResolvedValues(new Map());
    setProtectionKeys(new Map());
    setProtectionClearEpoch((current) => current + 1);
  }, []);

  const updateProtectionKey = useCallback((group: string, value: string) => {
    const next = new Map(protectionKeysRef.current);
    next.set(group, value);
    protectionKeysRef.current = next;
    setProtectionKeys(next);

    if (activeProtectionKeysRef.current.get(group) !== value) {
      activeProtectionKeysRef.current.delete(group);
    }
  }, []);

  const activateProtectionKey = useCallback((group: string, value: string) => {
    activeProtectionKeysRef.current.set(group, value);
  }, []);

  const clearLiveTokenTracking = useCallback(() => {
    liveEntryTokensRef.current.clear();
    liveTokenReferencesRef.current.clear();
  }, []);

  const disconnectLive = useCallback(() => {
    liveConnectionGeneration.current += 1;
    if (liveReconnectTimer.current != null) {
      window.clearTimeout(liveReconnectTimer.current);
      liveReconnectTimer.current = null;
    }
    liveSource.current?.close();
    liveSource.current = null;
    setLiveState("idle");
  }, []);

  const loadText = useCallback((name: string, text: string) => {
    disconnectLive();
    try {
      const loaded = parseHar(text);
      setDoc({ name, loaded });
      setLoadError(null);
      setFilters(emptyFilters);
      setSelectedId(loaded.entries.length > 0 ? loaded.entries[0].id : null);
      setSelectedTraceId(null);
      clearLiveTokenTracking();
      resetProtectionData();
    } catch (err) {
      if (err instanceof HarParseError) {
        setLoadError({ message: err.message, detail: err.detail });
      } else {
        setLoadError({ message: "Unexpected error while loading the file.", detail: String(err) });
      }
    }
  }, [clearLiveTokenTracking, disconnectLive, resetProtectionData]);

  const connectLive = useCallback(() => {
    let endpoint: URL;
    try {
      endpoint = validateDebugStreamURL(liveURL);
    } catch (error) {
      setLiveError(error instanceof Error ? error.message : String(error));
      return;
    }

    disconnectLive();
    replaceDeepLink(null);
    setDoc(null);
    setLoadError(null);
    setFilters(emptyFilters);
    setSelectedId(null);
    setSelectedTraceId(null);
    clearLiveTokenTracking();
    resetProtectionData();
    setLiveDropped(0);
    setLiveEvicted(0);
    setLiveProtectionFailures(0);
    setLiveError(null);
    setLiveState("connecting");
    nextLiveId.current = 0;

    const generation = liveConnectionGeneration.current;
    let reconnectAttempt = 0;

    const openSource = () => {
      if (liveConnectionGeneration.current !== generation) return;

      const source = new EventSource(endpoint);
      liveSource.current = source;

      source.addEventListener("ready", () => {
        if (liveSource.current !== source || liveConnectionGeneration.current !== generation) return;
        reconnectAttempt = 0;
        setLiveState("connected");
      });
      source.addEventListener("gap", (event) => {
        if (liveSource.current !== source || liveConnectionGeneration.current !== generation) return;
      if (!(event instanceof MessageEvent)) return;
      try {
        const payload = JSON.parse(String(event.data)) as { dropped?: unknown };
        const dropped = payload.dropped;
        if (typeof dropped === "number" && Number.isSafeInteger(dropped) && dropped > 0) {
          setLiveDropped((current) => current + dropped);
        }
      } catch {
        setLiveError("The live stream sent an invalid gap event.");
      }
      });
      source.addEventListener("entry", (event) => {
      if (!(event instanceof MessageEvent)) return;
      if (liveSource.current !== source || liveConnectionGeneration.current !== generation) return;
      try {
        const entry = parseLiveEntry(String(event.data), nextLiveId.current);
        nextLiveId.current += 1;
        const entryTokens = new Set(protectedOccurrences(entry.e).map((occurrence) => occurrence.token));
        liveEntryTokensRef.current.set(entry.id, entryTokens);
        for (const token of entryTokens) {
          liveTokenReferencesRef.current.set(token, (liveTokenReferencesRef.current.get(token) ?? 0) + 1);
        }
        setDoc((current) => {
          const previous = current?.loaded.format === "live" ? current.loaded : null;
          const allRawEntries: HarEntry[] = [...(previous?.har.log.entries ?? []), entry.e];
          const allEntries = [...(previous?.entries ?? []), entry];
          const overflow = Math.max(0, allEntries.length - liveEntryLimit);
          const rawEntries = overflow > 0 ? allRawEntries.slice(overflow) : allRawEntries;
          const entries = overflow > 0 ? allEntries.slice(overflow) : allEntries;
          if (overflow > 0) {
            for (const removed of allEntries.slice(0, overflow)) {
              for (const token of liveEntryTokensRef.current.get(removed.id) ?? []) {
                const references = (liveTokenReferencesRef.current.get(token) ?? 1) - 1;
                if (references > 0) liveTokenReferencesRef.current.set(token, references);
                else liveTokenReferencesRef.current.delete(token);
              }
              liveEntryTokensRef.current.delete(removed.id);
            }
            setResolvedValues((currentValues) => new Map(
              [...currentValues].filter(([token]) => liveTokenReferencesRef.current.has(token)),
            ));
            setLiveEvicted((count) => count + overflow);
            setSelectedId((selected) => selected != null && !entries.some((candidate) => candidate.id === selected) ? entries[0]?.id ?? null : selected);
          }
          return {
            name: endpoint.toString(),
            loaded: {
              format: "live",
              entries,
              har: {
                log: {
                  version: "1.2",
                  creator: { name: "github.com/mgurevin/recorder/inspector", version: "live" },
                  entries: rawEntries,
                  comment: "Ephemeral live view from DebugStreamRecorder.",
                },
              },
            },
          };
        });
        setSelectedId((current) => current ?? entry.id);
        setLiveError(null);

        const operationEpoch = protectionSessionEpoch.current;
        const activeKeys = new Map(activeProtectionKeysRef.current);
        if (activeKeys.size > 0) {
          void decryptLiveEntry(entry.e, activeKeys).then((result) => {
            if (liveSource.current !== source || protectionSessionEpoch.current !== operationEpoch || !liveEntryTokensRef.current.has(entry.id)) return;
            if (result.values.size > 0) {
              setResolvedValues((current) => new Map([...current, ...result.values]));
            }
            if (result.failures > 0) {
              setLiveProtectionFailures((current) => current + result.failures);
            }
          }).catch(() => {
            if (liveSource.current === source && protectionSessionEpoch.current === operationEpoch) {
              setLiveProtectionFailures((current) => current + 1);
            }
          });
        }
      } catch (error) {
        setLiveError(error instanceof Error ? error.message : String(error));
      }
      });
      source.onerror = () => {
        if (liveSource.current !== source || liveConnectionGeneration.current !== generation) return;

        source.close();
        liveSource.current = null;
        setLiveState("reconnecting");
        const delay = liveReconnectDelay(reconnectAttempt);
        reconnectAttempt += 1;
        liveReconnectTimer.current = window.setTimeout(() => {
          liveReconnectTimer.current = null;
          openSource();
        }, delay);
      };
    };

    openSource();
  }, [clearLiveTokenTracking, disconnectLive, liveURL, resetProtectionData]);

  useEffect(() => () => {
    liveConnectionGeneration.current += 1;
    if (liveReconnectTimer.current != null) window.clearTimeout(liveReconnectTimer.current);
    liveSource.current?.close();
  }, []);

  const loadFile = useCallback(
    (file: File) => {
      replaceDeepLink(null);
      file
        .text()
        .then((text) => loadText(file.name, text))
        .catch((err) => setLoadError({ message: "Could not read the file.", detail: String(err) }));
    },
    [loadText],
  );

  const loadSample = useCallback((updateDeepLink = true) => {
    if (updateDeepLink) replaceDeepLink("sample");
    loadText("sample.har (built-in)", JSON.stringify(sampleHar));
  }, [loadText]);

  // Deep links: ?sample loads the built-in sample; ?har=<HTTPS URL> fetches
  // a direct HAR/NDJSON capture or converts a normal gist.github.com share URL to raw.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const remote = params.get("har");
    if (remote) {
      const controller = new AbortController();
      setLoadingRemote(true);
      setLoadError(null);
      void fetchRemoteHar(remote, controller.signal)
        .then((result) => loadText(result.name, result.text))
        .catch((error) => {
          if (error instanceof DOMException && error.name === "AbortError") return;
          setLoadError({ message: "Could not load the remote capture.", detail: error instanceof Error ? error.message : String(error) });
        })
        .finally(() => setLoadingRemote(false));
      return () => controller.abort();
    }
    if (params.has("sample")) {
      loadSample(false);
    }
  }, [loadSample, loadText]);

  // Browsers do not consistently deliver a final dragleave to the app when a
  // file is pulled back out of the window. Global terminal events ensure the
  // overlay cannot remain stuck after an aborted drag.
  useEffect(() => {
    const handleWindowDragLeave = (event: DragEvent) => {
      const outsideViewport =
        event.clientX <= 0 ||
        event.clientY <= 0 ||
        event.clientX >= window.innerWidth ||
        event.clientY >= window.innerHeight;
      if (outsideViewport) clearDragging();
    };
    window.addEventListener("dragleave", handleWindowDragLeave);
    window.addEventListener("dragend", clearDragging);
    window.addEventListener("drop", clearDragging);
    window.addEventListener("blur", clearDragging);
    return () => {
      window.removeEventListener("dragleave", handleWindowDragLeave);
      window.removeEventListener("dragend", clearDragging);
      window.removeEventListener("drop", clearDragging);
      window.removeEventListener("blur", clearDragging);
    };
  }, [clearDragging]);

  const clearLiveEntries = useCallback(() => {
    protectionSessionEpoch.current += 1;
    clearLiveTokenTracking();
    setResolvedValues(new Map());
    setSelectedId(null);
    setSelectedTraceId(null);
    setLiveDropped(0);
    setLiveEvicted(0);
    setLiveProtectionFailures(0);
    setLiveError(null);
    setDoc((current) => {
      if (current?.loaded.format !== "live") return current;

      return {
        ...current,
        loaded: {
          ...current.loaded,
          entries: [],
          har: {
            ...current.loaded.har,
            log: {
              ...current.loaded.har.log,
              entries: [],
            },
          },
        },
      };
    });
  }, [clearLiveTokenTracking]);

  const entries = useMemo(() => doc?.loaded.entries ?? [], [doc?.loaded.entries]);
  const filtered = useMemo(() => applyFilters(entries, filters), [entries, filters]);
  const sorted = useMemo(() => sortEntries(filtered, sortKey, sortDesc), [filtered, sortKey, sortDesc]);
  const groups = useMemo(() => (grouped ? groupByTrace(sorted) : null), [grouped, sorted]);
  const selected: NEntry | null = useMemo(
    () => entries.find((e) => e.id === selectedId) ?? null,
    [entries, selectedId],
  );
  const selectedGroup = useMemo(
    () => groups?.find((group) => group.traceId === selectedTraceId) ?? null,
    [groups, selectedTraceId],
  );

  return (
    <>
      <TooltipLayer />
      <div
        className={`app ${dragging ? "dragging" : ""} ${selected || selectedGroup ? "has-selection" : ""}`}
        onDragEnter={(e) => {
          if (!e.dataTransfer.types.includes("Files")) return;
          e.preventDefault();
          dragDepth.current += 1;
          setDragging(true);
        }}
        onDragOver={(e) => {
          if (!e.dataTransfer.types.includes("Files")) return;
          e.preventDefault();
        }}
        onDragLeave={(e) => {
          e.preventDefault();
          dragDepth.current = Math.max(0, dragDepth.current - 1);
          if (dragDepth.current === 0) setDragging(false);
        }}
        onDrop={(e) => {
          e.preventDefault();
          clearDragging();
          const file = e.dataTransfer.files?.[0];
          if (file) loadFile(file);
        }}
      >
      <a className="skip-link" href="#request-list">Skip to request list</a>
      <a className="skip-link" href="#evidence-panel">Skip to evidence panel</a>
      <header className="topbar">
        <span className="brand mono">recorder<span className="brand-detail"> · HAR inspector</span></span>
        {doc ? (
          <span className="doc-name muted" title={doc.name}>
            {doc.name} · {doc.loaded.format.toUpperCase()} · {entries.length} entries
          </span>
        ) : null}
        <span className="spacer" />
        <AppearanceControls />
        <button type="button" className={`btn ${liveState !== "idle" ? "live-active" : ""}`} onClick={() => setLiveOpen((current) => !current)}>
          <Radio size={14} /> <span className="button-label">live</span>
        </button>
        <button type="button" className="btn" data-tooltip="Open HAR or NDJSON file" aria-label="Open HAR or NDJSON file" onClick={() => fileRef.current?.click()}>
          <FileUp size={14} /> <span className="button-label">open file</span>
        </button>
        <button type="button" className="btn" data-tooltip="Load built-in sample" aria-label="Load built-in sample" onClick={() => loadSample()}>
          <FlaskConical size={14} /> <span className="button-label">sample</span>
        </button>
        {/* Keep the picker unfiltered: macOS disables .ndjson for unknown MIME/UTI mappings. Content is validated after reading. */}
        <input
          ref={fileRef}
          type="file"
          hidden
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file) loadFile(file);
            e.target.value = "";
          }}
        />
      </header>

      {liveOpen ? (
        <section className="live-connect" aria-label="Live debug stream">
          <div className="live-copy">
            <strong>Local debug stream</strong>
            <span className="muted">Connect to one loopback-only DebugStreamRecorder SSE endpoint. Starting a connection clears the current capture.</span>
          </div>
          <label className="live-url">
            <span className="sr-only">Debug stream URL</span>
            <input
              className="input mono"
              value={liveURL}
              onChange={(event) => setLiveURL(event.target.value)}
              disabled={liveState !== "idle"}
              onKeyDown={(event) => {
                if (event.key === "Enter" && liveState === "idle") connectLive();
              }}
            />
          </label>
          {liveState === "idle" ? (
            <button type="button" className="btn primary" onClick={connectLive}>connect</button>
          ) : (
            <button type="button" className="btn" onClick={disconnectLive}>disconnect</button>
          )}
          <span className={`live-status ${liveState}`}>{liveState}</span>
          {doc?.loaded.format === "live" && entries.length > 0 ? (
            <button type="button" className="btn" onClick={clearLiveEntries} title="Clear listed live exchanges and trace chains">
              <Trash2 size={14} /> clear entries
            </button>
          ) : null}
          {liveDropped > 0 ? <span className="live-gap">{liveDropped} missed</span> : null}
          {liveEvicted > 0 ? <span className="live-gap">{liveEvicted} old removed</span> : null}
          {liveProtectionFailures > 0 ? <span className="live-gap">{liveProtectionFailures} decrypt failed</span> : null}
          <button type="button" className="icon-btn" aria-label="Close live connection panel" onClick={() => setLiveOpen(false)}>
            <X size={15} />
          </button>
          {liveError ? <div className="live-error mono">{liveError}</div> : null}
        </section>
      ) : null}

      {loadError ? (
        <div className="load-error">
          <strong>{loadError.message}</strong>
          {loadError.detail ? <div className="mono muted">{loadError.detail}</div> : null}
        </div>
      ) : null}

      {!doc ? (
        <div className="welcome">
          <div className="welcome-card">
            <h1>{loadingRemote ? "Loading remote capture…" : "Inspect recorder HAR and NDJSON files"}</h1>
            <p>
              Drop a <span className="mono">.har</span> or <span className="mono">.ndjson</span> file anywhere, open one,
              connect to a local live stream, or start with the built-in sample. Every HAR 1.2 field and every <span className="mono">_</span>
              extension produced by the recorder library is shown in full detail.
            </p>
            <div className="welcome-actions">
              <button type="button" className="btn primary" onClick={() => fileRef.current?.click()}>
                <FileUp size={15} /> open a capture file
              </button>
              <button type="button" className="btn" onClick={() => loadSample()}>
                <FlaskConical size={15} /> load sample data
              </button>
            </div>
            <p className="muted security-note">
              <ShieldCheck size={13} /> Capture files can contain sensitive data. This inspector runs entirely in your
              browser; nothing is uploaded anywhere.
            </p>
          </div>
        </div>
      ) : (
        <div className="layout" style={{ "--sidebar-current-width": `${sidebarWidth}px` } as CSSProperties}>
          <aside className="sidebar" id="request-list" tabIndex={-1} aria-label="Recorded request list">
            <Filters
              entries={entries}
              filters={filters}
              onChange={setFilters}
              sortKey={sortKey}
              sortDesc={sortDesc}
              onSort={(k, d) => {
                setSortKey(k);
                setSortDesc(d);
              }}
              grouped={grouped}
              onGrouped={(value) => {
                setGrouped(value);
                if (!value && selectedGroup) {
                  setSelectedTraceId(null);
                  setSelectedId(selectedGroup.entries[0]?.id ?? null);
                }
              }}
              shown={filtered.length}
              total={entries.length}
            />
            <EntryList
              entries={sorted}
              groups={groups}
              selectedId={selectedTraceId ? null : selectedId}
              selectedTraceId={selectedTraceId}
              onSelect={(id) => {
                setSelectedTraceId(null);
                setSelectedId(id);
              }}
              onSelectGroup={(traceId) => {
                setSelectedId(null);
                setSelectedTraceId(traceId);
              }}
            />
          </aside>
          <div
            className="sidebar-resizer"
            role="separator"
            aria-label="Resize request list"
            aria-orientation="vertical"
            aria-valuemin={sidebarMinWidth}
            aria-valuemax={sidebarMaxWidth}
            aria-valuenow={sidebarWidth}
            tabIndex={0}
            onDoubleClick={() => setSidebarWidth(sidebarDefaultWidth)}
            onPointerDown={resizeSidebar}
            onPointerMove={(event) => {
              if (event.currentTarget.hasPointerCapture(event.pointerId)) resizeSidebar(event);
            }}
            onKeyDown={resizeSidebarWithKeyboard}
            data-tooltip="Drag to resize; double-click to reset"
          />
          <main className="main" id="evidence-panel" tabIndex={-1}>
            {selectedGroup ? (
              <TraceGroupPanel
                key={selectedGroup.traceId}
                group={selectedGroup}
                onBack={() => setSelectedTraceId(null)}
                onSelectEntry={(id) => {
                  setSelectedTraceId(null);
                  setSelectedId(id);
                }}
              />
            ) : selected ? (
              <DetailPanel
                entry={selected}
                entries={entries}
                resolvedValues={resolvedValues}
                onResolved={(values) => setResolvedValues((current) => new Map([...current, ...values]))}
                onClearResolved={resetProtectionData}
                protectionClearEpoch={protectionClearEpoch}
                keyInputs={protectionKeys}
                onKeyInput={updateProtectionKey}
                onProtectionKeyActivated={activateProtectionKey}
                onBack={() => setSelectedId(null)}
              />
            ) : (
              <div className="empty-state big">select an exchange to inspect</div>
            )}
          </main>
        </div>
      )}
      {dragging ? <div className="drop-overlay">drop the HAR or NDJSON file to load it</div> : null}
      </div>
    </>
  );
}
