import { describe, expect, it } from "vitest";
import { HealthMeter } from "../latency";
import { decode, decodeFrame, encodePing, MalformedPacketError, PacketType, Sizes } from "../protocol";
import { formatFixed, fmtChangePct } from "../fixed";

function header(type: number, size: number, seq: number, ts: number) {
  const buf = new ArrayBuffer(size);
  const v = new DataView(buf);
  v.setUint8(0, type);
  v.setUint16(1, size, true);
  v.setUint32(3, seq, true);
  v.setBigInt64(7, BigInt(ts), true);
  return { buf, v };
}

describe("binary protocol", () => {
  it("decodes CHART_DELTA (63 B) with baseSeq in the first slot", () => {
    const { buf, v } = header(PacketType.ChartDelta, 63, 45, 1_700_000_040_000);
    v.setBigInt64(15, 41n, true);
    [0, 150, -50, 100, 123456].forEach((x, i) => v.setBigInt64(23 + i * 8, BigInt(x), true));
    expect(decode(buf)).toEqual({ kind: "chart", seq: 45, baseSeq: 41, start: 1_700_000_040_000, delta: [0, 150, -50, 100, 123456] });
  });

  it("decodes DEPTH_DELTA (115 B)", () => {
    const { buf, v } = header(PacketType.DepthDelta, 115, 9, 5);
    v.setUint32(15, 7, true);
    v.setBigInt64(19, 6_500_000n, true);
    v.setBigInt64(27, 6_500_050n, true);
    for (let i = 0; i < 10; i++) {
      v.setInt32(35 + i * 4, -i, true);
      v.setInt32(75 + i * 4, i * 10, true);
    }
    const p = decode(buf);
    expect(p).toMatchObject({ kind: "depth", seq: 9, baseSeq: 7, bestBid: 6_500_000, bestAsk: 6_500_050 });
    if (p.kind === "depth") {
      expect(p.bid[3]).toBe(-3);
      expect(p.ask[9]).toBe(90);
    }
  });

  it("decodes TRADE_UPDATE (143 B): consecutive ids, signed side, deltas", () => {
    const { buf, v } = header(PacketType.TradeUpdate, 143, 500, 1_000_000);
    v.setBigInt64(15, 6_500_000n, true);
    for (let i = 0; i < 10; i++) {
      v.setInt32(23 + i * 12, -i * 50, true);
      v.setInt32(27 + i * 12, i % 2 ? -1000 : 1000, true);
      v.setInt32(31 + i * 12, -i * 50, true);
    }
    const p = decode(buf);
    if (p.kind !== "trades") throw new Error("wrong kind");
    expect(p.trades[0]).toEqual({ id: 500, price: 6_500_000, qty: 1000, side: 1, ts: 1_000_000 });
    expect(p.trades[1]).toEqual({ id: 499, price: 6_499_950, qty: 1000, side: -1, ts: 999_950 });
  });

  it("rejects malformed packets", () => {
    expect(() => decode(new ArrayBuffer(3))).toThrow(MalformedPacketError);
    const { buf } = header(PacketType.ChartDelta, 63, 1, 1);
    new DataView(buf).setUint16(1, 62, true); // lying length field
    expect(() => decode(buf)).toThrow(MalformedPacketError);
    expect(() => decode(header(99, 15, 1, 1).buf)).toThrow(MalformedPacketError); // unknown type
    expect(() => decode(header(PacketType.ChartDelta, 15, 1, 1).buf)).toThrow(MalformedPacketError); // wrong size
  });

  it("encodes PING as a 15 B header echoed back as PONG", () => {
    const ping = encodePing(7, 123456);
    expect(ping.byteLength).toBe(Sizes.probe);
    new DataView(ping).setUint8(0, PacketType.Pong);
    expect(decode(ping)).toEqual({ kind: "pong", seq: 7, ts: 123456 });
  });
});

describe("batched frames", () => {
  it("splits one frame into its packets by the length field", () => {
    const a = header(PacketType.ChartDelta, 63, 7, 60_000).buf;
    const b = header(PacketType.Pong, 15, 3, 99).buf;
    const frame = new Uint8Array(78);
    frame.set(new Uint8Array(a), 0);
    frame.set(new Uint8Array(b), 63);
    const { packets, error } = decodeFrame(frame.buffer);
    expect(error).toBeUndefined();
    expect(packets.map((p) => p.kind)).toEqual(["chart", "pong"]);
  });

  it("keeps packets before a malformed one and reports the error", () => {
    const a = header(PacketType.Pong, 15, 3, 99).buf;
    const frame = new Uint8Array(15 + 20);
    frame.set(new Uint8Array(a), 0);
    frame.set([1, 200, 0], 15); // claims 200 bytes, only 20 left
    const { packets, error } = decodeFrame(frame.buffer);
    expect(packets).toHaveLength(1);
    expect(error).toMatch(/bad packet length/);
  });
});

describe("health report (latency = median5, jitter = MAD5)", () => {
  it("ignores a single spike and scores latency + 4*jitter", () => {
    const m = new HealthMeter();
    for (const r of [89, 90, 88, 118, 91]) m.add(r);
    expect(m.latency).toBe(90);
    expect(m.jitter).toBe(1);
    expect(m.score).toBe(94);
  });
  it("keeps only the last 5 probes", () => {
    const m = new HealthMeter();
    for (const r of [90, 90, 90, 90, 90, 420, 420, 420]) m.add(r);
    expect(m.latency).toBe(420); // 3 of 5 recent samples moved the median
    expect(m.samples).toBe(8);
  });
});

describe("fixed-point formatting", () => {
  it("formats without float drift", () => {
    expect(formatFixed(6_500_005, 100, 2)).toBe("65,000.05");
    expect(formatFixed(-150, 100, 2)).toBe("-1.50");
    expect(formatFixed(123_456, 1_000_000, 4)).toBe("0.1234");
    expect(fmtChangePct(10_000, 10_150)).toBe("+1.50%");
    expect(fmtChangePct(10_000, 9_900)).toBe("-1.00%");
  });
});
