import React from "react";
import { colorOf } from "./chart";
import { format } from "./format";
import { dimensions, toCSV, type Cell, type Pivot, type View } from "./pivot";

export type Selection = { x: string | null; series: string | null };

function Value({ cell, view }: { cell?: Cell; view: View }) {
  if (!cell) return <span className="faint">—</span>;
  return (
    <>
      {format(view.measure, cell.value)}
      {cell.unpriced ? "+" : ""}
      <small className="n">n={cell.missions}</small>
    </>
  );
}

export function PivotTable({
  pivot: p,
  view,
  slots,
  selected,
  onSelect,
}: {
  pivot: Pivot;
  view: View;
  slots: Record<string, number>;
  selected: Selection;
  onSelect: (s: Selection) => void;
}) {
  const many = p.seriesKeys.length > 1;
  const cls = (x: string | null, s: string | null) =>
    selected.x === x && selected.series === s ? "cell selected" : "cell";
  const cell = (x: string | null, s: string | null, c?: Cell) => (
    <td key={s ?? "total"}>
      <button
        className={cls(x, s)}
        disabled={!c}
        onClick={() => onSelect({ x, series: s })}
      >
        <Value cell={c} view={view} />
      </button>
    </td>
  );
  return (
    <>
      <div className="table-tools">
        <span>
          {p.xKeys.length} rows · click any value to list its missions
        </span>
        <button
          onClick={() => void navigator.clipboard?.writeText(toCSV(p, view))}
        >
          Copy CSV
        </button>
      </div>
      <div className="scroll">
        <table className="pivot" aria-label="Pivot table">
          <thead>
            <tr>
              <th>{dimensions[view.x]}</th>
              {many &&
                p.seriesKeys.map((s) => (
                  <th key={s}>
                    <span
                      className="swatch"
                      style={{ background: colorOf(s, slots) }}
                    />
                    {s}
                  </th>
                ))}
              <th>{many ? "All" : "Value"}</th>
            </tr>
          </thead>
          <tbody>
            {p.xKeys.map((x) => (
              <tr key={x}>
                <th scope="row">{x}</th>
                {many && p.seriesKeys.map((s) => cell(x, s, p.cells[x][s]))}
                {cell(x, null, p.xTotals[x])}
              </tr>
            ))}
          </tbody>
          <tfoot>
            <tr>
              <th scope="row">All</th>
              {many &&
                p.seriesKeys.map((s) => cell(null, s, p.seriesTotals[s]))}
              {cell(null, null, p.total)}
            </tr>
          </tfoot>
        </table>
      </div>
    </>
  );
}
