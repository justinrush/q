import type { Measure } from "./pivot";

const usd = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
});
const compactUSD = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  notation: "compact",
  maximumFractionDigits: 1,
});
const compact = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});

export const dollars = (v = 0) => usd.format(v);

export function format(measure: Measure, v: number, short = false) {
  if (measure === "missions") return String(v);
  if (measure === "tokens")
    return short ? compact.format(v) : v.toLocaleString();
  return short ? compactUSD.format(v) : usd.format(v);
}
