import "server-only";

import { fetchPayPage, type PayPageResult } from "@talksasa/api-client/public";
import { headers } from "next/headers";
import pino from "pino";

import { env } from "./env";

export const log = pino({
  level: env.TSP_LOG_LEVEL,
  base: { service: "talksasa-pay-page" },
  timestamp: pino.stdTimeFunctions.isoTime,
  // Payment-link tokens are bearer secrets for the bill: never log them.
  redact: { paths: ["*.token", "*.cookie", "*.authorization"], censor: "[REDACTED]" },
});

/** Looks up the bill behind a payment link, forwarding the visitor's IP for rate limiting. */
export async function getPayPage(token: string): Promise<PayPageResult> {
  const h = await headers();
  const clientIp = env.TSP_TRUST_FORWARDED_FOR ? h.get("x-forwarded-for")?.split(",").at(-1)?.trim() || undefined : undefined;
  return fetchPayPage(
    {
      baseUrl: env.TSP_PUBLIC_API_URL,
      requestId: h.get("x-request-id") ?? undefined,
      clientIp,
      clientUa: h.get("user-agent") ?? undefined,
    },
    token,
  );
}
