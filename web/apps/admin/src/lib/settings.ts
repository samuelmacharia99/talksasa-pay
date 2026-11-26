// Pure helpers for the Settings page (M-Pesa paybill). No server-only
// imports: client components use these too.

export type Credential = { set: boolean; last4: string | null };

/** What a stored credential looks like on the page: never its value. */
export function credentialLabel(c: Credential): string {
  if (!c.set) return "Not set";
  return c.last4 ? `Saved ••••${c.last4}` : "Saved";
}

export const SOURCE_LABEL: Record<string, { label: string; help: string }> = {
  database: { label: "Saved here", help: "These settings were saved on this page and are in use." },
  environment: {
    label: "Deployment configuration",
    help: "Nothing is saved here yet, so the TSP_MPESA_* values from the deployment configuration (SOPS) are in use. Saving this form takes over from them.",
  },
  none: { label: "Off", help: "M-Pesa is off: nothing is saved here and the deployment configuration sets none." },
};

/** The secret callback token is shown as dots. */
export function maskCallbackURL(url: string): string {
  return url.replace("[TOKEN]", "••••••••");
}

export const CONFIRM_WORD = "production";

/** A typed confirmation is needed when production is the saved or the chosen environment. */
export function needsConfirmation(saved: string | null, chosen: string): boolean {
  return saved === "production" || chosen === "production";
}

/** Light checks before sending; the Go service is authoritative. */
export function checkSettingsForm(v: { shortcode: string; stk_shortcode: string }): { field: string; message: string } | undefined {
  if (!/^\d{5,7}$/.test(v.shortcode)) return { field: "shortcode", message: "The paybill number must be 5-7 digits." };
  if (v.stk_shortcode && !/^\d{5,7}$/.test(v.stk_shortcode)) {
    return { field: "stk_shortcode", message: "The STK shortcode must be 5-7 digits, or empty to use the paybill." };
  }
  return undefined;
}

/**
 * The callback base URL generated from this panel's own public address:
 * the Go service answers under /api on the same host (the host's gateway
 * routes /api/* to it). null when the address is not a plain host[:port].
 * `host` is X-Forwarded-Host (set by the gateway) or Host; `proto` is
 * X-Forwarded-Proto.
 */
export function derivedCallbackBase(host: string | null, proto: string | null): string | null {
  const h = host?.split(",")[0]?.trim().toLowerCase() ?? "";
  if (!/^[a-z0-9.-]+(:\d{1,5})?$/.test(h) || h.startsWith(".") || h.startsWith("-")) return null;
  const p = proto?.split(",")[0]?.trim().toLowerCase();
  const local = /^(localhost|127\.0\.0\.1)(:\d+)?$/.test(h);
  const scheme = p === "http" || p === "https" ? p : local ? "http" : "https";
  return `${scheme}://${h}/api/`;
}

/** What the Settings page says about the callback token. */
export function callbackTokenLabel(source: string | null): string {
  if (source === "deployment") return "Set in the deployment configuration (TSP_MPESA_CALLBACK_TOKEN)";
  if (source === "generated") return "Generated and stored encrypted";
  return "Generated when you save the settings";
}
