import { InvitePreviewCard } from "codesk-frontend";

export const Default = () => (
  <div style={{ width: 360 }}>
    <InvitePreviewCard
      preview={{ workspace: { name: "Acme Docs", slug: "acme-docs" }, expiresAt: "2026-07-18T17:00:00Z" } as any}
    />
  </div>
);

export const LongName = () => (
  <div style={{ width: 360 }}>
    <InvitePreviewCard
      preview={
        {
          workspace: { name: "Platform Engineering Knowledge Base", slug: "platform-engineering-knowledge-base" },
          expiresAt: "2026-07-11T09:30:00Z",
        } as any
      }
    />
  </div>
);
