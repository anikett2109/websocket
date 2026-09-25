import { describe, expect, it } from "vitest";
import type { ChartDelta } from "../protocol";
import { applyChartDelta, normalizeCandles, type Candle, type ChartState } from "../sync/chart";
import { SyncError } from "../sync/streamSync";

const d = (baseSeq: number, seq: number, start: number, delta: ChartDelta["delta"]): ChartDelta => ({ kind: "chart", baseSeq, seq, start, delta });

const base: ChartState = { seq: 10, interval: "1m", closed: [], active: { t: 60_000, o: 100, h: 110, l: 90, c: 105, v: 5 } };

describe("chart delta application", () => {
  it("adds deltas to the active candle", () => {
    const s = applyChartDelta(base, d(10, 14, 60_000, [0, 5, 0, 10, 3]));
    expect(s.active).toEqual({ t: 60_000, o: 100, h: 115, l: 90, c: 115, v: 8 });
    expect(s.seq).toBe(14);
  });

  it("a newer candle start finalises the old candle and opens a fresh one from zero", () => {
    const s1 = applyChartDelta(base, d(10, 12, 60_000, [0, 0, -2, -17, 1])); // final values of old candle
    const s2 = applyChartDelta(s1, d(12, 15, 120_000, [88, 95, 87, 93, 4])); // absolute, not added to old
    expect(s2.closed).toEqual([{ t: 60_000, o: 100, h: 110, l: 88, c: 88, v: 6 }]);
    expect(s2.active).toEqual({ t: 120_000, o: 88, h: 95, l: 87, c: 93, v: 4 });
  });

  it("rejects a base mismatch and a delta for an older candle", () => {
    expect(() => applyChartDelta(base, d(9, 12, 60_000, [0, 0, 0, 0, 0]))).toThrow(SyncError);
    expect(() => applyChartDelta(base, d(10, 12, 0, [1, 1, 1, 1, 1]))).toThrow(SyncError);
  });

  it("normalises history: sorted and duplicate candles removed", () => {
    const c = (t: number, v: number): Candle => ({ t, o: 1, h: 1, l: 1, c: 1, v });
    expect(normalizeCandles([c(3, 1), c(1, 1), c(3, 2), c(2, 1)]).map((x) => [x.t, x.v])).toEqual([
      [1, 1],
      [2, 1],
      [3, 2],
    ]);
  });

  it("starts from empty history (no active candle)", () => {
    const empty: ChartState = { seq: 0, interval: "5m", closed: [], active: null };
    const s = applyChartDelta(empty, d(0, 1, 300_000, [7, 7, 7, 7, 1]));
    expect(s.active).toEqual({ t: 300_000, o: 7, h: 7, l: 7, c: 7, v: 1 });
  });
});
