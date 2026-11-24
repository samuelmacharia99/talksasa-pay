import { clearChallenge, clearSession } from "@/lib/session";

// Reached when the Go service reports the session is no longer valid (idle
// timeout, signed out elsewhere, account disabled). Clears the cookie and
// returns to sign-in. A relative Location keeps this correct behind a proxy.
export async function GET() {
  await clearSession();
  await clearChallenge();
  return new Response(null, { status: 303, headers: { Location: "/login?ended=1", "Cache-Control": "no-store" } });
}
