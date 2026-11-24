import "server-only";

import { z } from "zod";

const schema = z.object({
  /** Internal URL of the Go public listener. */
  TSP_PUBLIC_API_URL: z.url().default("http://127.0.0.1:8088/public/v1"),
  /** Set when our own reverse proxy appends the client IP to X-Forwarded-For. */
  TSP_TRUST_FORWARDED_FOR: z
    .enum(["true", "false", ""])
    .default("false")
    .transform((v) => v === "true"),
  TSP_LOG_LEVEL: z.enum(["debug", "info", "warn", "error"]).default("info"),
});

export const env = schema.parse(process.env);
