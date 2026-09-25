import type { Metadata } from "next";
import { TradeHistory } from "@/components/TradeHistory";

export const metadata: Metadata = { title: "Trade history · BTCUSDT" };

export default function TradesPage() {
  return (
    <main className="mx-auto flex max-w-5xl flex-col gap-3 p-3">
      <TradeHistory />
    </main>
  );
}
