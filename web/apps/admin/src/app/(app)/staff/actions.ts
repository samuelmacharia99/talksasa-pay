"use server";

import { revalidatePath } from "next/cache";

import { act, type ActionResult } from "@/lib/api";

type Role = "viewer" | "finance" | "approver" | "admin";
const ROLES: Role[] = ["viewer", "finance", "approver", "admin"];

function str(fd: FormData, k: string): string {
  const v = fd.get(k);
  return typeof v === "string" ? v.trim() : "";
}

function role(fd: FormData): Role | null {
  const r = str(fd, "role");
  return (ROLES as string[]).includes(r) ? (r as Role) : null;
}

export type LinkResult = ActionResult<{ url: string; name: string }> | undefined;

export async function inviteStaff(_: LinkResult, fd: FormData): Promise<LinkResult> {
  const r0 = role(fd);
  if (!r0) return { ok: false, error: "Choose a role." };
  const r = await act((c) => c.POST("/users", { body: { email: str(fd, "email"), name: str(fd, "name"), role: r0 } }));
  if (!r.ok) return r;
  revalidatePath("/staff");
  return { ok: true, data: { url: r.data.setup_url, name: r.data.user.name } };
}

export async function changeRole(id: string, _: ActionResult | undefined, fd: FormData): Promise<ActionResult> {
  const r0 = role(fd);
  if (!r0) return { ok: false, error: "Choose a role." };
  const r = await act((c) => c.POST("/users/{id}/role", { params: { path: { id } }, body: { role: r0 } }));
  if (!r.ok) return r;
  revalidatePath("/staff");
  return { ok: true, data: undefined };
}

export async function disableStaff(id: string, _: ActionResult | undefined): Promise<ActionResult> {
  const r = await act((c) => c.POST("/users/{id}/disable", { params: { path: { id } } }));
  if (!r.ok) return r;
  revalidatePath("/staff");
  return { ok: true, data: undefined };
}

export async function resetStaff(id: string, _: LinkResult): Promise<LinkResult> {
  const r = await act((c) => c.POST("/users/{id}/reset", { params: { path: { id } } }));
  if (!r.ok) return r;
  revalidatePath("/staff");
  return { ok: true, data: { url: r.data.setup_url, name: r.data.user.name } };
}
