// Pure pivot engine over the facts the q analytics server serves. Every view
// is computed here so changing an axis, series or filter needs no round-trip.

export type Tokens = Partial<
  Record<
    "input" | "output" | "cacheRead" | "cacheWrite5m" | "cacheWrite1h",
    number
  >
>;
export type Slice = {
  model: string;
  effort: string;
  phase: string;
  usd: number;
  unpriced: boolean;
  tokens: Tokens;
};
export type Mission = {
  id: string;
  name: string;
  operationId: string;
  operationName: string;
  tool: string;
  status: string;
  deleted: boolean;
  startedAt?: string;
  finishedAt?: string;
  configured: {
    model: string;
    effort: string;
    planModel: string;
    planEffort: string;
    planMode: boolean;
  };
  plan: string;
  implement: string;
  iterate: string;
  workflow: string;
  usd: number;
  unpriced: boolean;
  slices: Slice[];
};
export type Facts = {
  updatedAt: string;
  missions: Mission[];
  unmeasured: number;
};

// A row is one slice of one mission: the grain every dimension can describe.
export type Row = { mission: Mission; slice: Slice };

export const dimensions = {
  time: "Mission start",
  model: "Model",
  effort: "Effort",
  modelEffort: "Model / effort",
  phase: "Phase",
  tool: "Agent",
  plan: "Planned with",
  implement: "Implemented with",
  iterate: "Iterated with",
  workflow: "Plan → implement",
  operation: "Operation",
  status: "Status",
} as const;
export type Dimension = keyof typeof dimensions;
export type SeriesDimension = Exclude<Dimension, "time"> | "none";
export const filterDimensions = Object.keys(dimensions).filter(
  (d) => d !== "time",
) as Exclude<Dimension, "time">[];

export const measures = {
  usd: "Total cost",
  mean: "Cost per mission (mean)",
  median: "Cost per mission (median)",
  missions: "Missions",
  tokens: "Tokens",
} as const;
export type Measure = keyof typeof measures;
// Summing means or medians across series says nothing, so they never stack.
export const additive = (m: Measure) => m !== "mean" && m !== "median";

export type Bucket = "day" | "week" | "month";
export type Filters = {
  from: string; // local YYYY-MM-DD, inclusive; empty means unbounded
  to: string; // local YYYY-MM-DD, inclusive
  undated: boolean;
  include: Partial<Record<Exclude<Dimension, "time">, string[]>>;
};
export type View = {
  x: Dimension;
  bucket: Bucket;
  series: SeriesDimension;
  measure: Measure;
  chart: "stacked" | "grouped" | "line";
  filters: Filters;
};

const unknown = (s: string | undefined) => s || "unknown";
const pad = (n: number) => String(n).padStart(2, "0");
export const localDate = (d: Date) =>
  `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;

// Weeks start on Monday and are labelled by that Monday, so they sort as text.
export function bucketOf(iso: string | undefined, bucket: Bucket) {
  if (!iso) return "undated";
  const d = new Date(iso);
  if (bucket === "month") return `${d.getFullYear()}-${pad(d.getMonth() + 1)}`;
  if (bucket === "week") d.setDate(d.getDate() - ((d.getDay() + 6) % 7));
  return localDate(d);
}

export function value(row: Row, dim: Dimension, bucket: Bucket = "week") {
  const { mission: m, slice: s } = row;
  switch (dim) {
    case "time":
      return bucketOf(m.startedAt, bucket);
    case "model":
      return unknown(s.model);
    case "effort":
      return unknown(s.effort);
    case "modelEffort":
      return `${unknown(s.model)} / ${unknown(s.effort)}`;
    case "phase":
      return unknown(s.phase);
    case "tool":
      return unknown(m.tool);
    case "plan":
      return m.plan;
    case "implement":
      return m.implement;
    case "iterate":
      return m.iterate;
    case "workflow":
      return m.workflow;
    case "operation":
      return m.operationName || m.operationId || "unknown";
    case "status":
      return unknown(m.status);
  }
}

export function flatten(missions: Mission[]): Row[] {
  return missions.flatMap((mission) =>
    mission.slices.map((slice) => ({ mission, slice })),
  );
}

export function applyFilters(rows: Row[], f: Filters) {
  return rows.filter((r) => {
    const day = r.mission.startedAt
      ? localDate(new Date(r.mission.startedAt))
      : "";
    if (!day) {
      if (!f.undated) return false;
    } else if ((f.from && day < f.from) || (f.to && day > f.to)) return false;
    for (const [dim, allowed] of Object.entries(f.include)) {
      if (allowed?.length && !allowed.includes(value(r, dim as Dimension)))
        return false;
    }
    return true;
  });
}

// Facets come from every row, so choosing a filter never hides its siblings.
export function facets(rows: Row[]) {
  const out = {} as Record<Exclude<Dimension, "time">, string[]>;
  for (const dim of filterDimensions) {
    const totals = new Map<string, number>();
    for (const r of rows) {
      const v = value(r, dim);
      totals.set(v, (totals.get(v) || 0) + r.slice.usd);
    }
    out[dim] = order(dim, totals);
  }
  return out;
}

const natural: Partial<Record<Dimension, string[]>> = {
  effort: ["none", "minimal", "low", "medium", "high", "xhigh", "max"],
  phase: ["plan", "implement", "iterate"],
};
function order(dim: Dimension, totals: Map<string, number>) {
  const keys = [...totals.keys()];
  if (dim === "time") return keys.sort();
  const rank = natural[dim];
  return keys.sort((a, b) => {
    if (rank) {
      const ia = rank.indexOf(a),
        ib = rank.indexOf(b);
      if (ia !== ib)
        return (ia < 0 ? rank.length : ia) - (ib < 0 ? rank.length : ib);
    }
    return (totals.get(b) || 0) - (totals.get(a) || 0) || a.localeCompare(b);
  });
}

export type Cell = {
  value: number;
  usd: number;
  missions: number;
  unpriced: boolean;
};

// A per-mission measure sums the matching slices of each mission first, so a
// mean is "what a mission spent on this" rather than "what a slice cost".
export function aggregate(rows: Row[], measure: Measure): Cell {
  const perMission = new Map<string, number>();
  let usd = 0,
    tokens = 0,
    unpriced = false;
  for (const r of rows) {
    usd += r.slice.usd;
    unpriced ||= r.slice.unpriced;
    for (const t of Object.values(r.slice.tokens)) tokens += t || 0;
    perMission.set(
      r.mission.id,
      (perMission.get(r.mission.id) || 0) + r.slice.usd,
    );
  }
  const n = perMission.size;
  const samples = [...perMission.values()].sort((a, b) => a - b);
  const value = {
    usd,
    tokens,
    missions: n,
    mean: n ? usd / n : 0,
    median: !n
      ? 0
      : n % 2
        ? samples[(n - 1) / 2]
        : (samples[n / 2 - 1] + samples[n / 2]) / 2,
  }[measure];
  return { value, usd, missions: n, unpriced };
}

export const OTHER = "Other";
export const MAX_SERIES = 7;

export type Pivot = {
  xKeys: string[];
  seriesKeys: string[];
  cells: Record<string, Record<string, Cell>>;
  xTotals: Record<string, Cell>;
  seriesTotals: Record<string, Cell>;
  total: Cell;
};

// Keys rank by spend; anything beyond the top seven folds into Other so no
// series ever needs a generated eighth hue.
export function pivot(rows: Row[], view: View): Pivot {
  const xDim = view.x,
    sDim = view.series;
  const spend = (dim: Dimension) => {
    const t = new Map<string, number>();
    for (const r of rows) {
      const v = value(r, dim, view.bucket);
      t.set(v, (t.get(v) || 0) + r.slice.usd);
    }
    return order(dim, t);
  };
  const xKeys = spend(xDim);
  let seriesKeys = sDim === "none" ? ["All"] : spend(sDim);
  const kept = new Set(seriesKeys.slice(0, MAX_SERIES));
  if (seriesKeys.length > MAX_SERIES)
    seriesKeys = [...seriesKeys.slice(0, MAX_SERIES), OTHER];
  const seriesOf = (r: Row) => {
    if (sDim === "none") return "All";
    const v = value(r, sDim);
    return kept.has(v) ? v : OTHER;
  };
  const groups = new Map<string, Row[]>();
  const push = (key: string, r: Row) => {
    const g = groups.get(key);
    if (g) g.push(r);
    else groups.set(key, [r]);
  };
  for (const r of rows) {
    const x = value(r, xDim, view.bucket),
      s = seriesOf(r);
    push(`${x}\u0000${s}`, r);
    push(`x\u0000${x}`, r);
    push(`s\u0000${s}`, r);
  }
  const cells: Pivot["cells"] = {};
  const xTotals: Pivot["xTotals"] = {};
  const seriesTotals: Pivot["seriesTotals"] = {};
  for (const x of xKeys) {
    cells[x] = {};
    for (const s of seriesKeys) {
      const g = groups.get(`${x}\u0000${s}`);
      if (g) cells[x][s] = aggregate(g, view.measure);
    }
    xTotals[x] = aggregate(groups.get(`x\u0000${x}`) || [], view.measure);
  }
  for (const s of seriesKeys)
    seriesTotals[s] = aggregate(groups.get(`s\u0000${s}`) || [], view.measure);
  return {
    xKeys,
    seriesKeys,
    cells,
    xTotals,
    seriesTotals,
    total: aggregate(rows, view.measure),
  };
}

// The rows behind a cell; null x or series means "all of them".
export function cellRows(
  rows: Row[],
  view: View,
  p: Pivot,
  x: string | null,
  series: string | null,
) {
  const kept = new Set(p.seriesKeys.filter((s) => s !== OTHER));
  return rows.filter((r) => {
    if (x !== null && value(r, view.x, view.bucket) !== x) return false;
    if (series === null || view.series === "none") return true;
    const v = value(r, view.series);
    return series === OTHER ? !kept.has(v) : v === series;
  });
}

export const SLOTS = 7;
// A colour follows its entity: keys that rank in the unfiltered data keep that
// slot, and others take the slots left free, so filtering never repaints.
export function colorSlots(globalOrder: string[], visible: string[]) {
  const slots: Record<string, number> = {};
  const used = new Set<number>();
  globalOrder = globalOrder.filter((k) => k !== "unknown");
  visible = visible.filter((k) => k !== "unknown");
  for (const key of visible) {
    const i = globalOrder.indexOf(key);
    if (i >= 0 && i < SLOTS) {
      slots[key] = i;
      used.add(i);
    }
  }
  let next = 0;
  for (const key of visible) {
    if (key === OTHER || key in slots) continue;
    while (used.has(next)) next++;
    if (next >= SLOTS) break;
    slots[key] = next;
    used.add(next);
  }
  return slots;
}

export function toCSV(p: Pivot, view: View) {
  const quote = (s: string) =>
    /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  const head = [dimensions[view.x], ...p.seriesKeys, "Total"];
  const lines = [head.map(quote).join(",")];
  for (const x of p.xKeys)
    lines.push(
      [
        quote(x),
        ...p.seriesKeys.map((s) => String(p.cells[x][s]?.value ?? "")),
        String(p.xTotals[x].value),
      ].join(","),
    );
  return lines.join("\n");
}
