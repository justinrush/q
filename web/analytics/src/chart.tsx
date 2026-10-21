import React, { useEffect, useRef } from "react";
import * as echarts from "echarts/core";
import { BarChart, LineChart } from "echarts/charts";
import {
  GridComponent,
  LegendComponent,
  TooltipComponent,
} from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { additive, OTHER, type Pivot, type View } from "./pivot";
import { format } from "./format";

echarts.use([
  BarChart,
  LineChart,
  GridComponent,
  LegendComponent,
  TooltipComponent,
  CanvasRenderer,
]);

// Categorical slots in fixed order, validated for colour-vision separation.
export const palette = [
  "#2a78d6",
  "#eb6834",
  "#1baf7a",
  "#eda100",
  "#e87ba4",
  "#008300",
  "#6250d6",
];
export const otherColor = "#7d8a90";
// Missing attribution is not an entity, so it never takes a hue.
export const unknownColor = "#c3cacd";
export const colorOf = (key: string, slots: Record<string, number>) =>
  key === "unknown"
    ? unknownColor
    : key in slots
      ? palette[slots[key]]
      : otherColor;

const escape = (s: string) =>
  s.replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ]!,
  );

export function options(p: Pivot, view: View, slots: Record<string, number>) {
  const stack = view.chart === "stacked" && additive(view.measure);
  const line = view.chart === "line";
  return {
    animation: false,
    grid: { left: 8, right: 16, top: 40, bottom: 8, containLabel: true },
    legend: {
      show: p.seriesKeys.length > 1,
      top: 0,
      left: 0,
      icon: "roundRect",
      textStyle: { color: "#3c4a50" },
    },
    tooltip: {
      trigger: "axis",
      axisPointer: { type: line ? "line" : "shadow" },
      formatter: (
        items: {
          axisValue: string;
          seriesName: string;
          color: string;
          dataIndex: number;
        }[],
      ) => {
        const x = items[0]?.axisValue ?? "";
        const rows = items
          .map((i) => {
            const c = p.cells[x]?.[i.seriesName];
            if (!c) return "";
            return `<div class="tip-row"><span class="tip-swatch" style="background:${i.color}"></span>${escape(i.seriesName)}<b>${format(view.measure, c.value)}${c.unpriced ? "+" : ""}</b><small>n=${c.missions}</small></div>`;
          })
          .join("");
        const t = p.xTotals[x];
        return `<div class="tip"><strong>${escape(x)}</strong>${rows}${
          p.seriesKeys.length > 1 && t
            ? `<div class="tip-row tip-total">All<b>${format(view.measure, t.value)}</b><small>n=${t.missions}</small></div>`
            : ""
        }</div>`;
      },
    },
    xAxis: {
      type: "category",
      data: p.xKeys,
      axisTick: { show: false },
      axisLine: { lineStyle: { color: "#c9d0d4" } },
      axisLabel: { color: "#5d6b71", hideOverlap: true },
    },
    yAxis: {
      type: "value",
      splitLine: { lineStyle: { color: "#e6eaec" } },
      axisLabel: {
        color: "#5d6b71",
        formatter: (v: number) => format(view.measure, v, true),
      },
    },
    series: p.seriesKeys.map((s) => ({
      name: s,
      type: line ? "line" : "bar",
      stack: stack ? "total" : undefined,
      symbolSize: 8,
      lineStyle: { width: 2 },
      barMaxWidth: 36,
      itemStyle: {
        color: colorOf(s, slots),
        borderColor: "#ffffff",
        borderWidth: stack ? 1 : 0,
        borderRadius: stack ? 0 : [4, 4, 0, 0],
      },
      emphasis: { focus: "series" },
      data: p.xKeys.map((x) => p.cells[x][s]?.value ?? (line ? null : 0)),
    })),
  };
}

export function Chart({
  pivot,
  view,
  slots,
  onSelect,
}: {
  pivot: Pivot;
  view: View;
  slots: Record<string, number>;
  onSelect: (x: string, series: string | null) => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const chart = useRef<echarts.ECharts | null>(null);
  const select = useRef(onSelect);
  select.current = onSelect;
  const keys = useRef(pivot.xKeys);
  keys.current = pivot.xKeys;
  useEffect(() => {
    if (!ref.current) return;
    const c = echarts.init(ref.current);
    chart.current = c;
    // A bar selects its cell; anywhere else in a column selects the column,
    // so short bars never need a precise click.
    let onBar = false;
    c.on("click", (e) => {
      if (e.name === undefined || e.seriesName === undefined) return;
      onBar = true;
      select.current(e.name, e.seriesName);
    });
    c.getZr().on("click", (e) => {
      if (onBar) {
        onBar = false;
        return;
      }
      const point = [e.offsetX, e.offsetY];
      if (!c.containPixel("grid", point)) return;
      const [i] = c.convertFromPixel("grid", point) as number[];
      const x = keys.current[Math.round(i)];
      if (x !== undefined) select.current(x, null);
    });
    const resize = new ResizeObserver(() => c.resize());
    resize.observe(ref.current);
    return () => {
      resize.disconnect();
      c.dispose();
      chart.current = null;
    };
  }, []);
  useEffect(() => {
    chart.current?.setOption(options(pivot, view, slots), { notMerge: true });
  }, [pivot, view, slots]);
  return (
    <div
      ref={ref}
      className="chart"
      role="img"
      aria-label={`${view.measure} by ${view.x}${view.series === "none" ? "" : ` and ${view.series}`}; the table below lists every value`}
    />
  );
}
