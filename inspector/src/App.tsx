import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { FileUp, FlaskConical, ShieldCheck } from "lucide-react";
import type { NEntry } from "./types/har";
import { HarParseError, groupByTrace, parseHar, type LoadedHar } from "./lib/parse";
import { sampleHar } from "./sampleHar";
import { applyFilters, emptyFilters, Filters, sortEntries, type FilterState, type SortKey } from "./components/Filters";
import { EntryList } from "./components/EntryList";
import { DetailPanel } from "./components/DetailPanel";

interface Doc {
  name: string;
  loaded: LoadedHar;
}

export default function App() {
  const [doc, setDoc] = useState<Doc | null>(null);
  const [loadError, setLoadError] = useState<{ message: string; detail?: string } | null>(null);
  const [filters, setFilters] = useState<FilterState>(emptyFilters);
  const [sortKey, setSortKey] = useState<SortKey>("start");
  const [sortDesc, setSortDesc] = useState(false);
  const [grouped, setGrouped] = useState(false);
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [dragging, setDragging] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const loadText = useCallback((name: string, text: string) => {
    try {
      const loaded = parseHar(text);
      setDoc({ name, loaded });
      setLoadError(null);
      setFilters(emptyFilters);
      setSelectedId(loaded.entries.length > 0 ? loaded.entries[0].id : null);
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
      file
        .text()
        .then((text) => loadText(file.name, text))
        .catch((err) => setLoadError({ message: "Could not read the file.", detail: String(err) }));
    },
    [loadText],
  );

  const loadSample = useCallback(() => {
    loadText("sample.har (built-in)", JSON.stringify(sampleHar));
  }, [loadText]);

  // Deep link: /?sample starts with the built-in sample loaded (also used
  // to produce the documentation screenshot non-interactively).
  useEffect(() => {
    if (new URLSearchParams(window.location.search).has("sample")) {
      loadSample();
    }
  }, [loadSample]);

  const entries = doc?.loaded.entries ?? [];
  const filtered = useMemo(() => applyFilters(entries, filters), [entries, filters]);
  const sorted = useMemo(() => sortEntries(filtered, sortKey, sortDesc), [filtered, sortKey, sortDesc]);
  const groups = useMemo(() => (grouped ? groupByTrace(sorted) : null), [grouped, sorted]);
  const selected: NEntry | null = useMemo(
    () => entries.find((e) => e.id === selectedId) ?? null,
    [entries, selectedId],
  );

  return (
    <div
      className={`app ${dragging ? "dragging" : ""} ${selected ? "has-selection" : ""}`}
      onDragOver={(e) => {
        e.preventDefault();
        setDragging(true);
      }}
      onDragLeave={(e) => {
        if (e.currentTarget === e.target) setDragging(false);
      }}
      onDrop={(e) => {
        e.preventDefault();
        setDragging(false);
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
        <button type="button" className="btn" onClick={loadSample}>
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
            <h1>Inspect recorder HAR files</h1>
            <p>
              Drop a <span className="mono">.har</span> file anywhere, open one with the button above, or start with
              the built-in sample. Every HAR 1.2 field and every <span className="mono">_</span> extension produced by
              the recorder library is shown in full detail.
            </p>
            <div className="welcome-actions">
              <button type="button" className="btn primary" onClick={() => fileRef.current?.click()}>
                <FileUp size={15} /> open a HAR file
              </button>
              <button type="button" className="btn" onClick={loadSample}>
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
              onGrouped={setGrouped}
              shown={filtered.length}
              total={entries.length}
            />
            <EntryList entries={sorted} groups={groups} selectedId={selectedId} onSelect={setSelectedId} />
          </aside>
          <main className="main">
            {selected ? (
              <DetailPanel key={selected.id} entry={selected} onBack={() => setSelectedId(null)} />
            ) : (
              <div className="empty-state big">select an exchange to inspect</div>
            )}
          </main>
        </div>
      )}
      {dragging ? <div className="drop-overlay">drop the HAR file to load it</div> : null}
    </div>
  );
}
