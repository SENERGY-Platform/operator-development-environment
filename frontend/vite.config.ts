import { defineConfig } from "vite";
import react from "@vitejs/plugin-react-swc";

export default defineConfig({
  plugins: [react()],
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
});
