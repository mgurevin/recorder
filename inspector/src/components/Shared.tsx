import { useEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Check, ChevronDown, ChevronRight, Copy } from "lucide-react";
import type { HarCookie, NameValue } from "../types/har";
import { statusTone } from "../lib/format";

export function CopyButton({ text, label }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      className="copy-btn"
      data-tooltip="Copy to clipboard"
      onClick={() => {
        void navigator.clipboard?.writeText(text).then(() => {
          setDone(true);
          setTimeout(() => setDone(false), 1200);
        });
      }}
    >
      {done ? <Check size={13} /> : <Copy size={13} />}
      {label ? <span>{done ? "copied" : label}</span> : null}
    </button>
  );
}

/** Fast, unclipped tooltips for elements carrying a data-tooltip attribute. */
export function TooltipLayer() {
  const [tip, setTip] = useState<{ text: string; x: number; y: number; above: boolean }>();
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => {
    const hide = () => {
      window.clearTimeout(timer.current);
      setTip(undefined);
    };
    const show = (target: HTMLElement, delay: number) => {
      const text = target.dataset.tooltip;
      if (!text) return;
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => {
        const rect = target.getBoundingClientRect();
        const above = window.innerHeight - rect.bottom < 70;
        setTip({
          text,
          x: Math.min(Math.max(rect.left + rect.width / 2, 170), window.innerWidth - 170),
          y: above ? rect.top - 8 : rect.bottom + 8,
          above,
        });
      }, delay);
    };
    const trigger = (event: Event) => (event.target as Element | null)?.closest<HTMLElement>("[data-tooltip]");
    const onPointerOver = (event: PointerEvent) => {
      const target = trigger(event);
      if (target) show(target, 120);
    };
    const onPointerOut = (event: PointerEvent) => {
      const target = trigger(event);
      if (!target || !event.relatedTarget || !target.contains(event.relatedTarget as Node)) hide();
    };
    const onFocusIn = (event: FocusEvent) => {
      const target = trigger(event);
      if (target) show(target, 0);
    };
    document.addEventListener("pointerover", onPointerOver);
    document.addEventListener("pointerout", onPointerOut);
    document.addEventListener("focusin", onFocusIn);
    document.addEventListener("focusout", hide);
    window.addEventListener("scroll", hide, true);
    return () => {
      hide();
      document.removeEventListener("pointerover", onPointerOver);
      document.removeEventListener("pointerout", onPointerOut);
      document.removeEventListener("focusin", onFocusIn);
      document.removeEventListener("focusout", hide);
      window.removeEventListener("scroll", hide, true);
    };
  }, []);

  if (!tip) return null;
  return createPortal(
    <div
      className={`tooltip ${tip.above ? "above" : "below"}`}
      role="tooltip"
      style={{ left: tip.x, top: tip.y }}
    >
      {tip.text}
    </div>,
    document.body,
  );
}

export function Section({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="section">
      <div className="section-head">
        <h3>{title}</h3>
        <div className="section-actions">{actions}</div>
      </div>
      {children}
    </section>
  );
}

export function EmptyState({ text }: { text: string }) {
  return <div className="empty-state">{text}</div>;
}

export function SegmentedControl<T extends string>({ label, value, options, onChange }: {
  label: string;
  value: T;
  options: readonly T[];
  onChange: (value: T) => void;
}) {
  return (
    <div className="segmented-control" role="tablist" aria-label={label}>
      {options.map((option) => (
        <button
          type="button"
          role="tab"
          aria-selected={value === option}
          className={value === option ? "active" : ""}
          key={option}
          onClick={() => onChange(option)}
        >
          {option}
        </button>
      ))}
    </div>
  );
}

/** KV renders a definition grid of label/value rows, skipping empty values. */
export function KV({ rows }: { rows: Array<[string, ReactNode]> }) {
  const visible = rows.filter(([, v]) => v !== null && v !== undefined && v !== "");
  if (visible.length === 0) return <EmptyState text="nothing recorded" />;
  return (
    <div className="kv">
      {visible.map(([k, v], i) => (
        <div className="kv-row" key={`${k}-${i}`}>
          <div className="kv-key">{k}</div>
          <div className="kv-val">{v}</div>
        </div>
      ))}
    </div>
  );
}

export function PairsTable({ pairs }: { pairs: NameValue[] | undefined }) {
  if (!pairs || pairs.length === 0) return <EmptyState text="none" />;
  const asText = pairs.map((p) => `${p.name}: ${p.value}`).join("\n");
  return (
    <div className="pairs">
      <div className="pairs-toolbar">
        <CopyButton text={asText} label="copy all" />
      </div>
      <table className="pairs-table">
        <tbody>
          {pairs.map((p, i) => (
            <tr key={i}>
              <td className="pair-name">{p.name}</td>
              <td className="pair-value mono">{p.value}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function CookiesTable({ cookies }: { cookies: HarCookie[] | undefined }) {
  if (!cookies || cookies.length === 0) return <EmptyState text="none" />;
  return (
    <table className="pairs-table">
      <thead>
        <tr>
          <th>name</th>
          <th>value</th>
          <th>attributes</th>
        </tr>
      </thead>
      <tbody>
        {cookies.map((c, i) => (
          <tr key={i}>
            <td className="pair-name">{c.name}</td>
            <td className="pair-value mono">{c.value}</td>
            <td className="muted">
              {[
                c.path && `path=${c.path}`,
                c.domain && `domain=${c.domain}`,
                c.expires && `expires=${c.expires}`,
                c.httpOnly && "httpOnly",
                c.secure && "secure",
              ]
                .filter(Boolean)
                .join("; ")}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function CodeBlock({
  text,
  copyText,
  note,
  language,
}: {
  text: string;
  copyText?: string;
  note?: string;
  language?: "json" | "xml" | "shell";
}) {
  return (
    <div className="codeblock">
      <div className="codeblock-toolbar">
        {note ? <span className="muted">{note}</span> : <span />}
        <CopyButton text={copyText ?? text} label="copy" />
      </div>
      <pre className="mono">{language ? <HighlightedCode text={text} language={language} /> : text}</pre>
    </div>
  );
}

function HighlightedCode({ text, language }: { text: string; language: "json" | "xml" | "shell" }) {
  return <>{language === "json" ? highlightJson(text) : language === "xml" ? highlightXml(text) : highlightShell(text)}</>;
}

function highlightShell(text: string): ReactNode[] {
  const pattern = /(^curl\b)|(--[a-z-]+)|('(?:[^']|'\\'''')*')|(\\(?=\n))/gm;
  const out: ReactNode[] = [];
  let last = 0;
  for (const match of text.matchAll(pattern)) {
    const index = match.index;
    if (index > last) out.push(text.slice(last, index));
    const tone = match[1] ? "command" : match[2] ? "option" : match[3] ? "string" : "continuation";
    out.push(<span className={`syntax-${tone}`} key={`${index}-${tone}`}>{match[0]}</span>);
    last = index + match[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

function highlightJson(text: string): ReactNode[] {
  const pattern = /("(?:\\.|[^"\\])*")(?=\s*:)|("(?:\\.|[^"\\])*")|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false)\b|\b(null)\b|([{}[\],:])/g;
  const out: ReactNode[] = [];
  let last = 0;
  for (const match of text.matchAll(pattern)) {
    const index = match.index;
    if (index > last) out.push(text.slice(last, index));
    const tone = match[1]
      ? "key"
      : match[2]
        ? "string"
        : match[3]
          ? "number"
          : match[4]
            ? "boolean"
            : match[5]
              ? "null"
              : "punctuation";
    out.push(<span className={`syntax-${tone}`} key={`${index}-${tone}`}>{match[0]}</span>);
    last = index + match[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

function highlightXml(text: string): ReactNode[] {
  const pattern = /<!--[\s\S]*?-->|<!\[CDATA\[[\s\S]*?\]\]>|<[^>]+>/g;
  const out: ReactNode[] = [];
  let last = 0;
  for (const match of text.matchAll(pattern)) {
    const index = match.index;
    if (index > last) out.push(text.slice(last, index));
    const token = match[0];
    const tone = token.startsWith("<!--") ? "comment" : token.startsWith("<![CDATA[") ? "cdata" : "tag";
    out.push(<span className={`syntax-${tone}`} key={`${index}-${tone}`}>{token}</span>);
    last = index + token.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

export function StatusBadge({ status }: { status: number }) {
  return (
    <span
      className={`badge status-${statusTone(status)}`}
      data-tooltip={status > 0 ? `HTTP status ${status}` : "No HTTP response received"}
    >
      {status > 0 ? status : "ERR"}
    </span>
  );
}

export function MethodBadge({ method }: { method: string }) {
  return <span className="badge method" data-tooltip={`HTTP method: ${method}`}>{method}</span>;
}

export function StateBadge({ state }: { state: string }) {
  if (!state) return null;
  const tone = state === "completed" ? "ok" : state === "failed" ? "server" : "client";
  return <span className={`badge state-${tone}`} data-tooltip={`Recorder state: ${state}`}>{state}</span>;
}

export function BoolMark({ v }: { v: boolean | undefined }) {
  return <span className={v ? "bool-yes" : "bool-no"}>{v ? "yes" : "no"}</span>;
}

/** JsonTree renders arbitrary JSON with collapsible nodes. */
export function JsonTree({ value, name, depth = 0 }: { value: unknown; name?: string; depth?: number }) {
  const [open, setOpen] = useState(depth < 2);
  const label = name !== undefined ? <span className="jt-key">{name}: </span> : null;

  if (value === null) return <div className="jt-line" style={indent(depth)}>{label}<span className="jt-null">null</span></div>;
  if (typeof value === "string")
    return <div className="jt-line" style={indent(depth)}>{label}<span className="jt-str">"{truncate(value)}"</span></div>;
  if (typeof value === "number" || typeof value === "boolean")
    return <div className="jt-line" style={indent(depth)}>{label}<span className="jt-num">{String(value)}</span></div>;

  const isArray = Array.isArray(value);
  const entries = isArray
    ? (value as unknown[]).map((v, i) => [String(i), v] as const)
    : Object.entries(value as Record<string, unknown>);
  const preview = isArray ? `[${entries.length}]` : `{${entries.length}}`;
  return (
    <div>
      <div className="jt-line jt-toggle" style={indent(depth)} onClick={() => setOpen(!open)}>
        {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
        {label}
        <span className="jt-preview">{preview}</span>
      </div>
      {open &&
        entries.map(([k, v]) => (
          <JsonTree key={k} name={isArray ? undefined : k} value={v} depth={depth + 1} />
        ))}
    </div>
  );
}

function indent(depth: number) {
  return { paddingLeft: `${depth * 14 + 4}px` };
}

function truncate(s: string, n = 400): string {
  return s.length > n ? `${s.slice(0, n)}… (${s.length} chars)` : s;
}
