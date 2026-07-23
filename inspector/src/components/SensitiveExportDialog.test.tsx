// @vitest-environment jsdom

import { useState } from "react";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SensitiveExportDialog } from "./SensitiveExportDialog";

const summary = {
  resolvedLocations: 7,
  uniqueResolvedValues: 3,
  encryptedLocations: 5,
  tokenizedLocations: 2,
  unresolvedLocations: 1,
  keyIds: ["archive-2026"],
  areas: [
    { name: "request body", count: 4 },
    { name: "response headers", count: 3 },
  ],
};

function ControlledDialog({ onDownload = vi.fn() }: { onDownload?: () => void }) {
  const [riskAccepted, setRiskAccepted] = useState(false);
  const [handlingAccepted, setHandlingAccepted] = useState(false);

  return (
    <SensitiveExportDialog
      summary={summary}
      riskAccepted={riskAccepted}
      handlingAccepted={handlingAccepted}
      onRiskAccepted={setRiskAccepted}
      onHandlingAccepted={setHandlingAccepted}
      onCancel={vi.fn()}
      onDownload={onDownload}
    />
  );
}

afterEach(cleanup);

describe("SensitiveExportDialog", () => {
  it("requires both explicit confirmations before downloading plaintext", async () => {
    const user = userEvent.setup();
    const onDownload = vi.fn();
    render(<ControlledDialog onDownload={onDownload} />);

    const download = screen.getByRole<HTMLButtonElement>("button", { name: "download plaintext export" });
    expect(download.disabled).toBe(true);

    await user.click(screen.getByLabelText(/I understand this file contains resolved plaintext/));
    expect(download.disabled).toBe(true);

    await user.click(screen.getByLabelText(/I will store, share, and delete this derived file/));
    expect(download.disabled).toBe(false);

    await user.click(download);
    expect(onDownload).toHaveBeenCalledOnce();
  });

  it("shows a plaintext-free impact summary and explicit risk warning", () => {
    const { container } = render(<ControlledDialog />);

    const exportSummary = screen.getByLabelText("Resolved value export summary");
    expect(within(exportSummary).getByText("7")).toBeTruthy();
    expect(screen.getByText("request body")).toBeTruthy();
    expect(screen.getByText(/key IDs: archive-2026/)).toBeTruthy();
    expect(screen.getByText(/may contain credentials, personal data, financial information/)).toBeTruthy();
    expect(container.textContent).not.toContain("customer-secret");
  });
});
