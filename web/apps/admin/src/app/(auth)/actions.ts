"use server";

import { isApiError } from "@talksasa/api-client/errors";
import { redirect } from "next/navigation";

import { api } from "@/lib/api";
import { log } from "@/lib/log";
import { clearChallenge, clearSession, getChallenge, setChallenge, setSession } from "@/lib/session";

export type FormState = { error?: string; recoveryCodes?: string[] } | undefined;

function str(fd: FormData, key: string): string {
  const v = fd.get(key);
  return typeof v === "string" ? v : "";
}

function failure(e: unknown): FormState {
  if (isApiError(e)) return { error: e.detail };
  log.error({ err: e instanceof Error ? e.message : String(e) }, "sign-in call failed");
  return { error: "The payment service could not be reached. Try again shortly." };
}

/** Step 1: email and password. On success the 2FA challenge goes into a short-lived httpOnly cookie. */
export async function signIn(_: FormState, fd: FormData): Promise<FormState> {
  const email = str(fd, "email").trim();
  const password = str(fd, "password");
  if (!email || !password) return { error: "Enter your email and password." };
  try {
    const { data } = await (await api()).POST("/auth/login", { body: { email, password } });
    await setChallenge(data!.challenge, data!.expires_at);
  } catch (e) {
    return failure(e);
  }
  redirect("/login/verify");
}

/** Step 2: the authenticator or recovery code. Success sets the session cookie. */
export async function verifyCode(_: FormState, fd: FormData): Promise<FormState> {
  const challenge = await getChallenge();
  if (!challenge) return { error: "Your sign-in step expired. Start again with your email and password." };
  const code = str(fd, "code").trim();
  if (!code) return { error: "Enter the 6-digit code from your authenticator app." };
  try {
    const { data } = await (await api()).POST("/auth/mfa", { body: { challenge, code } });
    await setSession(data!.session_token, data!.expires_at);
    await clearChallenge();
  } catch (e) {
    if (isApiError(e) && e.code === "session_invalid") {
      await clearChallenge();
      return { error: "Your sign-in step expired. Start again with your email and password." };
    }
    return failure(e);
  }
  redirect("/");
}

/** Accepting an invitation: password, first TOTP code, then recovery codes are shown once. */
export async function completeSetup(token: string, _: FormState, fd: FormData): Promise<FormState> {
  const password = str(fd, "password");
  if (password !== str(fd, "confirm")) return { error: "The two passwords don’t match." };
  try {
    const { data } = await (await api()).POST("/auth/setup/{token}", {
      params: { path: { token } },
      body: { enrolment: str(fd, "enrolment"), password, code: str(fd, "code").trim() },
    });
    return { recoveryCodes: data!.recovery_codes };
  } catch (e) {
    return failure(e);
  }
}

export async function signOut(): Promise<void> {
  try {
    await (await api()).POST("/auth/logout");
  } catch {
    // The cookie is cleared regardless; a stale server session expires on its own.
  }
  await clearSession();
  redirect("/login");
}
