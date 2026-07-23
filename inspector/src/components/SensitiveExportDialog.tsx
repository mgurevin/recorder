import { forwardRef } from "react";
import { AlertTriangle, Download } from "lucide-react";
import type { ResolvedExportSummary } from "../lib/exportCapture";

export const SensitiveExportDialog = forwardRef<HTMLElement, {
  summary: ResolvedExportSummary;
  riskAccepted: boolean;
  handlingAccepted: boolean;
  onRiskAccepted: (accepted: boolean) => void;
  onHandlingAccepted: (accepted: boolean) => void;
  onCancel: () => void;
  onDownload: () => void;
}>(function SensitiveExportDialog({
  summary,
  riskAccepted,
  handlingAccepted,
  onRiskAccepted,
  onHandlingAccepted,
  onCancel,
  onDownload,
}, ref) {
  return (
    <div className="modal-backdrop" role="presentation">
      <section
        ref={ref}
        className="sensitive-export-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="sensitive-export-title"
        tabIndex={-1}
      >
        <header>
          <span className="sensitive-export-icon"><AlertTriangle size={20} /></span>
          <div>
            <h2 id="sensitive-export-title">Review plaintext export</h2>
            <p>This download creates a derived fixture containing sensitive values resolved in browser memory.</p>
          </div>
        </header>

        <div className="sensitive-export-summary" aria-label="Resolved value export summary">
          <div><strong>{summary.resolvedLocations}</strong><span>resolved locations</span></div>
          <div><strong>{summary.uniqueResolvedValues}</strong><span>unique values</span></div>
          <div><strong>{summary.encryptedLocations}</strong><span>decrypted locations</span></div>
          <div><strong>{summary.tokenizedLocations}</strong><span>verified locations</span></div>
        </div>

        <div className="sensitive-export-details">
          <p><strong>Affected areas</strong></p>
          <ul>
            {summary.areas.map((area) => (
              <li key={area.name}><span>{area.name}</span><strong>{area.count}</strong></li>
            ))}
          </ul>
          {summary.keyIds.length > 0 ? (
            <p className="mono muted">key IDs: {summary.keyIds.join(", ")}</p>
          ) : null}
          {summary.unresolvedLocations > 0 ? (
            <p className="muted">{summary.unresolvedLocations} unresolved locations will remain protected.</p>
          ) : null}
        </div>

        <div className="sensitive-export-warning">
          <AlertTriangle size={16} />
          <p>
            The downloaded file may contain credentials, personal data, financial information, proxy secrets,
            cookies, and request or response bodies in plaintext. It is not equivalent to the original protected evidence.
          </p>
        </div>

        <label className="sensitive-confirmation">
          <input
            type="checkbox"
            checked={riskAccepted}
            onChange={(event) => onRiskAccepted(event.target.checked)}
          />
          <span>I understand this file contains resolved plaintext and may expose sensitive data.</span>
        </label>
        <label className="sensitive-confirmation">
          <input
            type="checkbox"
            checked={handlingAccepted}
            onChange={(event) => onHandlingAccepted(event.target.checked)}
          />
          <span>I will store, share, and delete this derived file according to its sensitivity.</span>
        </label>

        <footer>
          <button type="button" className="btn" onClick={onCancel}>cancel</button>
          <button
            type="button"
            className="btn danger"
            disabled={!riskAccepted || !handlingAccepted}
            onClick={onDownload}
          >
            <Download size={14} /> download plaintext export
          </button>
        </footer>
      </section>
    </div>
  );
});
