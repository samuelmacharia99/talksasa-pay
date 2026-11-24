import { type NextRequest, NextResponse } from "next/server";

const REQUEST_ID = /^[A-Za-z0-9._-]{8,64}$/;

/**
 * Runs before every page: issues a per-request CSP nonce (Next.js applies it
 * to its own scripts) and a request id that is forwarded to the Go service
 * so frontend and backend log lines can be correlated.
 */
export function proxy(req: NextRequest) {
  const nonce = Buffer.from(crypto.randomUUID()).toString("base64");
  const incoming = req.headers.get("x-request-id");
  const requestId = incoming && REQUEST_ID.test(incoming) ? incoming : crypto.randomUUID();
  const dev = process.env.NODE_ENV !== "production";

  const csp = [
    "default-src 'self'",
    `script-src 'self' 'nonce-${nonce}' 'strict-dynamic'${dev ? " 'unsafe-eval'" : ""}`,
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self'",
    `connect-src 'self'${dev ? " ws:" : ""}`,
    "frame-ancestors 'none'",
    "form-action 'self'",
    "base-uri 'none'",
    "object-src 'none'",
  ].join("; ");

  const forwarded = new Headers(req.headers);
  forwarded.set("x-nonce", nonce);
  forwarded.set("x-request-id", requestId);
  forwarded.set("Content-Security-Policy", csp);

  const res = NextResponse.next({ request: { headers: forwarded } });
  res.headers.set("Content-Security-Policy", csp);
  res.headers.set("X-Request-Id", requestId);
  res.headers.set("Cache-Control", "no-store");
  return res;
}

export const config = {
  matcher: [{ source: "/((?!_next/static|_next/image|favicon.ico|brand/).*)" }],
};
