import "./globals.css";

import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";

import { plex, plexMono, sora } from "@/lib/fonts";

export const metadata: Metadata = {
  title: { default: "Talksasa Pay admin", template: "%s · Talksasa Pay admin" },
  robots: { index: false, follow: false },
  icons: { icon: "/favicon.ico" },
};

export const viewport: Viewport = { themeColor: "#1b1530" };

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en-KE" className={`${sora.variable} ${plex.variable} ${plexMono.variable}`}>
      <body className="min-h-dvh bg-paper text-ink antialiased">{children}</body>
    </html>
  );
}
