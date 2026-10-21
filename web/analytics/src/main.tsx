import React, { useEffect, useMemo, useState } from "react";
import { createRoot } from "react-dom/client";
import { Chart } from "./chart";
import { FilterBar, Layout, Presets } from "./controls";
import { dollars } from "./format";
import { Missions } from "./missions";
import {
  additive,
  applyFilters,
  cellRows,
  colorSlots,
  dimensions,
  facets as facetsOf,
  flatten,
  measures,
  pivot as pivotOf,
  type Facts,
  type View,
} from "./pivot";
import { decode, encode } from "./state";
import { PivotTable, type Selection } from "./table";
import "./style.css";

function useFacts(refresh: number) {
  const [data, setData] = useState<Facts | null>(null),
    [error, setError] = useState(""),
    [checked, setChecked] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    let busy = false;
    const load = async () => {
      if (busy) return;
      busy = true;
      try {
        const response = await fetch("/api/facts", {
          signal: controller.signal,
        });
        if (!response.ok) throw new Error(await response.text());
        const value = await response.json();
        if (!controller.signal.aborted) {
          setData(value);
          setError("");
          setChecked(new Date().toLocaleTimeString());
        }
      } catch (e) {
        if (!controller.signal.aborted)
          setError(e instanceof Error ? e.message : "Unable to load costs");
      } finally {
        busy = false;
      }
    };
    void load();
    const interval = setInterval(() => {
      if (!document.hidden) void load();
    }, 15000);
    const onVisible = () => {
      if (!document.hidden) void load();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      controller.abort();
      clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [refresh]);
  return { data, error, checked };
}

function useView() {
  const [view, setView] = useState(() => decode(location.hash));
  useEffect(() => {
    const onHash = () => setView(decode(location.hash));
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  const update = (next: View) => {
    if (!additive(next.measure) && next.chart === "stacked")
      next = { ...next, chart: "grouped" };
    if (next.series === next.x) next = { ...next, series: "none" };
    history.pushState(null, "", "#" + encode(next));
    setView(next);
  };
  return [view, update] as const;
}

const everything: Selection = { x: null, series: null };

export function App() {
  const [refresh, setRefresh] = useState(0);
  const { data, error, checked } = useFacts(refresh);
  const [view, setView] = useView();
  const [selected, setSelected] = useState<Selection>(everything);
  const change = (v: View) => {
    setSelected(everything);
    setView(v);
  };
  const all = useMemo(() => flatten(data?.missions || []), [data]);
  const facets = useMemo(() => facetsOf(all), [all]);
  const rows = useMemo(
    () => applyFilters(all, view.filters),
    [all, view.filters],
  );
  const p = useMemo(() => pivotOf(rows, view), [rows, view]);
  const slots = useMemo(
    () =>
      colorSlots(
        view.series === "none" ? [] : facets[view.series],
        p.seriesKeys,
      ),
    [facets, view.series, p.seriesKeys],
  );
  const chosen = useMemo(
    () => cellRows(rows, view, p, selected.x, selected.series),
    [rows, view, p, selected],
  );
  const missions = new Set(rows.map((r) => r.mission.id));
  const unpriced = new Set(
    rows.filter((r) => r.slice.unpriced).map((r) => r.mission.id),
  );
  const unknownEffort = new Set(
    rows.filter((r) => !r.slice.effort).map((r) => r.mission.id),
  );
  const perMission = view.measure === "mean" || view.measure === "median";
  return (
    <main>
      <header>
        <div className="brand">
          q<span>/ ANALYTICS</span>
        </div>
        <div className="live">
          <span className={error ? "dot stale" : "dot"} />
          {error
            ? "Connection interrupted"
            : checked
              ? `Refreshed ${checked}`
              : "Connecting…"}
          <button onClick={() => setRefresh((n) => n + 1)}>Refresh ↻</button>
        </div>
      </header>
      <section className="intro">
        <p className="eyebrow">UNDERSTAND YOUR AGENT SPEND</p>
        <h1>Where is the money going?</h1>
        <p>
          Pivot recorded cost by time, agent, model, effort, and phase. Start
          from a question or build your own view.
        </p>
      </section>
      <Presets view={view} onChange={change} />
      <FilterBar view={view} facets={facets} onChange={change} />
      {error && (
        <div role="alert" className="error">
          {error} {data ? "Showing the last successful result." : ""}
        </div>
      )}
      {data ? (
        <>
          <section className="metrics">
            <article>
              <span>Recorded cost</span>
              <strong>
                {dollars(p.total.usd)}
                {unpriced.size ? "+" : ""}
              </strong>
              <small>API-equivalent estimate of matching usage</small>
            </article>
            <article>
              <span>Missions</span>
              <strong>{missions.size}</strong>
              <small>{data.unmeasured} more have no measured usage</small>
            </article>
            <article>
              <span>Unknown effort</span>
              <strong>{unknownEffort.size}</strong>
              <small>Missions with attribution gaps</small>
            </article>
            <article>
              <span>Unpriced usage</span>
              <strong>{unpriced.size}</strong>
              <small>Mission totals shown as a floor</small>
            </article>
          </section>
          <section className="panel">
            <div className="panel-heading">
              <div>
                <h2>
                  {measures[view.measure]} by {dimensions[view.x].toLowerCase()}
                  {view.series === "none"
                    ? ""
                    : `, split by ${dimensions[view.series].toLowerCase()}`}
                </h2>
                <p>
                  {perMission
                    ? "Each mission's matching usage is summed, then compared across missions. Mission sizes differ, so treat small n as anecdotal."
                    : "Only usage matching the filters is counted. Click a bar to list its missions."}
                </p>
              </div>
            </div>
            <Layout view={view} onChange={change} />
            {p.xKeys.length ? (
              <>
                <Chart
                  pivot={p}
                  view={view}
                  slots={slots}
                  onSelect={(x, series) =>
                    setSelected({
                      x,
                      series: view.series === "none" ? null : series,
                    })
                  }
                />
                <PivotTable
                  pivot={p}
                  view={view}
                  slots={slots}
                  selected={selected}
                  onSelect={setSelected}
                />
              </>
            ) : (
              <div className="empty">
                No measured usage matches these filters. Try a wider date range
                or fewer filters.
              </div>
            )}
          </section>
          <section className="panel">
            <div className="panel-heading">
              <div>
                <h2>
                  Missions
                  {selected.x !== null || selected.series !== null
                    ? ` in ${[selected.x, selected.series].filter((s) => s !== null).join(" · ")}`
                    : ""}
                </h2>
                <p>
                  Ranked by spend within the selection; expand one for its usage
                  slices.
                </p>
              </div>
              {(selected.x !== null || selected.series !== null) && (
                <button onClick={() => setSelected(everything)}>
                  Show all
                </button>
              )}
            </div>
            <Missions
              key={`${selected.x}\u0000${selected.series}`}
              rows={chosen}
            />
          </section>
          <footer>
            <p>
              Dates select mission starts in your local timezone, not when
              tokens were consumed. Retained history includes deleted missions.
            </p>
            <p>
              Estimates use prices recorded at the last measurement, not your
              subscription bill. Unknown attribution stays unknown. Different
              tasks and small samples limit comparisons; lower cost does not
              establish equal quality.
            </p>
            <p>
              State updated {new Date(data.updatedAt).toLocaleString()} ·
              Refreshes every 15 seconds while visible.
            </p>
          </footer>
        </>
      ) : (
        !error && <div className="empty">Loading recorded costs…</div>
      )}
    </main>
  );
}
const root = document.getElementById("root");
if (root) createRoot(root).render(<App />);
