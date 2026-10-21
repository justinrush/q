import React from "react";
import {
  additive,
  dimensions,
  filterDimensions,
  measures,
  type Dimension,
  type View,
} from "./pivot";
import { presets } from "./state";

export function Presets({
  view,
  onChange,
}: {
  view: View;
  onChange: (v: View) => void;
}) {
  return (
    <section className="presets" aria-label="Questions">
      {presets.map((p) => (
        <button
          key={p.label}
          title={p.question}
          onClick={() => onChange(p.apply(view))}
        >
          {p.label}
        </button>
      ))}
    </section>
  );
}

export function Layout({
  view,
  onChange,
}: {
  view: View;
  onChange: (v: View) => void;
}) {
  const set = (patch: Partial<View>) => onChange({ ...view, ...patch });
  return (
    <section className="layout">
      <label>
        X axis
        <select
          value={view.x}
          onChange={(e) => set({ x: e.target.value as Dimension })}
        >
          {Object.entries(dimensions).map(([k, l]) => (
            <option key={k} value={k}>
              {l}
            </option>
          ))}
        </select>
      </label>
      {view.x === "time" && (
        <label>
          Bucket
          <select
            value={view.bucket}
            onChange={(e) => set({ bucket: e.target.value as View["bucket"] })}
          >
            <option value="day">Day</option>
            <option value="week">Week</option>
            <option value="month">Month</option>
          </select>
        </label>
      )}
      <label>
        Series
        <select
          value={view.series}
          onChange={(e) => set({ series: e.target.value as View["series"] })}
        >
          <option value="none">None</option>
          {filterDimensions
            .filter((d) => d !== view.x)
            .map((d) => (
              <option key={d} value={d}>
                {dimensions[d]}
              </option>
            ))}
        </select>
      </label>
      <label>
        Measure
        <select
          value={view.measure}
          onChange={(e) => set({ measure: e.target.value as View["measure"] })}
        >
          {Object.entries(measures).map(([k, l]) => (
            <option key={k} value={k}>
              {l}
            </option>
          ))}
        </select>
      </label>
      <label>
        Chart
        <select
          value={view.chart}
          onChange={(e) => set({ chart: e.target.value as View["chart"] })}
        >
          <option value="stacked" disabled={!additive(view.measure)}>
            Stacked bars
          </option>
          <option value="grouped">Grouped bars</option>
          <option value="line">Lines</option>
        </select>
      </label>
    </section>
  );
}

export function FilterBar({
  view,
  facets,
  onChange,
}: {
  view: View;
  facets: Record<string, string[]>;
  onChange: (v: View) => void;
}) {
  const f = view.filters;
  const setFilters = (patch: Partial<View["filters"]>) =>
    onChange({ ...view, filters: { ...f, ...patch } });
  const toggle = (dim: string, value: string, on: boolean) => {
    const current = f.include[dim as keyof typeof f.include] || [];
    const next = on ? [...current, value] : current.filter((v) => v !== value);
    setFilters({
      include: { ...f.include, [dim]: next.length ? next : undefined },
    });
  };
  return (
    <section className="filters">
      <label>
        Started from
        <input
          type="date"
          value={f.from}
          onChange={(e) => setFilters({ from: e.target.value })}
        />
      </label>
      <label>
        Through
        <input
          type="date"
          value={f.to}
          onChange={(e) => setFilters({ to: e.target.value })}
        />
      </label>
      <label className="check">
        <input
          type="checkbox"
          checked={f.undated}
          onChange={(e) => setFilters({ undated: e.target.checked })}
        />
        Include undated
      </label>
      <div className="chips">
        {filterDimensions.map((dim) => {
          const chosen = f.include[dim] || [];
          return (
            <details
              key={dim}
              className={chosen.length ? "chip active" : "chip"}
            >
              <summary>
                {dimensions[dim]}
                {chosen.length
                  ? `: ${chosen.length === 1 ? chosen[0] : `${chosen.length} selected`}`
                  : ""}
              </summary>
              <div
                className="chip-menu"
                role="group"
                aria-label={`Filter ${dimensions[dim]}`}
              >
                {(facets[dim] || []).map((v) => (
                  <label key={v} className="option">
                    <input
                      type="checkbox"
                      checked={chosen.includes(v)}
                      onChange={(e) => toggle(dim, v, e.target.checked)}
                    />
                    {v}
                  </label>
                ))}
                {chosen.length > 0 && (
                  <button
                    onClick={() =>
                      setFilters({
                        include: { ...f.include, [dim]: undefined },
                      })
                    }
                  >
                    Clear
                  </button>
                )}
              </div>
            </details>
          );
        })}
      </div>
    </section>
  );
}
