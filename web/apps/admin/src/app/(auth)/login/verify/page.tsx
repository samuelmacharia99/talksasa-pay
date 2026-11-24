import type { Metadata } from "next";
import Link from "next/link";
import { redirect } from "next/navigation";

import { getChallenge } from "@/lib/session";

import { VerifyForm } from "./VerifyForm";

export const metadata: Metadata = { title: "Confirm sign-in" };

export default async function VerifyPage() {
  if (!(await getChallenge())) redirect("/login");
  return (
    <>
      <h1 className="font-display text-2xl font-semibold tracking-[-0.015em]">Confirm it’s you</h1>
      <p className="mt-2 text-sm text-muted">
        Enter the 6-digit code from your authenticator app. Lost your phone? Use one of your recovery codes instead.
      </p>
      <VerifyForm />
      <Link href="/login" className="mt-6 inline-block text-sm text-violet hover:underline">
        Start again
      </Link>
    </>
  );
}
