"use client";

export default function GlobalError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return (
    <html lang="en-KE">
      <body style={{ fontFamily: "system-ui, sans-serif", padding: "4rem 1.5rem", maxWidth: 560, margin: "0 auto", color: "#1b1530" }}>
        <h1 style={{ fontSize: "1.5rem" }}>The admin panel hit an error</h1>
        <p>Try again. If it keeps happening, quote this reference: {error.digest ?? "unknown"}.</p>
        <button type="button" onClick={reset} style={{ marginTop: 16 }}>
          Try again
        </button>
      </body>
    </html>
  );
}
