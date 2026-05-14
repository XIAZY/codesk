import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const backendProxyTarget = process.env.NOTTY_VITE_BACKEND_PROXY_TARGET || "http://localhost:8080";
const staticProxyTarget = process.env.NOTTY_VITE_STATIC_PROXY_TARGET || "http://localhost";
const localProxy = {
  "/api": {
    target: backendProxyTarget,
    changeOrigin: true,
  },
  "/healthz": {
    target: backendProxyTarget,
    changeOrigin: true,
  },
  "/ws": {
    target: backendProxyTarget,
    changeOrigin: true,
    ws: true,
  },
  "/daemons": {
    target: staticProxyTarget,
    changeOrigin: true,
  },
};

export default defineConfig({
  plugins: [react()],
  server: {
    host: "0.0.0.0",
    port: 5173,
    allowedHosts: ["app.nottyai.co"],
    proxy: localProxy,
  },
  preview: {
    host: "0.0.0.0",
    port: 5173,
    allowedHosts: ["app.nottyai.co"],
    proxy: localProxy,
  },
});
