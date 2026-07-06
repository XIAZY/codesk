import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

function hostFromOrigin(value: string | undefined) {
  if (!value) {
    return "";
  }
  try {
    return new URL(value).hostname;
  } catch {
    return value;
  }
}

const configuredHost = hostFromOrigin(process.env.VITE_PUBLIC_ORIGIN || process.env.NOTTY_FRONTEND_ORIGIN);
const allowedHosts = Array.from(new Set(["localhost", "127.0.0.1", configuredHost || "app.nottyai.co"].filter(Boolean)));

export default defineConfig({
  plugins: [react()],
  server: {
    host: "0.0.0.0",
    port: 5173,
    allowedHosts,
    // Allow importing the Go contract-pin golden file (daemon fixtures) that lives one directory up,
    // outside frontend/. See frontend/src/daemonFixtures.ts.
    fs: {
      allow: [".", ".."],
    },
  },
  preview: {
    host: "0.0.0.0",
    port: 5173,
    allowedHosts,
  },
});
