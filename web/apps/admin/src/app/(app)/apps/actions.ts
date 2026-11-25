"use server";

import { revalidatePath } from "next/cache";

import { act, type ActionResult } from "@/lib/api";
import { splitAllowedIPs } from "@/lib/webhooks";

// Permissions are enforced by the Go service on every call; the UI only
// hides controls a role cannot use.

function str(fd: FormData, k: string): string {
  const v = fd.get(k);
  return typeof v === "string" ? v.trim() : "";
}

export type SecretResult = ActionResult<{ secret: string; appId?: string }> | undefined;

export async function createApp(_: SecretResult, fd: FormData): Promise<SecretResult> {
  const r = await act((c) =>
    c.POST("/apps", {
      body: { name: str(fd, "name"), account_prefix: str(fd, "account_prefix").toUpperCase(), webhook_url: str(fd, "webhook_url") },
    }),
  );
  if (!r.ok) return r;
  revalidatePath("/apps");
  return { ok: true, data: { secret: r.data.webhook_secret, appId: r.data.app.id } };
}

export async function setWebhookURL(id: string, _: ActionResult | undefined, fd: FormData): Promise<ActionResult> {
  const r = await act((c) => c.PUT("/apps/{id}/webhook-url", { params: { path: { id } }, body: { webhook_url: str(fd, "webhook_url") } }));
  if (!r.ok) return r;
  revalidatePath(`/apps/${id}`);
  return { ok: true, data: undefined };
}

export async function rotateWebhookSecret(id: string, _: SecretResult): Promise<SecretResult> {
  const r = await act((c) => c.POST("/apps/{id}/rotate-webhook-secret", { params: { path: { id } } }));
  if (!r.ok) return r;
  return { ok: true, data: { secret: r.data.webhook_secret } };
}

export async function setAppStatus(id: string, status: "active" | "suspended", _: ActionResult | undefined): Promise<ActionResult> {
  const r = await act((c) => c.POST("/apps/{id}/status", { params: { path: { id } }, body: { status } }));
  if (!r.ok) return r;
  revalidatePath(`/apps/${id}`);
  revalidatePath("/apps");
  return { ok: true, data: undefined };
}

export async function issueAPIKey(id: string, _: SecretResult, fd: FormData): Promise<SecretResult> {
  const expires = str(fd, "expires");
  const r = await act((c) =>
    c.POST("/apps/{id}/api-keys", {
      params: { path: { id } },
      // A date means "valid through the end of that day, Kenyan time".
      body: { label: str(fd, "label"), expires_at: expires ? `${expires}T23:59:59+03:00` : null },
    }),
  );
  if (!r.ok) return r;
  revalidatePath(`/apps/${id}`);
  return { ok: true, data: { secret: r.data.api_key } };
}

export async function revokeAPIKey(id: string, keyID: string, _: ActionResult | undefined): Promise<ActionResult> {
  const r = await act((c) => c.POST("/apps/{id}/api-keys/{keyID}/revoke", { params: { path: { id, keyID } } }));
  if (!r.ok) return r;
  revalidatePath(`/apps/${id}`);
  return { ok: true, data: undefined };
}

export type AllowedIPsResult = ActionResult<{ allowed_ips: string[] | null }> | undefined;

export async function setAllowedIPs(id: string, keyID: string, _: AllowedIPsResult, fd: FormData): Promise<AllowedIPsResult> {
  // The Go service parses and validates the list; this only splits the textarea.
  const allowed_ips = splitAllowedIPs(str(fd, "allowed_ips"));
  const r = await act((c) =>
    c.PUT("/apps/{id}/api-keys/{keyID}/allowed-ips", { params: { path: { id, keyID } }, body: { allowed_ips } }),
  );
  if (!r.ok) return r;
  revalidatePath(`/apps/${id}`);
  return { ok: true, data: { allowed_ips: r.data.allowed_ips ?? null } };
}
