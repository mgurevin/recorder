export const sidebarMinWidth = 320;
export const sidebarMaxWidth = 720;
export const sidebarDefaultWidth = 420;

export function clampSidebarWidth(value: number): number {
  if (!Number.isFinite(value)) return sidebarDefaultWidth;

  return Math.min(sidebarMaxWidth, Math.max(sidebarMinWidth, Math.round(value)));
}
