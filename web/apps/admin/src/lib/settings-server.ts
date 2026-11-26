import "server-only";

import { headers } from "next/headers";

import { derivedCallbackBase } from "./settings";

/** The callback base URL for the address this request reached the panel on. */
export async function callbackBaseForThisPanel(): Promise<string | null> {
  const h = await headers();
  return derivedCallbackBase(h.get("x-forwarded-host") ?? h.get("host"), h.get("x-forwarded-proto"));
}
