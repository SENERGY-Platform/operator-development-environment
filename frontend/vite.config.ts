/// <reference types="vitest/config" />
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react-swc";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    // `@/` is what the shadcn generator writes into every component it emits.
    // Adding the alias here rather than rewriting the imports keeps a regenerated
    // component a drop-in replacement for the one it supersedes.
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    port: 5173,
    // The backend enforces every rule; the proxy only saves configuring CORS
    // for local development.
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
        // The profiler runs over a WebSocket, and the proxy will not forward an
        // upgrade without this.
        ws: true,
        rewrite: (path) => path.replace(/^\/api/, ""),
      },
    },
  },
  test: {
    // Only the tests. A `*.test.ts` under src is never reached from index.html, so
    // it is not in the bundle either — Rollup follows the entry, not the folder.
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    // node by default, because most of what is worth testing here is pure. The
    // files that need a document say so with `// @vitest-environment jsdom`, which
    // keeps the requirement next to the code that has it.
    environment: "node",
    // keycloak.ts refuses to load without this, on purpose: a deployment address
    // must not be a committed default. The tests still have to import the modules
    // that reach it, so they get an obviously-fake one — if a test ever talks to
    // this host, that is the bug rather than a flake.
    env: { VITE_KEYCLOAK_URL: "https://keycloak.invalid/auth" },
  },
});
