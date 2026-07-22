import { useEffect, useRef, useState, type ReactNode } from "react";

/**
 * VirtualList renders only the visible window of fixed-height rows so large
 * HAR files stay responsive. No external dependency needed for the simple
 * fixed-height case.
 */
export function VirtualList<T>({
  rows,
  rowHeight,
  render,
  overscan = 8,
  activeIndex,
  ariaLabel,
}: {
  rows: T[];
  rowHeight: number;
  render: (row: T, index: number) => ReactNode;
  overscan?: number;
  activeIndex?: number;
  ariaLabel?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [height, setHeight] = useState(600);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setHeight(el.clientHeight));
    ro.observe(el);
    setHeight(el.clientHeight);
    return () => ro.disconnect();
  }, []);

  useEffect(() => {
    const el = ref.current;
    if (!el || activeIndex == null || activeIndex < 0) return;
    const shouldRestoreFocus = el.contains(document.activeElement);
    const top = activeIndex * rowHeight;
    const bottom = top + rowHeight;
    if (top < el.scrollTop) el.scrollTop = top;
    else if (bottom > el.scrollTop + el.clientHeight) el.scrollTop = bottom - el.clientHeight;
    if (!shouldRestoreFocus) return;

    const frame = window.requestAnimationFrame(() => {
      el.querySelector<HTMLElement>('[role="option"][aria-selected="true"]')?.focus();
    });
    return () => window.cancelAnimationFrame(frame);
  }, [activeIndex, rowHeight]);

  const total = rows.length * rowHeight;
  const first = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const last = Math.min(rows.length, Math.ceil((scrollTop + height) / rowHeight) + overscan);

  return (
    <div className="vlist" ref={ref} role="listbox" aria-label={ariaLabel} onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}>
      <div style={{ height: total, position: "relative" }}>
        {rows.slice(first, last).map((row, i) => (
          <div
            key={first + i}
            style={{ position: "absolute", top: (first + i) * rowHeight, left: 0, right: 0, height: rowHeight }}
          >
            {render(row, first + i)}
          </div>
        ))}
      </div>
    </div>
  );
}
