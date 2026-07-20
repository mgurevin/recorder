import { useRef, useState, type PointerEvent } from "react";
import { formatTimelineCursor } from "../lib/format";

interface CursorState {
  left: number;
  top: number;
  height: number;
  offsetMs: number;
  edge: "start" | "middle" | "end";
}

export function useTimelineCursor(scaleMs: number, trackSelector: string) {
  const ref = useRef<HTMLDivElement>(null);
  const [cursor, setCursor] = useState<CursorState>();

  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const container = ref.current;
    if (!container) return;
    const tracks = [...container.querySelectorAll<HTMLElement>(trackSelector)];
    if (tracks.length === 0) return;
    const first = tracks[0].getBoundingClientRect();
    const last = tracks[tracks.length - 1].getBoundingClientRect();
    if (event.clientX < first.left || event.clientX > first.right || event.clientY < first.top || event.clientY > last.bottom) {
      setCursor(undefined);
      return;
    }
    const containerRect = container.getBoundingClientRect();
    const ratio = Math.min(1, Math.max(0, (event.clientX - first.left) / first.width));
    setCursor({
      left: first.left - containerRect.left + ratio * first.width,
      top: first.top - containerRect.top,
      height: last.bottom - first.top,
      offsetMs: ratio * Math.max(scaleMs, 0),
      edge: ratio < 0.08 ? "start" : ratio > 0.92 ? "end" : "middle",
    });
  };

  return {
    ref,
    cursor,
    onPointerMove,
    onPointerLeave: () => setCursor(undefined),
  };
}

export function TimelineCursor({ cursor }: { cursor: CursorState | undefined }) {
  if (!cursor) return null;
  return (
    <span
      className="timeline-cursor"
      style={{ left: cursor.left, top: cursor.top, height: cursor.height }}
      aria-hidden="true"
    >
      <span className={`timeline-cursor-label ${cursor.edge}`}>{formatTimelineCursor(cursor.offsetMs)}</span>
    </span>
  );
}
