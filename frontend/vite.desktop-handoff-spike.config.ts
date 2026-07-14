import { defineConfig } from "vite";

export default defineConfig({
  build: {
    outDir: ".test-dist/desktop-handoff",
    emptyOutDir: true,
    rollupOptions: {
      input: "desktop-handoff-spike.html",
    },
  },
});
