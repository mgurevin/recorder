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
}: {
  rows: T[];
  rowHeight: number;
  render: (row: T, index: number) => ReactNode;
  overscan?: number;
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

  const total = rows.length * rowHeight;
  const first = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const last = Math.min(rows.length, Math.ceil((scrollTop + height) / rowHeight) + overscan);

  return (
    <div className="vlist" ref={ref} onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}>
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
