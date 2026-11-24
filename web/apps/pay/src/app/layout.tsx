import "./globals.css";

import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";

import { plex, plexMono, sora } from "@/lib/fonts";

export const metadata: Metadata = {
  title: { default: "Pay your Talksasa bill", template: "%s · Talksasa" },
  robots: { index: false, follow: false },
  icons: { icon: "/favicon.ico" },
};

export const viewport: Viewport = { themeColor: "#f7f6fb", width: "device-width", initialScale: 1 };

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en-KE" className={`${sora.variable} ${plex.variable} ${plexMono.variable}`}>
      <body className="min-h-dvh bg-paper text-ink antialiased">{children}</body>
    </html>
  );
}
