import "server-only";

import createClient from "openapi-fetch";

import { type ForwardContext, forwardHeaders } from "./context";
import { toApiError } from "./errors";
import type { components, paths } from "./generated/public";

export type PayPage = components["schemas"]["PayPage"];

export interface PublicClientOptions extends ForwardContext {
  /** Internal URL of the Go public listener, e.g. http://api:8088/public/v1 */
  baseUrl: string;
  timeoutMs?: number;
}

/** Result of a pay-page lookup: not-found is a normal outcome, not an error. */
export type PayPageResult = { found: true; page: PayPage } | { found: false };

/** Fetches the bill behind a hosted payment link (server-side only). */
export async function fetchPayPage(o: PublicClientOptions, token: string): Promise<PayPageResult> {
  if (!/^[a-z2-7]{52}$/.test(token)) return { found: false };
  const client = createClient<paths>({
    baseUrl: o.baseUrl,
    headers: forwardHeaders(o),
    cache: "no-store",
    fetch: (req: Request) => fetch(req, { signal: AbortSignal.timeout(o.timeoutMs ?? 10_000) }),
  });
  const { data, error, response } = await client.GET("/pay/{token}", { params: { path: { token } } });
  if (response.status === 404) return { found: false };
  if (error || !data) throw toApiError(response.status, error);
  return { found: true, page: data };
}
