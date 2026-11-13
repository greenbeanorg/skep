import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath, URL } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// Development serving (ADR 0008, decision 4b). The dev server speaks HTTPS
// with the certificate skep-api generated for itself in .skep-dev (created
// by "make run-dev"), so that the browser sees one HTTPS origin: the
// session cookie is Secure and __Host- as in production, and the Origin
// header matches the Host that skep-api sees. /api is proxied to skep-api,
// verified with that same certificate, with the Host header kept.
//
// There is no plain-HTTP mode: it would need the server to relax the cookie
// or the origin check, and ADR 0008 excludes that.
const repoRoot = fileURLToPath(new URL("..", import.meta.url));
const tlsDir = process.env.SKEP_DEV_TLS_DIR ?? `${repoRoot}.skep-dev/api/tls`;
const certFile = `${tlsDir}/cert.pem`;
const keyFile = `${tlsDir}/key.pem`;
const apiTarget = process.env.SKEP_API_URL ?? "https://127.0.0.1:8443";

function devCertificate(): { cert: Buffer; key: Buffer } | undefined {
  if (!existsSync(certFile) || !existsSync(keyFile)) return undefined;
  return { cert: readFileSync(certFile), key: readFileSync(keyFile) };
}

export default defineConfig(({ command, mode }) => {
  const tls = devCertificate();
  // Vitest loads this file too (mode "test"); only the dev server needs the
  // certificate.
  if (command === "serve" && mode !== "test" && !tls) {
    throw new Error(
      `the development certificate ${certFile} does not exist; run "make run-dev" in the repository root first ` +
        "(or set SKEP_DEV_TLS_DIR to a directory with cert.pem and key.pem)",
    );
  }
  return {
    plugins: [react(), tailwindcss()],
    resolve: {
      alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
    },
    build: {
      // The built files are embedded in skep-api (internal/webui).
      outDir: "dist",
      emptyOutDir: true,
      sourcemap: false,
    },
    server: {
      host: "127.0.0.1",
      port: 5173,
      strictPort: true,
      https: tls,
      proxy: {
        "/api": {
          target: apiTarget,
          changeOrigin: false,
          // Verify skep-api with the certificate it generated: never
          // "secure: false".
          secure: true,
          ...(tls ? { ssl: { ca: tls.cert } } : {}),
        },
      },
    },
    test: {
      environment: "jsdom",
      include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
      exclude: ["e2e/**", "node_modules/**"],
      setupFiles: ["src/test/setup.ts"],
      css: false,
    },
  };
});
