import { describe, expect, it, vi } from "vitest";
import type { DepthDelta } from "../protocol";
import { applyDepthDelta, type BookState } from "../sync/book";
import { StreamSync, type SyncIO } from "../sync/streamSync";

const TICK = 50;

/** Canonical book at seq with a best bid; quantities are a deterministic function of (seq, level). */
function book(seq: number, bestBid: number): BookState {
  const bids = [];
  const asks = [];
  for (let i = 0; i < 10; i++) {
    bids.push({ price: bestBid - i * TICK, qty: 1000 * (seq + i + 1) });
    asks.push({ price: bestBid + TICK + i * TICK, qty: 2000 * (seq + i + 1) });
  }
  return { seq, bids, asks };
}

/** Server-side diff (mirror of backend orderbook.Diff). */
function diff(base: BookState, cur: BookState): DepthDelta {
  const at = (ls: BookState["bids"], p: number) => ls.find((l) => l.price === p)?.qty ?? 0;
  return {
    kind: "depth",
    baseSeq: base.seq,
    seq: cur.seq,
    ts: 0,
    bestBid: cur.bids[0].price,
    bestAsk: cur.asks[0].price,
    bid: cur.bids.map((l) => l.qty - at(base.bids, l.price)),
    ask: cur.asks.map((l) => l.qty - at(base.asks, l.price)),
  };
}

// A drifting canonical history: the best bid moves by a tick every few seqs.
const canon = (seq: number) => book(seq, 6_500_000 + Math.floor(seq / 3) * TICK);

function harness() {
  const io = {
    requestSnapshot: vi.fn<(t: number) => void>(),
    sendSync: vi.fn<(s: number) => void>(),
    onState: vi.fn(),
  } satisfies SyncIO<BookState>;
  const sync = new StreamSync<BookState, DepthDelta>("book", (s, d) => applyDepthDelta(s, d, TICK), io);
  const token = () => io.requestSnapshot.mock.calls.at(-1)![0];
  return { io, sync, token };
}

describe("order book snapshot + delta synchronisation", () => {
  it("snapshot 100 then deltas 101, 102, 103 produce the canonical book", () => {
    const { io, sync, token } = harness();
    sync.start("initial");
    sync.onSnapshot(canon(100), token());
    expect(io.sendSync).toHaveBeenCalledWith(100); // nothing buffered: rebase the server to 100
    sync.onSynced(100);
    expect(sync.phase).toBe("live");

    for (const s of [101, 102, 103]) sync.onDelta(diff(canon(s - 1), canon(s)));
    expect(sync.state).toEqual(canon(103));
  });

  it("coalesced deltas (slow tier) spanning price shifts are exact", () => {
    const { sync, token } = harness();
    sync.start("initial");
    sync.onSnapshot(canon(100), token());
    sync.onSynced(100);
    sync.onDelta(diff(canon(100), canon(110)));
    sync.onDelta(diff(canon(110), canon(125)));
    expect(sync.state).toEqual(canon(125));
  });

  it("detects a gap (100, 101, 103), re-snapshots and resumes", () => {
    const { io, sync, token } = harness();
    sync.start("initial");
    sync.onSnapshot(canon(100), token());
    sync.onSynced(100);
    sync.onDelta(diff(canon(100), canon(101)));
    expect(io.requestSnapshot).toHaveBeenCalledTimes(1);

    sync.onDelta(diff(canon(102), canon(103))); // 102 was lost
    expect(sync.phase).toBe("snapshotting");
    expect(io.requestSnapshot).toHaveBeenCalledTimes(2);
    expect(sync.state!.seq).toBe(101); // local book not corrupted by the bad delta

    sync.onSnapshot(canon(103), token());
    sync.onSynced(103);
    sync.onDelta(diff(canon(103), canon(104)));
    expect(sync.state).toEqual(canon(104));
    expect(sync.phase).toBe("live");
  });

  it("buffers deltas while the snapshot is in flight and replays the ones chaining from it", () => {
    const { io, sync, token } = harness();
    sync.start("initial");
    // Deltas arrive before the REST response.
    for (const s of [99, 100, 101, 102]) sync.onDelta(diff(canon(s - 1), canon(s)));
    sync.onSnapshot(canon(100), token()); // snapshot taken at 100
    // 99 and 100 are covered by the snapshot and dropped; 101, 102 chain from 100.
    expect(sync.state).toEqual(canon(102));
    expect(sync.phase).toBe("live");
    expect(io.sendSync).not.toHaveBeenCalled(); // fast path, no server rebase needed
  });

  it("falls back to SYNC when buffered coalesced deltas straddle the snapshot seq", () => {
    const { io, sync, token } = harness();
    sync.start("initial");
    sync.onDelta(diff(canon(95), canon(105))); // spans the snapshot: cannot be partially applied
    sync.onSnapshot(canon(100), token());
    expect(io.sendSync).toHaveBeenCalledWith(100);
    expect(sync.phase).toBe("syncing");
    sync.onDelta(diff(canon(105), canon(110))); // pre-rebase packet: ignored, not a gap
    expect(sync.state).toEqual(canon(100));
    sync.onSynced(100);
    sync.onDelta(diff(canon(100), canon(110)));
    expect(sync.state).toEqual(canon(110));
  });

  it("ignores a snapshot response that arrives after a newer request", () => {
    const { sync, token } = harness();
    sync.start("initial");
    const stale = token();
    sync.start("retry");
    sync.onSnapshot(canon(50), stale);
    expect(sync.state).toBeNull();
    sync.onSnapshot(canon(60), token());
    expect(sync.state!.seq).toBe(60);
  });

  it("ignores duplicate deltas and stops on disconnect", () => {
    const { sync, token } = harness();
    sync.start("initial");
    sync.onSnapshot(canon(10), token());
    sync.onSynced(10);
    const d = diff(canon(10), canon(11));
    sync.onDelta(d);
    sync.onDelta(d); // duplicate
    expect(sync.state).toEqual(canon(11));
    sync.stop();
    sync.onDelta(diff(canon(11), canon(12)));
    expect(sync.state).toEqual(canon(11)); // kept (shown stale), not updated
    expect(sync.phase).toBe("idle");
  });
});
