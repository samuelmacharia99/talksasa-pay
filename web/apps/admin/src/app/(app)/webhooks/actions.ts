"use server";

import { revalidatePath } from "next/cache";

import { act, type ActionResult } from "@/lib/api";

// The Go service checks products.manage on every call; the UI only hides
// the button from roles that cannot use it.

export type RedeliverResult = ActionResult<{ status: string }> | undefined;

export async function redeliver(eventID: string, _: RedeliverResult): Promise<RedeliverResult> {
  const r = await act((c) => c.POST("/webhook-deliveries/{eventID}/redeliver", { params: { path: { eventID } } }));
  if (!r.ok) return r;
  revalidatePath(`/webhooks/${eventID}`);
  revalidatePath("/webhooks");
  return { ok: true, data: { status: r.data.status } };
}
