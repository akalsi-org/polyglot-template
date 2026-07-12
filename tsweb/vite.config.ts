import deno from "@deno/vite-plugin";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("./app/site", import.meta.url));
const shared = fileURLToPath(new URL("./lib", import.meta.url));
const output = fileURLToPath(new URL("../build/tsweb/site", import.meta.url));

export default defineConfig({
  root,
  plugins: [deno(), react()],
  resolve: {
    alias: {
      "#": shared,
    },
  },
  build: {
    outDir: output,
    emptyOutDir: true,
    sourcemap: false,
  },
});
