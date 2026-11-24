import { getPayPage, log } from "@/lib/pay";

// Minimal status for the page's polling: no bill details, no caching. The
// latest payment prompt is reduced to its status and customer message.
export async function GET(_: Request, { params }: { params: Promise<{ token: string }> }) {
  const { token } = await params;
  try {
    const res = await getPayPage(token);
    if (!res.found) return Response.json({ error: "not_found" }, { status: 404, headers: { "Cache-Control": "no-store" } });
    const { status, amount_paid, payable, stk_push } = res.page;
    return Response.json(
      { status, amount_paid, payable, stk_push: stk_push ? { status: stk_push.status, message: stk_push.message } : null },
      { headers: { "Cache-Control": "no-store" } },
    );
  } catch (e) {
    const status = typeof e === "object" && e && "status" in e && e.status === 429 ? 429 : 502;
    if (status !== 429) log.error({ err: e instanceof Error ? e.message : String(e) }, "status lookup failed");
    return Response.json({ error: "unavailable" }, { status, headers: { "Cache-Control": "no-store" } });
  }
}
