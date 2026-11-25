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
export function checkSettingsForm(v: { shortcode: string; stk_shortcode: string; callback_base_url: string; https_only: boolean }):
  | { field: string; message: string }
  | undefined {
  if (!/^\d{5,7}$/.test(v.shortcode)) return { field: "shortcode", message: "The paybill number must be 5-7 digits." };
  if (v.stk_shortcode && !/^\d{5,7}$/.test(v.stk_shortcode)) {
    return { field: "stk_shortcode", message: "The STK shortcode must be 5-7 digits, or empty to use the paybill." };
  }
  let u: URL;
  try {
    u = new URL(v.callback_base_url);
  } catch {
    return { field: "callback_base_url", message: "Enter an absolute URL such as https://api.pay.example.com/." };
  }
  if (u.protocol !== "https:" && (v.https_only || u.protocol !== "http:")) {
    return { field: "callback_base_url", message: "The callback base URL must use https." };
  }
  if (!v.callback_base_url.endsWith("/")) return { field: "callback_base_url", message: "The callback base URL must end with /." };
  return undefined;
}
