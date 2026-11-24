import "server-only";

import { z } from "zod";

const bool = z
  .enum(["true", "false", ""])
  .default("false")
  .transform((v) => v === "true");

const schema = z.object({
  NODE_ENV: z.enum(["development", "production", "test"]).default("production"),
  /** Internal URL of the Go admin listener. Never exposed to browsers. */
  TSP_ADMIN_API_URL: z.url().default("http://127.0.0.1:8089/admin/v1"),
  /**
   * Plain-HTTP local development only: drops the Secure flag and the __Host-
   * cookie prefix so the session cookie works on http://127.0.0.1.
   */
  TSP_ADMIN_INSECURE_COOKIES: bool,
  /**
   * Set when a reverse proxy we control (Caddy) sits in front and appends the
   * client address to X-Forwarded-For. Otherwise the header is ignored.
   */
  TSP_TRUST_FORWARDED_FOR: bool,
  TSP_LOG_LEVEL: z.enum(["debug", "info", "warn", "error"]).default("info"),
});

/** Validated configuration; the process fails fast on bad values. */
export const env = schema.parse(process.env);
