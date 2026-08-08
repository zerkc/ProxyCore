import { defineConfig } from "vitest/config";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  resolve: {
    alias: {
      "@proxycore/domain": path.resolve(root, "packages/domain/src"),
      "@proxycore/db": path.resolve(root, "packages/db/src"),
      "@proxycore/config": path.resolve(root, "packages/config/src"),
      "@proxycore/crypto": path.resolve(root, "packages/crypto/src"),
      "@proxycore/renderers": path.resolve(root, "packages/renderers/src"),
      "@proxycore/certificates": path.resolve(
        root,
        "packages/certificates/src",
      ),
      "@proxycore/testing": path.resolve(root, "packages/testing/src"),
    },
  },
  test: {
    environment: "node",
    include: ["packages/**/*.test.ts", "apps/**/*.test.ts"],
    coverage: {
      reporter: ["text", "json-summary"],
    },
  },
});
