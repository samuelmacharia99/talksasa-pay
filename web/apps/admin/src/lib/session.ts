import "server-only";

import { cookies } from "next/headers";

import { env } from "./env";

const secure = !env.TSP_ADMIN_INSECURE_COOKIES;

// __Host- cookies must be Secure, Path=/ and host-only: they cannot be set
// or overwritten by a sibling subdomain.
const SESSION = secure ? "__Host-tsp_admin" : "tsp_admin";
const CHALLENGE = secure ? "__Host-tsp_mfa" : "tsp_mfa";

const base = { httpOnly: true, secure, sameSite: "strict" as const, path: "/" };

export async function getSessionToken(): Promise<string | undefined> {
  return (await cookies()).get(SESSION)?.value;
}

/** Stores the staff session token where browser JavaScript cannot read it. */
export async function setSession(token: string, expiresAt: string): Promise<void> {
  (await cookies()).set(SESSION, token, { ...base, expires: new Date(expiresAt) });
}

export async function clearSession(): Promise<void> {
  (await cookies()).set(SESSION, "", { ...base, maxAge: 0 });
}

/** The password step's 2FA challenge lives for at most 5 minutes. */
export async function setChallenge(token: string, expiresAt: string): Promise<void> {
  (await cookies()).set(CHALLENGE, token, { ...base, expires: new Date(expiresAt) });
}

export async function getChallenge(): Promise<string | undefined> {
  return (await cookies()).get(CHALLENGE)?.value;
}

export async function clearChallenge(): Promise<void> {
  (await cookies()).set(CHALLENGE, "", { ...base, maxAge: 0 });
}
