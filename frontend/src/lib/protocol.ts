// Binary wire format (little-endian). Mirrors backend/internal/protocol.
//
// Header (15 B): type u8 | length u16 (whole packet) | seq u32 | timestamp i64
//   CHART_DELTA  63 B: baseSeq (i64 slot), open/high/low/close/volume deltas (i64)
//   DEPTH_DELTA 115 B: baseSeq u32, bestBid i64, bestAsk i64, 10 bid + 10 ask qty deltas (i32)
//   TRADE_UPDATE 143 B: ltp i64, 10 x (priceDelta i32, signedQty i32, timeDelta i32)
//   PING/PONG    15 B: header only (seq = probe seq, ts = sender clock, echoed)

export const PacketType = {
  ChartDelta: 1,
  DepthDelta: 2,
  TradeUpdate: 3,
  Ping: 4,
  Pong: 5,
} as const;

export const Sizes = { header: 15, chart: 63, depth: 115, trade: 143, probe: 15 } as const;
export const LEVELS = 10;

export class MalformedPacketError extends Error {}

export interface ChartDelta {
  kind: "chart";
  seq: number;
  baseSeq: number;
  start: number; // candle start, unix ms
  delta: [number, number, number, number, number]; // open, high, low, close, volume
}

export interface DepthDelta {
  kind: "depth";
  seq: number;
  baseSeq: number;
  ts: number;
  bestBid: number;
  bestAsk: number;
  bid: number[];
  ask: number[];
}

export interface Trade {
  id: number;
  ts: number;
  price: number;
  qty: number;
  side: 1 | -1;
}

export interface TradeUpdate {
  kind: "trades";
  newestId: number;
  ts: number;
  trades: Trade[]; // newest first
}

interface Pong {
  kind: "pong";
  seq: number;
  ts: number;
}

export type Packet = ChartDelta | DepthDelta | TradeUpdate | Pong;

function i64(v: DataView, off: number): number {
  const n = v.getBigInt64(off, true);
  if (n > BigInt(Number.MAX_SAFE_INTEGER) || n < BigInt(Number.MIN_SAFE_INTEGER)) {
    throw new MalformedPacketError(`int64 out of safe range at ${off}`);
  }
  return Number(n);
}

const expected: Record<number, number> = {
  [PacketType.ChartDelta]: Sizes.chart,
  [PacketType.DepthDelta]: Sizes.depth,
  [PacketType.TradeUpdate]: Sizes.trade,
  [PacketType.Pong]: Sizes.probe,
};

/** Decode one binary frame. Throws MalformedPacketError on any inconsistency. */
export function decode(buf: ArrayBuffer): Packet {
  if (buf.byteLength < Sizes.header) throw new MalformedPacketError(`short packet (${buf.byteLength} B)`);
  const v = new DataView(buf);
  const type = v.getUint8(0);
  const length = v.getUint16(1, true);
  const seq = v.getUint32(3, true);
  const ts = i64(v, 7);
  if (length !== buf.byteLength) throw new MalformedPacketError(`length field ${length} != ${buf.byteLength}`);
  if (expected[type] === undefined) throw new MalformedPacketError(`unknown packet type ${type}`);
  if (expected[type] !== length) throw new MalformedPacketError(`type ${type} must be ${expected[type]} B`);

  switch (type) {
    case PacketType.ChartDelta:
      return {
        kind: "chart",
        seq,
        start: ts,
        baseSeq: i64(v, 15),
        delta: [i64(v, 23), i64(v, 31), i64(v, 39), i64(v, 47), i64(v, 55)],
      };
    case PacketType.DepthDelta: {
      const bid: number[] = [];
      const ask: number[] = [];
      for (let i = 0; i < LEVELS; i++) {
        bid.push(v.getInt32(35 + i * 4, true));
        ask.push(v.getInt32(75 + i * 4, true));
      }
      return { kind: "depth", seq, ts, baseSeq: v.getUint32(15, true), bestBid: i64(v, 19), bestAsk: i64(v, 27), bid, ask };
    }
    case PacketType.TradeUpdate: {
      const ltp = i64(v, 15);
      const trades: Trade[] = [];
      for (let i = 0; i < 10; i++) {
        const off = 23 + i * 12;
        const q = v.getInt32(off + 4, true);
        trades.push({
          id: seq - i, // ids are consecutive, newest first
          price: ltp + v.getInt32(off, true),
          qty: Math.abs(q),
          side: q < 0 ? -1 : 1,
          ts: ts + v.getInt32(off + 8, true),
        });
      }
      return { kind: "trades", newestId: seq, ts, trades };
    }
    default:
      return { kind: "pong", seq, ts };
  }
}

/**
 * Decode one WebSocket binary frame. The server batches every packet that is
 * due on the same 50 ms tick into one frame; packets are self-delimiting via
 * the header length field. Decoding stops at the first malformed packet (the
 * framing after it cannot be trusted); packets before it are returned.
 */
export function decodeFrame(buf: ArrayBuffer): { packets: Packet[]; error?: string } {
  const packets: Packet[] = [];
  const v = new DataView(buf);
  let off = 0;
  while (off < buf.byteLength) {
    if (buf.byteLength - off < Sizes.header) return { packets, error: `trailing ${buf.byteLength - off} bytes` };
    const len = v.getUint16(off + 1, true);
    if (len < Sizes.header || off + len > buf.byteLength) return { packets, error: `bad packet length ${len} at ${off}` };
    try {
      packets.push(decode(buf.slice(off, off + len)));
    } catch (e) {
      if (e instanceof MalformedPacketError) return { packets, error: e.message };
      throw e;
    }
    off += len;
  }
  return { packets };
}

/** PING: header only; ts is the client's clock in microseconds, echoed by the server. */
export function encodePing(seq: number, tsMicros: number): ArrayBuffer {
  const buf = new ArrayBuffer(Sizes.probe);
  const v = new DataView(buf);
  v.setUint8(0, PacketType.Ping);
  v.setUint16(1, Sizes.probe, true);
  v.setUint32(3, seq >>> 0, true);
  v.setBigInt64(7, BigInt(Math.round(tsMicros)), true);
  return buf;
}
