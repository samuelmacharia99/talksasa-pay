/** Per-request values forwarded to the Go service. */
export interface ForwardContext {
  /** Correlates frontend and Go log lines. */
  requestId?: string;
  /** The end user's IP; Go trusts it only from configured proxies. */
  clientIp?: string;
  /** The end user's browser user agent (audit log only). */
  clientUa?: string;
}

const REQUEST_ID = /^[A-Za-z0-9._-]{8,64}$/;

export function forwardHeaders(ctx: ForwardContext): Record<string, string> {
  const h: Record<string, string> = { Accept: "application/json" };
  if (ctx.requestId && REQUEST_ID.test(ctx.requestId)) h["X-Request-Id"] = ctx.requestId;
  if (ctx.clientIp) h["X-Talksasa-Client-Ip"] = ctx.clientIp.slice(0, 64);
  if (ctx.clientUa) h["X-Talksasa-Client-Ua"] = ctx.clientUa.slice(0, 300);
  return h;
}
