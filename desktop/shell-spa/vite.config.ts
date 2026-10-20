import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// The one thing that makes this page loadable by the shell: it is read off
// the filesystem with loadFile (file://), so every asset URL must be
// relative. base "./" is what Vite calls that. Without it the built page
// asks the origin for /assets/... and the window stays blank — the shell
// would then fall back to its missingPage placeholder (main.ts loadPage).
export default defineConfig({
  base: "./",
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  test: {
    // Two suites, two runners, one command (npm test):
    //   node:test   the pure logic (test/*.test.ts) — no transform, no DOM
    //   vitest      the rendered page (test/dom/*.test.tsx) — jsdom + React
    // The include keeps vitest to its half so nothing runs twice.
    environment: "jsdom",
    include: ["test/dom/**/*.test.tsx"],
  },
});
