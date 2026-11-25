import "server-only";

import {
  fetchPayPage,
  type PayPageResult,
  type PayPageSTK,
  type PublicClientOptions,
  sendPayPageSTK,
} from "@talksasa/api-client/public";
import { headers } from "next/headers";
import pino from "pino";

import { env } from "./env";

export const log = pino({
  level: env.TSP_LOG_LEVEL,
  base: { service: "talksasa-pay-page" },
  timestamp: pino.stdTimeFunctions.isoTime,
  // Payment-link tokens are bearer secrets for the charge and phone numbers
  // are personal data: never log them.
  redact: { paths: ["*.token", "*.cookie", "*.authorization", "*.phone"], censor: "[REDACTED]" },
});

/** Where the Go public API is, plus the visitor's IP (for per-visitor rate limiting), UA and request id. */
async function forwardOptions(): Promise<PublicClientOptions> {
  const h = await headers();
  const clientIp = env.TSP_TRUST_FORWARDED_FOR ? h.get("x-forwarded-for")?.split(",").at(-1)?.trim() || undefined : undefined;
  return {
    baseUrl: env.TSP_PUBLIC_API_URL,
    requestId: h.get("x-request-id") ?? undefined,
    clientIp,
    clientUa: h.get("user-agent") ?? undefined,
  };
}

/** Looks up the charge behind a payment link. */
export async function getPayPage(token: string): Promise<PayPageResult> {
  return fetchPayPage(await forwardOptions(), token);
}

/** Sends an M-Pesa payment prompt for the charge. Throws ApiError when the Go service refuses. */
export async function sendStk(token: string, phone: string): Promise<PayPageSTK> {
  return sendPayPageSTK(await forwardOptions(), token, phone);
}
