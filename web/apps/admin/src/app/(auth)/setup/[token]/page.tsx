import { isApiError } from "@talksasa/api-client/errors";
import { formatDateTime } from "@talksasa/ui";
import type { Metadata } from "next";
import QRCode from "qrcode";

import { api } from "@/lib/api";
import { log } from "@/lib/log";

import { completeSetup } from "../../actions";
import { SetupForm } from "./SetupForm";

export const metadata: Metadata = { title: "Set up your account" };

export default async function SetupPage({ params }: { params: Promise<{ token: string }> }) {
  const { token } = await params;
  let info;
  try {
    info = (await (await api()).GET("/auth/setup/{token}", { params: { path: { token } } })).data!;
  } catch (e) {
    if (!isApiError(e) || e.status >= 500) log.error({ err: e instanceof Error ? e.message : String(e) }, "setup lookup failed");
    return (
      <>
        <h1 className="font-display text-2xl font-semibold tracking-[-0.015em]">This setup link can’t be used</h1>
        <p className="mt-3 text-sm text-ink-2">
          {isApiError(e) && e.status < 500
            ? e.detail
            : "The payment service could not be reached. Try the link again in a minute."}
        </p>
      </>
    );
  }
  // Rendered on the server from the otpauth URI; only the enrolling person sees it.
  const qr = await QRCode.toDataURL(info.otpauth_uri, { margin: 1, width: 192, color: { dark: "#1b1530", light: "#ffffff" } });
  return (
    <>
      <h1 className="font-display text-2xl font-semibold tracking-[-0.015em]">Welcome, {info.name.split(" ")[0]}</h1>
      <p className="mt-2 text-sm text-muted">
        Set up sign-in for <span className="text-ink-2">{info.email}</span>. This link works once and expires{" "}
        {formatDateTime(info.expires_at)}.
      </p>
      <SetupForm
        action={completeSetup.bind(null, token)}
        qrDataUrl={qr}
        secret={info.totp_secret}
        enrolment={info.enrolment}
      />
    </>
  );
}
