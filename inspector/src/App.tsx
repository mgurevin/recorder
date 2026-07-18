import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { FileUp, FlaskConical, ShieldCheck } from "lucide-react";
import type { NEntry } from "./types/har";
import { HarParseError, groupByTrace, parseHar, type LoadedHar } from "./lib/parse";
import { sampleHar } from "./sampleHar";
import { applyFilters, emptyFilters, Filters, sortEntries, type FilterState, type SortKey } from "./components/Filters";
import { EntryList } from "./components/EntryList";
import { DetailPanel } from "./components/DetailPanel";
import { TooltipLayer } from "./components/Shared";
import { TraceGroupPanel } from "./components/TraceGroupPanel";
import { fetchRemoteHar } from "./lib/remoteHar";

interface Doc {
  name: string;
  loaded: LoadedHar;
}

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
  const fileRef = useRef<HTMLInputElement>(null);
  const dragDepth = useRef(0);

  const clearDragging = useCallback(() => {
    dragDepth.current = 0;
    setDragging(false);
  }, []);

  const loadText = useCallback((name: string, text: string) => {
    try {
      const loaded = parseHar(text);
      setDoc({ name, loaded });
      setLoadError(null);
      setFilters(emptyFilters);
      setSelectedId(loaded.entries.length > 0 ? loaded.entries[0].id : null);
      setSelectedTraceId(null);
    } catch (err) {
      if (err instanceof HarParseError) {
        setLoadError({ message: err.message, detail: err.detail });
      } else {
        setLoadError({ message: "Unexpected error while loading the file.", detail: String(err) });
      }
    }
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
  // a direct HAR or converts a normal gist.github.com share URL to raw.
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
          setLoadError({ message: "Could not load the remote HAR.", detail: error instanceof Error ? error.message : String(error) });
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

  const entries = doc?.loaded.entries ?? [];
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
      <header className="topbar">
        <span className="brand mono">recorder · HAR inspector</span>
        {doc ? (
          <span className="doc-name muted" title={doc.name}>
            {doc.name} · {entries.length} entries
          </span>
        ) : null}
        <span className="spacer" />
        <button type="button" className="btn" onClick={() => fileRef.current?.click()}>
          <FileUp size={14} /> open HAR
        </button>
        <button type="button" className="btn" onClick={() => loadSample()}>
          <FlaskConical size={14} /> sample
        </button>
        <input
          ref={fileRef}
          type="file"
          accept=".har,.json,application/json"
          hidden
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file) loadFile(file);
            e.target.value = "";
          }}
        />
      </header>

      {loadError ? (
        <div className="load-error">
          <strong>{loadError.message}</strong>
          {loadError.detail ? <div className="mono muted">{loadError.detail}</div> : null}
        </div>
      ) : null}

      {!doc ? (
        <div className="welcome">
          <div className="welcome-card">
            <h1>{loadingRemote ? "Loading remote HAR…" : "Inspect recorder HAR files"}</h1>
            <p>
              Drop a <span className="mono">.har</span> file anywhere, open one with the button above, or start with
              the built-in sample. Every HAR 1.2 field and every <span className="mono">_</span> extension produced by
              the recorder library is shown in full detail.
            </p>
            <div className="welcome-actions">
              <button type="button" className="btn primary" onClick={() => fileRef.current?.click()}>
                <FileUp size={15} /> open a HAR file
              </button>
              <button type="button" className="btn" onClick={() => loadSample()}>
                <FlaskConical size={15} /> load sample data
              </button>
            </div>
            <p className="muted security-note">
              <ShieldCheck size={13} /> HAR files can contain sensitive data. This inspector runs entirely in your
              browser; nothing is uploaded anywhere.
            </p>
          </div>
        </div>
      ) : (
        <div className="layout">
          <aside className="sidebar">
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
          <main className="main">
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
              <DetailPanel entry={selected} onBack={() => setSelectedId(null)} />
            ) : (
              <div className="empty-state big">select an exchange to inspect</div>
            )}
          </main>
        </div>
      )}
      {dragging ? <div className="drop-overlay">drop the HAR file to load it</div> : null}
      </div>
    </>
  );
}
