import { isApiError } from "@talksasa/api-client/errors";

import { log, sendStk } from "@/lib/pay";
import { normalizeKenyanPhone } from "@/lib/stk";

const NO_STORE = { "Cache-Control": "no-store" };
const MAX_BODY = 1024;
/** Go answers the browser may learn about; anything else becomes a generic 502. */
const PASS_THROUGH = new Set([404, 409, 422, 429, 502, 503]);

function answer(status: number, body: Record<string, unknown>) {
  return Response.json(body, { status, headers: NO_STORE });
}

/**
 * Sends an M-Pesa payment prompt for the bill behind the link. The browser
 * only ever talks to this same-origin handler; it forwards to the Go public
 * API server-side, so the page's CSP stays connect-src 'self'.
 *
 * Only JSON from this site is accepted: a cross-site form cannot send
 * application/json without a CORS preflight, which is never granted, and
 * browsers that send Sec-Fetch-Site are checked too.
 */
export async function POST(req: Request, { params }: { params: Promise<{ token: string }> }) {
  const { token } = await params;

  const site = req.headers.get("sec-fetch-site");
  if (site && site !== "same-origin") return answer(403, { code: "forbidden" });
  if (!(req.headers.get("content-type") ?? "").toLowerCase().startsWith("application/json")) {
    return answer(415, { code: "unsupported_media_type" });
  }

  const raw = await req.text();
  if (raw.length > MAX_BODY) return answer(413, { code: "payload_too_large" });
  let phoneInput: unknown;
  try {
    phoneInput = (JSON.parse(raw) as { phone?: unknown } | null)?.phone;
  } catch {
    return answer(400, { code: "invalid_json" });
  }
  const phone = typeof phoneInput === "string" ? normalizeKenyanPhone(phoneInput) : null;
  if (!phone) return answer(422, { code: "validation_failed" });

  try {
    const stk = await sendStk(token, phone);
    return answer(202, { stk_push: { status: stk.status, message: stk.message } });
  } catch (e) {
    if (isApiError(e) && PASS_THROUGH.has(e.status)) {
      if (e.status >= 500) log.warn({ status: e.status, code: e.code, requestId: e.requestId }, "stk push refused");
      return answer(e.status, { code: e.code });
    }
    log.error({ err: e instanceof Error ? e.message : String(e) }, "stk push failed");
    return answer(502, { code: "upstream_error" });
  }
}
