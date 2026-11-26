"use server";

import type { AdminSchemas } from "@talksasa/api-client/admin";
import { revalidatePath } from "next/cache";

import { act, type ActionResult } from "@/lib/api";
import { callbackBaseForThisPanel } from "@/lib/settings-server";

// Permissions are enforced by the Go service on every call (settings.manage,
// admin only). Credentials pass through here once, to the service, and are
// never returned, logged or put back into the form.

function str(fd: FormData, k: string): string {
  const v = fd.get(k);
  return typeof v === "string" ? v.trim() : "";
}

export async function saveSettings(_: ActionResult | undefined, fd: FormData): Promise<ActionResult> {
  // The callback base URL is not typed in: it is this panel's own public
  // address plus /api/ (the Go service ignores it when the deployment sets
  // TSP_MPESA_CALLBACK_BASE_URL).
  const callbackBase = await callbackBaseForThisPanel();
  if (!callbackBase) {
    return { ok: false, error: "This panel's address could not be read, so the callback URL cannot be generated. Set TSP_MPESA_CALLBACK_BASE_URL in the deployment configuration." };
  }
  const r = await act((c) =>
    c.PUT("/settings/mpesa", {
      body: {
        environment: str(fd, "environment") === "production" ? "production" : "sandbox",
        shortcode: str(fd, "shortcode"),
        stk_shortcode: str(fd, "stk_shortcode") || null,
        callback_base_url: callbackBase,
        // Empty keeps the stored value.
        consumer_key: str(fd, "consumer_key") || null,
        consumer_secret: str(fd, "consumer_secret") || null,
        passkey: str(fd, "passkey") || null,
        confirm: str(fd, "confirm") || null,
      },
    }),
  );
  if (!r.ok) return r;
  revalidatePath("/settings");
  return { ok: true, data: undefined };
}

export type TestResult = ActionResult<{ ok: boolean; tested_at: string; message: string | null }> | undefined;

export async function testCredentials(_: TestResult): Promise<TestResult> {
  const r = await act((c) => c.POST("/settings/mpesa/test"));
  if (!r.ok) return r;
  revalidatePath("/settings");
  return { ok: true, data: r.data };
}

export type RegisterResult = ActionResult<{ registered_at: string; confirmation_url: string }> | undefined;

export async function registerURLs(_: RegisterResult, fd: FormData): Promise<RegisterResult> {
  const confirm = str(fd, "confirm");
  const r = await act((c) => c.POST("/settings/mpesa/register-urls", { body: { confirm: confirm || null } }));
  if (!r.ok) return r;
  revalidatePath("/settings");
  return { ok: true, data: { registered_at: r.data.registered_at, confirmation_url: r.data.confirmation_url } };
}

export type TestPayment = AdminSchemas["TestPayment"];
export type TestPaymentResult = ActionResult<TestPayment> | undefined;

/** Sends a real test payment prompt (admins; KES 1-1,000). */
export async function startTestPayment(_: TestPaymentResult, fd: FormData): Promise<TestPaymentResult> {
  const amount = Number(str(fd, "amount"));
  const r = await act((c) =>
    c.POST("/settings/mpesa/test-payments", {
      body: {
        amount: Number.isInteger(amount) ? amount : 0,
        phone: str(fd, "phone"),
        key: str(fd, "key"),
        confirm: str(fd, "confirm") || null,
      },
    }),
  );
  if (!r.ok) return r;
  revalidatePath("/settings");
  return { ok: true, data: r.data };
}

/** The latest test payments, for the live status while a prompt is out. */
export async function recentTestPayments(): Promise<TestPayment[]> {
  const r = await act((c) => c.GET("/settings/mpesa/test-payments"));
  return r.ok ? r.data.items : [];
}
