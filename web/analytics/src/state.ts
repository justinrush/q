// The view lives in the URL hash so any pivot can be bookmarked or shared,
// and the back button steps through what you explored.
import {
  dimensions,
  filterDimensions,
  localDate,
  measures,
  type Bucket,
  type Dimension,
  type Measure,
  type SeriesDimension,
  type View,
} from "./pivot";

export function defaultView(today = new Date()): View {
  const from = new Date(today);
  from.setDate(from.getDate() - 29);
  return {
    x: "time",
    bucket: "week",
    series: "model",
    measure: "usd",
    chart: "stacked",
    filters: {
      from: localDate(from),
      to: localDate(today),
      undated: false,
      include: {},
    },
  };
}

const charts = ["stacked", "grouped", "line"] as const;
const buckets: Bucket[] = ["day", "week", "month"];

export function encode(v: View) {
  const p = new URLSearchParams();
  p.set("x", v.x);
  p.set("bucket", v.bucket);
  p.set("series", v.series);
  p.set("measure", v.measure);
  p.set("chart", v.chart);
  p.set("from", v.filters.from);
  p.set("to", v.filters.to);
  if (v.filters.undated) p.set("undated", "1");
  for (const [dim, values] of Object.entries(v.filters.include))
    for (const value of values || []) p.append(`f.${dim}`, value);
  return p.toString();
}

// Anything unrecognised falls back to the default rather than failing, so an
// old or hand-edited link still opens.
export function decode(hash: string, today = new Date()): View {
  const v = defaultView(today);
  const p = new URLSearchParams(hash.replace(/^#/, ""));
  const pick = <T extends string>(
    key: string,
    allowed: readonly T[],
    fallback: T,
  ) => {
    const got = p.get(key) as T | null;
    return got !== null && allowed.includes(got) ? got : fallback;
  };
  v.x = pick("x", Object.keys(dimensions) as Dimension[], v.x);
  v.series = pick(
    "series",
    ["none", ...filterDimensions] as SeriesDimension[],
    v.series,
  );
  v.measure = pick("measure", Object.keys(measures) as Measure[], v.measure);
  v.bucket = pick("bucket", buckets, v.bucket);
  v.chart = pick("chart", charts, v.chart);
  if (p.has("from")) v.filters.from = p.get("from") || "";
  if (p.has("to")) v.filters.to = p.get("to") || "";
  v.filters.undated = p.get("undated") === "1";
  for (const dim of filterDimensions) {
    const values = p.getAll(`f.${dim}`);
    if (values.length) v.filters.include[dim] = values;
  }
  return v;
}

type Preset = { label: string; question: string; apply: (v: View) => View };
const closed = (v: View) => ({ ...v.filters.include, status: ["closed"] });
// Spend counts everything consumed so far, ongoing missions included.
const spend = (v: View, series: View["series"]): View => ({
  ...v,
  x: "time",
  series,
  measure: "usd",
  chart: "stacked",
  filters: {
    ...v.filters,
    include: { ...v.filters.include, status: undefined },
  },
});

// Each preset answers one of the questions the explorer exists for, keeping
// the chosen date range.
export const presets: Preset[] = [
  {
    label: "Spend by model",
    question: "What is my overall spend by model?",
    apply: (v) => spend(v, "model"),
  },
  {
    label: "Spend by agent",
    question: "What is my overall spend by tool?",
    apply: (v) => spend(v, "tool"),
  },
  {
    label: "Plan vs implement",
    question: "What is my overall spend by phase?",
    apply: (v) => spend(v, "phase"),
  },
  {
    label: "Workflow cost",
    question: "Which plan → implement pairing costs least per mission?",
    apply: (v) => ({
      ...v,
      x: "workflow",
      series: "none",
      measure: "median",
      chart: "grouped",
      filters: { ...v.filters, include: closed(v) },
    }),
  },
  {
    label: "Planner × implementer",
    question: "Does cheaper planning pay for itself in implementation?",
    apply: (v) => ({
      ...v,
      x: "plan",
      series: "implement",
      measure: "median",
      chart: "grouped",
      filters: { ...v.filters, include: closed(v) },
    }),
  },
  {
    label: "Model × effort",
    question: "How do models compare at each reasoning effort?",
    apply: (v) => ({
      ...v,
      x: "effort",
      series: "model",
      measure: "mean",
      chart: "grouped",
      filters: { ...v.filters, include: closed(v) },
    }),
  },
];
