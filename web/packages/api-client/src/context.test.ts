import { describe, expect, it } from "vitest";
import { forwardHeaders } from "./context";
import { ApiError, toApiError } from "./errors";

describe("forwardHeaders", () => {
  it("forwards a well-formed request id and drops injected ones", () => {
    expect(forwardHeaders({ requestId: "abc-123_XYZ" })["X-Request-Id"]).toBe("abc-123_XYZ");
    expect(forwardHeaders({ requestId: "bad id\r\nX-Evil: 1" })["X-Request-Id"]).toBeUndefined();
  });
  it("caps forwarded client values", () => {
    const h = forwardHeaders({ clientIp: "203.0.113.9", clientUa: "x".repeat(1000) });
    expect(h["X-Talksasa-Client-Ip"]).toBe("203.0.113.9");
    expect(h["X-Talksasa-Client-Ua"]).toHaveLength(300);
  });
});

describe("toApiError", () => {
  it("keeps problem details", () => {
    const e = toApiError(409, { status: 409, code: "prefix_taken", detail: "Taken.", request_id: "r1" });
    expect(e).toBeInstanceOf(ApiError);
    expect([e.status, e.code, e.detail, e.requestId]).toEqual([409, "prefix_taken", "Taken.", "r1"]);
  });
  it("never surfaces raw upstream bodies", () => {
    const e = toApiError(502, "<html>nginx stack trace</html>");
    expect(e.detail).not.toContain("nginx");
    expect(e.code).toBe("upstream_error");
  });
  it("keeps well-formed field errors and drops malformed ones", () => {
    const e = toApiError(400, {
      status: 400,
      code: "validation_failed",
      detail: "Check the highlighted fields.",
      errors: [{ field: "allowed_ips[1]", message: "not an IP address" }, { field: 3 }, null],
    });
    expect(e.fieldErrors).toEqual([{ field: "allowed_ips[1]", message: "not an IP address" }]);
    expect(toApiError(404, { status: 404 }).fieldErrors).toEqual([]);
  });
});
