import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import { FeedProvider } from "@/components/FeedProvider";
import "./globals.css";

const geistSans = Geist({ variable: "--font-geist-sans", subsets: ["latin"] });
const geistMono = Geist_Mono({ variable: "--font-geist-mono", subsets: ["latin"] });

export const metadata: Metadata = {
  title: "BTCUSDT · Adaptive Feed",
  description: "Simulated crypto market with adaptive, latency-aware live delivery",
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="en" className={`${geistSans.variable} ${geistMono.variable} h-full antialiased`}>
      <body className="min-h-full">
        <FeedProvider>{children}</FeedProvider>
      </body>
    </html>
  );
}
