import { CandleChart } from "@/components/CandleChart";
import { OrderBook } from "@/components/OrderBook";
import { RecentTrades } from "@/components/RecentTrades";
import { DebugPanel, EventLog, TierPanel } from "@/components/TierPanel";
import { TopBar } from "@/components/TopBar";

// The page is a server component that only lays out client islands; all live
// data flows through the single FeedClient provided in the root layout.
export default function TradingScreen() {
  return (
    <main className="mx-auto flex max-w-[1680px] flex-col gap-3 p-3">
      <TopBar />
      {/* xl: chart+log | book+trades | tier+debug.  lg: two columns, panels wrap below. */}
      <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_320px] xl:grid-cols-[minmax(0,1fr)_320px_340px]">
        <div className="flex min-w-0 flex-col xl:col-start-1 xl:row-start-1">
          <CandleChart />
        </div>
        <div className="flex flex-col gap-3 xl:col-start-2 xl:row-span-2 xl:row-start-1">
          <OrderBook />
          <RecentTrades />
        </div>
        <div className="grid content-start gap-3 md:grid-cols-2 lg:col-span-2 xl:col-span-1 xl:col-start-3 xl:row-span-2 xl:row-start-1 xl:grid-cols-1">
          <TierPanel />
          <DebugPanel />
        </div>
        <div className="lg:col-span-2 xl:col-span-1 xl:col-start-1 xl:row-start-2">
          <EventLog />
        </div>
      </div>
    </main>
  );
}
