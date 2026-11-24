// Liveness probe for the container healthcheck. It does not call the Go
// service, so a backend outage does not restart the frontend.
export function GET() {
  return Response.json({ status: "ok" }, { headers: { "Cache-Control": "no-store" } });
}
