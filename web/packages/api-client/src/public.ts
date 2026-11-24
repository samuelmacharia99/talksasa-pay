import "server-only";

import createClient from "openapi-fetch";

import { type ForwardContext, forwardHeaders } from "./context";
import { ApiError, toApiError } from "./errors";
import type { components, paths } from "./generated/public";

export type PayPage = components["schemas"]["PayPage"];
export type PayPageSTK = components["schemas"]["PayPageSTK"];

const TOKEN = /^[a-z2-7]{52}$/;

export interface PublicClientOptions extends ForwardContext {
  /** Internal URL of the Go public listener, e.g. http://api:8088/public/v1 */
  baseUrl: string;
  timeoutMs?: number;
}

/** Result of a pay-page lookup: not-found is a normal outcome, not an error. */
export type PayPageResult = { found: true; page: PayPage } | { found: false };

/** Fetches the bill behind a hosted payment link (server-side only). */
export async function fetchPayPage(o: PublicClientOptions, token: string): Promise<PayPageResult> {
  if (!TOKEN.test(token)) return { found: false };
  const client = publicClient(o, 10_000);
  const { data, error, response } = await client.GET("/pay/{token}", { params: { path: { token } } });
  if (response.status === 404) return { found: false };
  if (error || !data) throw toApiError(response.status, error);
  return { found: true, page: data };
}

/**
 * Asks the Go service to send an M-Pesa payment prompt for the bill behind
 * a hosted payment link (server-side only). Resolves with the new prompt;
 * any refusal (404, 409 stk_in_progress / not_payable / amount_too_large,
 * 422, 429, 502 mpesa_error, 503 mpesa_unavailable) throws an ApiError
 * whose `code` says which. Never retried here: a retry could send the
 * customer a second prompt.
 */
export async function sendPayPageSTK(o: PublicClientOptions, token: string, phone: string): Promise<PayPageSTK> {
  if (!TOKEN.test(token)) throw new ApiError({ status: 404, code: "not_found", detail: "This payment link isn’t valid." });
  // Go calls Safaricom before answering, so allow more time than a lookup.
  const client = publicClient(o, 30_000);
  const { data, error, response } = await client.POST("/pay/{token}/stk", {
    params: { path: { token } },
    body: { phone },
  });
  if (error || !data) throw toApiError(response.status, error);
  return data;
}

function publicClient(o: PublicClientOptions, defaultTimeoutMs: number) {
  return createClient<paths>({
    baseUrl: o.baseUrl,
    headers: forwardHeaders(o),
    cache: "no-store",
    fetch: (req: Request) => fetch(req, { signal: AbortSignal.timeout(o.timeoutMs ?? defaultTimeoutMs) }),
  });
}
