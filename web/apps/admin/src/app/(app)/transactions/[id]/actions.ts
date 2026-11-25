"use server";

import { revalidatePath } from "next/cache";

import { act, type ActionResult } from "@/lib/api";
import { normalizeReceipt, type ResolveAction, validateAssign, validateResolve, validateReverse } from "@/lib/mpesa";

// The Go service checks the permission on every call (payments.resolve for
// resolve and assign: finance and admin; payments.reverse for reverse: admin
// only) and the transaction's state; this only validates the form first.

export type ResolveResult = ActionResult<{ status: string }> | undefined;
export type ReverseResult = ActionResult<{ reversalId: string }> | undefined;
export type AssignResult = ActionResult<{ appName: string | null; appStatus: string | null }> | undefined;

function str(fd: FormData, k: string): string {
  const v = fd.get(k);
  return typeof v === "string" ? v : "";
}

function invalid<T>(errors: Partial<Record<string, string>>): ActionResult<T> | undefined {
  const list = Object.entries(errors).filter((e): e is [string, string] => typeof e[1] === "string");
  const first = list[0];
  if (!first) return undefined;
  return { ok: false, error: first[1], field: first[0], fieldErrors: list.map(([field, message]) => ({ field, message })) };
}

function revalidateMoney(id: string, chargeId: string | null | undefined) {
  revalidatePath(`/transactions/${id}`);
  revalidatePath("/transactions");
  revalidatePath("/unmatched");
  revalidatePath("/");
  if (chargeId) revalidatePath(`/charges/${chargeId}`);
}

export async function resolveTransaction(id: string, action: ResolveAction, _: ResolveResult, fd: FormData): Promise<ResolveResult> {
  const input = { action, receipt: str(fd, "receipt"), reason: str(fd, "reason") };
  const bad = invalid<{ status: string }>(validateResolve(input));
  if (bad) return bad;
  const r = await act((c) =>
    c.POST("/transactions/{id}/resolve", {
      params: { path: { id } },
      body:
        action === "confirm"
          ? { action, receipt: normalizeReceipt(input.receipt), reason: input.reason.trim() }
          : { action, reason: input.reason.trim() },
    }),
  );
  if (!r.ok) return r;
  revalidateMoney(id, r.data.charge_id);
  return { ok: true, data: { status: r.data.status } };
}

/**
 * Records a reversal of a succeeded payment (admin only). `expected` is what
 * staff had to type to confirm; it is checked again here.
 */
export async function reverseTransaction(
  id: string,
  opts: { receiptRequired: boolean; expected: string },
  _: ReverseResult,
  fd: FormData,
): Promise<ReverseResult> {
  const input = { reason: str(fd, "reason"), receipt: str(fd, "receipt"), confirm: str(fd, "confirm"), ...opts };
  const bad = invalid<{ reversalId: string }>(validateReverse(input));
  if (bad) return bad;
  const receipt = normalizeReceipt(input.receipt);
  const r = await act((c) =>
    c.POST("/transactions/{id}/reverse", {
      params: { path: { id } },
      body: receipt ? { reason: input.reason.trim(), receipt } : { reason: input.reason.trim() },
    }),
  );
  if (!r.ok) return r;
  revalidateMoney(id, r.data.charge_id);
  revalidatePath(`/transactions/${r.data.id}`);
  return { ok: true, data: { reversalId: r.data.id } };
}

/** Assigns an unmatched payment to an app (finance, admin); the app then gets payment.received. */
export async function assignTransaction(id: string, _: AssignResult, fd: FormData): Promise<AssignResult> {
  const input = { appId: str(fd, "app_id"), reason: str(fd, "reason") };
  const bad = invalid<{ appName: string | null; appStatus: string | null }>(validateAssign(input));
  if (bad) return bad;
  const r = await act((c) =>
    c.POST("/transactions/{id}/assign", {
      params: { path: { id } },
      body: { app_id: input.appId.trim(), reason: input.reason.trim() },
    }),
  );
  if (!r.ok) return r;
  revalidateMoney(id, r.data.charge_id);
  // The spec says the response names a suspended app in `app_status`; read it if present.
  const appStatus = (r.data as { app_status?: string }).app_status ?? null;
  return { ok: true, data: { appName: r.data.app_name, appStatus } };
}
