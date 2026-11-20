import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import wails from "@wailsio/runtime/plugins/vite";

// The page carries a content security policy for the built app. The dev server injects inline
// scripts and styles (hot reload), which that policy would block, so it serves the page without it.
const devWithoutPolicy = {
  name: "dev-without-policy",
  apply: "serve",
  transformIndexHtml: {
    order: "pre",
    handler: (html) => html.replace(/\s*<meta http-equiv="Content-Security-Policy"[^>]*>/, ""),
  },
};

// https://vitejs.dev/config/
export default defineConfig({
  server: {
    host: "127.0.0.1",
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
  },
  plugins: [react(), wails("./bindings"), devWithoutPolicy],
});
