import { Modal } from "codesk-frontend";

export const ShareWorkspace = () => (
  <div style={{ position: "relative", transform: "translateZ(0)", height: 620 }}>
    <Modal title="Share workspace" onClose={() => {}}>
      <div className="form-stack">
        <label className="field">
          <span className="lab">Invite link</span>
          <input readOnly value="https://codesk.app/i/8f3k2mqz" />
        </label>
        <p className="tiny muted">Expires Jul 11, 2026, 9:00 AM</p>
        <button className="btn accent full" type="button">
          Copy invite link
        </button>
      </div>
    </Modal>
  </div>
);
