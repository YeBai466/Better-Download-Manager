import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

const fixture = fileURLToPath(new URL("./fake-api.js", import.meta.url));
const runtime = fileURLToPath(new URL("./fake-runtime.js", import.meta.url));
export default defineConfig({
  plugins: [
    react(),
    {
      name: "add-page-fixture",
      enforce: "pre",
      resolveId(source, importer) {
        if (!importer?.replaceAll("\\", "/").endsWith("/src/AddPage.tsx"))
          return;
        if (source === "./api") return fixture;
        if (source === "@wailsio/runtime") return runtime;
      },
    },
  ],
});
