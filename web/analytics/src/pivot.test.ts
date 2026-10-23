import { describe, expect, it } from "vitest";
import { facts, mission, slice } from "./fixture";
import {
  aggregate,
  applyFilters,
  bucketOf,
  cellRows,
  colorSlots,
  facets,
  flatten,
  OTHER,
  pivot,
  toCSV,
  type View,
} from "./pivot";
import { decode, defaultView, encode, presets } from "./state";

const rows = flatten(facts.missions);
const view = (over: Partial<View> = {}): View => ({
  ...defaultView(new Date("2026-10-20T12:00:00")),
  ...over,
});
const open = { from: "", to: "", undated: true, include: {} };

describe("pivot", () => {
  it("buckets weeks by their Monday and months by calendar", () => {
    const iso = new Date("2026-10-08T12:00:00").toISOString(); // a Thursday
    expect(bucketOf(iso, "week")).toBe("2026-10-05");
    expect(bucketOf(iso, "month")).toBe("2026-10");
    expect(bucketOf(undefined, "day")).toBe("undated");
  });

  it("filters by date, undated, and any dimension", () => {
    expect(applyFilters(rows, { ...open, undated: false }).length).toBe(5);
    expect(
      applyFilters(rows, {
        ...open,
        from: "2026-10-06",
        to: "2026-10-06",
        undated: false,
      }).map((r) => r.mission.id),
    ).toEqual(["b"]);
    const plans = applyFilters(rows, { ...open, include: { phase: ["plan"] } });
    expect(plans.map((r) => r.slice.usd)).toEqual([1, 2]);
    expect(
      applyFilters(rows, {
        ...open,
        include: { tool: ["codex"], phase: ["plan"] },
      }),
    ).toEqual([]);
  });

  it("measures per mission after summing its matching slices", () => {
    const astra = rows.filter((r) => r.slice.model === "astra");
    expect(aggregate(astra, "usd").value).toBe(9);
    expect(aggregate(astra, "missions").value).toBe(2);
    expect(aggregate(astra, "mean").value).toBe(4.5); // (1+3) and 5
    expect(aggregate(rows, "median").value).toBe(4.5); // 4, 4, 5, 12
    expect(aggregate(rows, "tokens").value).toBe(6 * 110);
    expect(aggregate(rows, "usd").unpriced).toBe(true);
  });

  it("pivots time by series with consistent totals", () => {
    const p = pivot(applyFilters(rows, { ...open, undated: false }), view());
    expect(p.xKeys).toEqual(["2026-10-05", "2026-10-12"]);
    expect(p.seriesKeys).toEqual(["opus", "astra", "fable"]);
    expect(p.cells["2026-10-05"].astra.value).toBe(9);
    expect(p.cells["2026-10-05"].opus).toBeUndefined();
    expect(p.xTotals["2026-10-12"].value).toBe(12);
    expect(p.seriesTotals.opus.value).toBe(10);
    expect(p.total.value).toBe(21);
    expect(toCSV(p, view()).split("\n")[1]).toBe("2026-10-05,,9,,9");
  });

  it("orders efforts naturally and answers per-mission comparisons", () => {
    const p = pivot(
      rows,
      view({ x: "effort", series: "model", measure: "mean" }),
    );
    expect(p.xKeys).toEqual(["low", "medium", "high", "unknown"]);
    expect(p.cells.high.fable.value).toBe(2);
    expect(p.cells.low.astra.value).toBe(4);
  });

  it("folds series past the seventh into Other and drills into it", () => {
    const many = flatten(
      Array.from({ length: 9 }, (_, i) =>
        mission(`m${i}`, "2026-10-05T12:00:00Z", [
          slice(`model-${i}`, "low", "implement", 10 - i),
        ]),
      ),
    );
    const v = view({ x: "tool", filters: open });
    const p = pivot(many, v);
    expect(p.seriesKeys.length).toBe(8);
    expect(p.seriesKeys.at(-1)).toBe(OTHER);
    expect(p.cells.claude[OTHER].value).toBe(3 + 2);
    expect(
      cellRows(many, v, p, "claude", OTHER).map((r) => r.slice.model),
    ).toEqual(["model-7", "model-8"]);
  });

  it("keeps facets from unfiltered rows and colours stable under filtering", () => {
    expect(facets(rows).model).toEqual(["opus", "astra", "fable"]);
    expect(colorSlots(["opus", "astra", "fable"], ["fable"])).toEqual({
      fable: 2,
    });
    expect(colorSlots(["a"], ["z", "a", OTHER])).toEqual({ a: 0, z: 1 });
    expect(colorSlots(["unknown", "a"], ["unknown", "a"])).toEqual({ a: 0 });
  });

  it("orders phases through iteration and splits it by model", () => {
    const iterated = flatten([
      mission("i", "2026-10-07T12:00:00.000Z", [
        slice("opus", "high", "iterate", 3),
        slice("opus", "high", "implement", 10),
        slice("sonnet", "medium", "iterate", 1),
        slice("fable", "high", "plan", 2),
      ]),
    ]);
    const byPhase = pivot(iterated, view({ x: "phase", series: "none" }));
    expect(byPhase.xKeys).toEqual(["plan", "implement", "iterate"]);
    expect(iterated[0].mission.iterate).toBe("mixed");

    const v = presets.find((p) => p.label === "Iteration by model")!;
    const applied = v.apply(view());
    const p = pivot(applyFilters(iterated, applied.filters), applied);
    expect(p.seriesKeys).toEqual(["opus", "sonnet"]);
    expect(p.total.value).toBe(4);
  });

  it("round-trips views through the URL and tolerates junk", () => {
    const v = presets[4].apply(view());
    expect(decode(encode(v))).toEqual(v);
    expect(v.filters.include.status).toEqual(["closed"]);
    expect(decode("#x=bogus&measure=nope").x).toBe("time");
  });
});
