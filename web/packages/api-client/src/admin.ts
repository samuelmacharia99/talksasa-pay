import "server-only";

import createClient, { type Middleware } from "openapi-fetch";

import { type ForwardContext, forwardHeaders } from "./context";
import { toApiError } from "./errors";
import type { components, paths } from "./generated/admin";

export type AdminPaths = paths;
export type AdminSchemas = components["schemas"];

export interface AdminClientOptions extends ForwardContext {
  /** Internal URL of the Go admin listener, e.g. http://api:8089/admin/v1 */
  baseUrl: string;
  /** Staff session token from the httpOnly cookie (absent before sign-in). */
  sessionToken?: string;
  /** Per-call timeout in milliseconds. */
  timeoutMs?: number;
}

/**
 * A typed client for the Go admin API. Server-side only: it carries the
 * staff member's session token, which must never reach browser JavaScript.
 * Non-2xx responses throw ApiError.
 */
export function adminClient(o: AdminClientOptions) {
  const headers = forwardHeaders(o);
  if (o.sessionToken) headers.Authorization = `Bearer ${o.sessionToken}`;
  const timeoutMs = o.timeoutMs ?? 15_000;

  const client = createClient<paths>({
    baseUrl: o.baseUrl,
    headers,
    // Never cache: every admin view must reflect the ledger right now.
    cache: "no-store",
    fetch: (req: Request) => fetch(req, { signal: AbortSignal.timeout(timeoutMs) }),
  });
  const throwOnError: Middleware = {
    async onResponse({ response }) {
      if (!response.ok) {
        const body = await response.clone().json().catch(() => undefined);
        throw toApiError(response.status, body);
      }
      return undefined;
    },
  };
  client.use(throwOnError);
  return client;
}

export type AdminClient = ReturnType<typeof adminClient>;
