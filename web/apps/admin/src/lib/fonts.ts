import { IBM_Plex_Mono, IBM_Plex_Sans, Sora } from "next/font/google";

// Fonts are downloaded at build time and served from this app (no request
// to Google at runtime), so the CSP can stay font-src 'self'.
export const sora = Sora({ subsets: ["latin"], weight: ["500", "600", "700"], variable: "--font-sora", display: "swap" });
export const plex = IBM_Plex_Sans({ subsets: ["latin"], weight: ["400", "500", "600"], variable: "--font-plex", display: "swap" });
export const plexMono = IBM_Plex_Mono({ subsets: ["latin"], weight: ["400", "500"], variable: "--font-plex-mono", display: "swap" });
