"use server";

import { redirect } from "next/navigation";

import { act, type ActionResult } from "@/lib/api";
import { clearSession } from "@/lib/session";

export async function changePassword(_: ActionResult | undefined, fd: FormData): Promise<ActionResult> {
  const current = String(fd.get("current_password") ?? "");
  const next = String(fd.get("new_password") ?? "");
  if (next !== String(fd.get("confirm") ?? "")) return { ok: false, error: "The new passwords don’t match." };
  const r = await act((c) => c.POST("/auth/password", { body: { current_password: current, new_password: next } }));
  if (!r.ok) return r;
  // Changing the password signs out every session, including this one.
  await clearSession();
  redirect("/login?ended=1");
}
