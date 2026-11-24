import type { Metadata } from "next";

import { LoginForm } from "./LoginForm";

export const metadata: Metadata = { title: "Sign in" };

export default async function LoginPage({ searchParams }: { searchParams: Promise<{ ended?: string }> }) {
  const { ended } = await searchParams;
  return (
    <>
      <h1 className="font-display text-2xl font-semibold tracking-[-0.015em]">Sign in</h1>
      <p className="mt-2 text-sm text-muted">Use your Talksasa staff email. You’ll confirm with your authenticator app next.</p>
      {ended ? (
        <p className="mt-6 rounded-md border border-line bg-wash px-3 py-2.5 text-sm text-ink-2">
          Your session ended, so you were signed out. Sign in again to continue.
        </p>
      ) : null}
      <LoginForm />
    </>
  );
}
