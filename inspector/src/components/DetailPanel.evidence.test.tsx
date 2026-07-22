// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { parseHar } from "../lib/parse";
import { maximalEvidenceHar } from "../test/maximalEvidenceHar";
import { DetailPanel } from "./DetailPanel";

afterEach(cleanup);

function renderEvidencePanel() {
  const loaded = parseHar(JSON.stringify(maximalEvidenceHar));
  render(
    <DetailPanel
      entry={loaded.entries[0]}
      entries={loaded.entries}
      har={loaded.har}
      resolvedValues={new Map()}
      onResolved={() => undefined}
      onClearResolved={() => undefined}
      protectionClearEpoch={0}
      keyInputs={new Map()}
      onKeyInput={() => undefined}
      onProtectionKeyActivated={() => undefined}
      onBack={() => undefined}
    />,
  );
}

async function selectTop(user: ReturnType<typeof userEvent.setup>, name: string) {
  await user.click(screen.getByRole("tab", { name }));
}

describe("DetailPanel evidence coverage", () => {
  it("exposes representative evidence from every structured workspace", async () => {
    const user = userEvent.setup();
    renderEvidencePanel();

    expect(screen.getByText("entry-comment-evidence")).toBeTruthy();
    expect(screen.getByText("page-evidence-id")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "HAR cache state" }).closest("section")?.textContent)
      .toContain("cache-comment-evidence");

    await selectTop(user, "Request");
    expect(screen.getByText("request-comment-evidence")).toBeTruthy();
    expect(screen.getByText("application/json")).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "Headers" }));
    expect(screen.getByText("request-header-comment-evidence")).toBeTruthy();
    expect(screen.getByText("request-trailer-evidence")).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "Parameters" }));
    expect(screen.getByText("query-comment-two")).toBeTruthy();
    expect(screen.getByText("request-cookie-comment-evidence")).toBeTruthy();

    await selectTop(user, "Response");
    expect(screen.getByText("identity-evidence")).toBeTruthy();
    expect(screen.getByText("response-comment-evidence")).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "Headers" }));
    expect(screen.getByText("Path=/; HttpOnly; SameSite=Lax; Priority=High; Partitioned")).toBeTruthy();
    expect(screen.getByText("response-trailer-evidence")).toBeTruthy();

    await selectTop(user, "Connection");
    expect(screen.getByText("timing-comment-evidence")).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "Network" }));
    expect(screen.getByText("put-idle-error-evidence", { exact: false })).toBeTruthy();
    expect(screen.getByText("informational-header-evidence")).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "TLS" }));
    expect(screen.getByText("certificate-issuer-evidence")).toBeTruthy();

    await selectTop(user, "Diagnostics");
    expect(screen.getByText("error-cause-evidence")).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "Trace" }));
    expect(screen.getByText("trace-detail-evidence")).toBeTruthy();

    await selectTop(user, "Privacy");
    expect(screen.getByText("total reported")).toBeTruthy();
  });

  it("keeps raw/formatted display and copy semantics aligned", async () => {
    const user = userEvent.setup();
    renderEvidencePanel();
    await selectTop(user, "Request");
    await user.click(screen.getByRole("tab", { name: "Body" }));

    const bodySection = screen.getByRole("heading", { name: "Body" }).closest("section");
    expect(bodySection).not.toBeNull();
    const raw = maximalEvidenceHar.log.entries[0].request.postData?.text ?? "";
    expect((bodySection as HTMLElement).querySelector("pre")?.textContent).toBe(raw);

    const writeText = vi.spyOn(navigator.clipboard, "writeText");
    const copyButton = within(bodySection as HTMLElement).getByRole("button", { name: "copy full" });
    await user.click(copyButton);
    expect(writeText).toHaveBeenLastCalledWith(raw);

    await user.click(within(bodySection as HTMLElement).getByRole("tab", { name: "Formatted" }));
    const formatted = JSON.stringify(JSON.parse(raw), null, 2);
    expect((bodySection as HTMLElement).querySelector("pre")?.textContent).toBe(formatted);
    await user.click(copyButton);
    expect(writeText).toHaveBeenLastCalledWith(formatted);
  });

  it("makes capture-level and future evidence inspectable", async () => {
    const user = userEvent.setup();
    renderEvidencePanel();
    await selectTop(user, "Raw");
    expect(screen.getByText("entry-extension-evidence", { exact: false })).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "Capture metadata" }));
    const metadata = screen.getByRole("heading", { name: "Capture metadata" }).closest("section");
    expect(metadata?.textContent).toContain("creator-comment-evidence");
    expect(metadata?.textContent).toContain("browser-comment-evidence");
    expect(metadata?.textContent).toContain("page-timing-comment-evidence");
    expect(metadata?.textContent).toContain("top-level-extension-evidence");
    expect(metadata?.textContent).not.toContain("entry-comment-evidence");
  });
});
