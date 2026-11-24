import path from "node:path";

import type { NextConfig } from "next";

// Monorepo root: the standalone build must trace the workspace packages.
const root = path.join(import.meta.dirname, "../..");

const config: NextConfig = {
  output: "standalone",
  outputFileTracingRoot: root,
  turbopack: { root },
  poweredByHeader: false,
  reactStrictMode: true,
  transpilePackages: ["@talksasa/ui", "@talksasa/money", "@talksasa/api-client"],
  // forbidden() renders app/forbidden.tsx for staff whose role lacks a permission.
  experimental: { authInterrupts: true },
  // The admin panel is never indexed, framed or cached by intermediaries.
  // The Content-Security-Policy (with a per-request nonce) is set in proxy.ts.
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Frame-Options", value: "DENY" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "no-referrer" },
          { key: "X-Robots-Tag", value: "noindex, nofollow" },
          { key: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=(), payment=(), usb=()" },
          { key: "Cross-Origin-Opener-Policy", value: "same-origin" },
          { key: "Strict-Transport-Security", value: "max-age=63072000; includeSubDomains" },
        ],
      },
    ];
  },
};

export default config;
