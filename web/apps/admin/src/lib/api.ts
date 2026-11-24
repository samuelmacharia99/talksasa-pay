import "server-only";

import { type AdminClient, adminClient, type AdminPaths, type AdminSchemas } from "@talksasa/api-client/admin";
import { isApiError } from "@talksasa/api-client/errors";
import { headers } from "next/headers";
import { forbidden, notFound, redirect } from "next/navigation";
import { cache } from "react";

import { env } from "./env";
import { log } from "./log";
import { getSessionToken } from "./session";

export type Me = AdminSchemas["Me"];
export type Permission = Me["permissions"][number];

async function forwardContext() {
  const h = await headers();
  let clientIp: string | undefined;
  if (env.TSP_TRUST_FORWARDED_FOR) {
    // Our own reverse proxy appends the real peer last; earlier entries are
    // client-supplied and untrusted.
    clientIp = h.get("x-forwarded-for")?.split(",").at(-1)?.trim() || undefined;
  }
  return {
    requestId: h.get("x-request-id") ?? undefined,
    clientIp,
    clientUa: h.get("user-agent") ?? undefined,
  };
}

/** A Go admin API client carrying this visitor's session (if any). */
export async function api(): Promise<AdminClient> {
  return adminClient({
    baseUrl: env.TSP_ADMIN_API_URL,
    sessionToken: await getSessionToken(),
    ...(await forwardContext()),
  });
}

/**
 * Runs a read against the admin API for a page. A 401 means the session
 * ended (idle timeout, sign-out elsewhere, account disabled): send the user
 * to sign in again. A 404 renders the not-found page.
 */
export async function load<T>(fn: (c: AdminClient) => Promise<{ data?: T }>): Promise<T> {
  const c = await api();
  let res: { data?: T };
  try {
    res = await fn(c);
  } catch (e) {
    if (isApiError(e)) {
      if (e.status === 401) redirect("/session-ended");
      if (e.status === 403) forbidden();
      if (e.status === 404) notFound();
      log.error({ status: e.status, code: e.code, request_id: e.requestId }, "admin API error");
    }
    throw e;
  }
  return res.data as T;
}

/** The signed-in staff member, fetched once per request. */
export const getMe = cache(async (): Promise<Me> => load((c) => c.GET("/auth/me")));

/** Ensures a session and, optionally, a permission (403 page otherwise). */
export async function requireMe(perm?: Permission): Promise<Me> {
  const me = await getMe();
  if (perm && !me.permissions.includes(perm)) forbidden();
  return me;
}

/** Result shape for Server Actions used with useActionState. */
export type ActionResult<T = undefined> = { ok: true; data: T } | { ok: false; error: string; field?: string };

/**
 * Runs a mutating call from a Server Action, turning API errors into a
 * message for the form. Problem `detail` text is written for staff and is
 * safe to show.
 */
export async function act<T>(fn: (c: AdminClient) => Promise<{ data?: T }>): Promise<ActionResult<T>> {
  const c = await api();
  try {
    const res = await fn(c);
    return { ok: true, data: res.data as T };
  } catch (e) {
    if (isApiError(e)) {
      if (e.status === 401) redirect("/session-ended");
      if (e.status >= 500) log.error({ status: e.status, code: e.code, request_id: e.requestId }, "admin API error");
      return { ok: false, error: e.detail };
    }
    log.error({ err: e instanceof Error ? e.message : String(e) }, "admin API call failed");
    return { ok: false, error: "The payment service could not be reached. Try again shortly." };
  }
}

/**
 * Query parameters of a GET list endpoint. Values from the URL are strings;
 * the Go service validates enums and formats and answers 400 on bad input.
 */
export type QueryOf<P extends keyof AdminPaths> = AdminPaths[P] extends { get: { parameters: { query?: infer Q } } } ? NonNullable<Q> : never;
