import { OPENAPI_YAML } from "@/content/developer-docs";
import { requireMe } from "@/lib/api";

/** The app API contract (api/app.openapi.yaml), for signed-in staff. */
export async function GET() {
  await requireMe();
  return new Response(OPENAPI_YAML, {
    headers: {
      "Content-Type": "application/yaml; charset=utf-8",
      "Content-Disposition": 'attachment; filename="talksasa-pay-app.openapi.yaml"',
      "Cache-Control": "no-store",
    },
  });
}
