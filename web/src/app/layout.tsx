import type { Metadata, Viewport } from "next";
import localFont from "next/font/local";
import { ThemeProvider } from "@/components/theme-provider";
import { AuthGuard } from "@/components/auth-guard";
import { AppShell } from "@/components/app-shell";
import { I18nProvider } from "@/lib/i18n";
import "./globals.css";
import { cn } from "@/lib/utils";

const figtreeHeading = localFont({
  src: "./fonts/Figtree.woff2",
  variable: "--font-heading",
  weight: "400 900",
});

// Body typeface is Vercel Geist Sans — the font the Geist design system was
// built around. Bundled locally (./fonts/GeistVF.woff) so no network request.
const geistSans = localFont({
  src: "./fonts/GeistVF.woff",
  variable: "--font-sans",
  weight: "100 900",
});

const geistMono = localFont({
  src: "./fonts/GeistMonoVF.woff",
  variable: "--font-mono",
  weight: "100 900",
});

export const metadata: Metadata = {
  title: "Fluctio",
  description: "AI Agent Framework",
  manifest: "/manifest.json",
  appleWebApp: {
    capable: true,
    title: "Fluctio",
    statusBarStyle: "default",
  },
};

// themeColor drives the <meta name="theme-color"> tag — the install
// prompt and installed-app window chrome tint off this. Lives in viewport
// (not metadata) per Next 14+ Metadata API. viewportFit=cover extends the
// webview under the notch so the safe-area insets the full-screen dialogs
// apply (env(safe-area-inset-*)) resolve to real values.
export const viewport: Viewport = {
  themeColor: "#1890ff",
  viewportFit: "cover",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en" suppressHydrationWarning className={cn("font-sans", geistSans.variable, figtreeHeading.variable, geistMono.variable)}>
      <head>
        <script
          dangerouslySetInnerHTML={{
            __html: `(function(){try{var t=localStorage.getItem('fluctio-theme');if(t==='light')return;document.documentElement.classList.add('dark')}catch(e){document.documentElement.classList.add('dark')}})()`,
          }}
        />
      </head>
      <body className="antialiased">
        <ThemeProvider><I18nProvider><AuthGuard><AppShell>{children}</AppShell></AuthGuard></I18nProvider></ThemeProvider>
        <script
          dangerouslySetInnerHTML={{
            __html: `if('serviceWorker' in navigator){window.addEventListener('load',function(){navigator.serviceWorker.register('/sw.js').catch(function(e){console.warn('SW register failed:',e)})})}`,
          }}
        />
      </body>
    </html>
  );
}
