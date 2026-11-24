"use server";

import { revalidatePath } from "next/cache";

import { act, type ActionResult } from "@/lib/api";
import { normalizeReceipt, type ResolveAction, validateResolve } from "@/lib/mpesa";

// The Go service checks payments.resolve (finance, admin) and that the
// transaction is still needs_review; this only validates the form first.

export type ResolveResult = ActionResult<{ status: string }> | undefined;

function str(fd: FormData, k: string): string {
  const v = fd.get(k);
  return typeof v === "string" ? v : "";
}

export async function resolveTransaction(id: string, action: ResolveAction, _: ResolveResult, fd: FormData): Promise<ResolveResult> {
  const input = { action, receipt: str(fd, "receipt"), reason: str(fd, "reason") };
  const errors = validateResolve(input);
  const first = Object.entries(errors)[0];
  if (first) {
    return { ok: false, error: first[1], field: first[0], fieldErrors: Object.entries(errors).map(([field, message]) => ({ field, message })) };
  }
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
  revalidatePath(`/transactions/${id}`);
  revalidatePath("/transactions");
  revalidatePath("/");
  if (r.data.payment_request_id) revalidatePath(`/requests/${r.data.payment_request_id}`);
  return { ok: true, data: { status: r.data.status } };
}
