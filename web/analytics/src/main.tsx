import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";

export type Group = {
  label: string;
  missions: number;
  usd: number;
  mean: number;
  median: number;
  planUSD: number;
  implementUSD: number;
  unknownUSD: number;
  planningShare: number | null;
  incomplete: number;
  tokens: Record<string, number>;
};
type Slice = {
  model: string;
  effort: string;
  phase: string;
  usd: number;
  unpriced: boolean;
  tokens: Record<string, number>;
};
type Detail = {
  record: {
    id: string;
    name: string;
    operationName: string;
    tool: string;
    status: string;
    deleted: boolean;
    startedAt?: string;
    model?: string;
    effort?: string;
    planModel?: string;
    planEffort?: string;
  };
  usage: { usd?: number; unpriced?: boolean; at?: string };
  breakdown: Slice[] | null;
  workflow: string;
};
type Result = {
  updatedAt: string;
  matched: number;
  measured: number;
  unknownEffort: number;
  unpriced: number;
  totalUSD: number;
  workflows: Group[];
  breakdown: Group[];
  missions: Detail[];
  page: number;
  pages: number;
  facets: Record<string, string[]>;
  operations: Record<string, string>;
};
const dollars = (v = 0) =>
  new Intl.NumberFormat("en-US", { style: "currency", currency: "USD" }).format(
    v,
  );
export function dateBoundary(value: string, end = false) {
  const d = new Date(`${value}T00:00:00`);
  if (end) d.setDate(d.getDate() + 1);
  return d.toISOString();
}
export function Bars({ group, max }: { group: Group; max: number }) {
  return (
    <div
      className="bar-track"
      aria-label={`Planning ${dollars(group.planUSD)}, implementation ${dollars(group.implementUSD)}, unknown ${dollars(group.unknownUSD)}`}
    >
      <div
        className="bar"
        style={{ width: `${max ? (group.usd / max) * 100 : 0}%` }}
      >
        {[
          ["plan", group.planUSD],
          ["implement", group.implementUSD],
          ["unknown", group.unknownUSD],
        ].map(([key, value]) => (
          <span
            key={key}
            className={String(key)}
            style={{
              width: `${group.usd ? (Number(value) / group.usd) * 100 : 0}%`,
            }}
          />
        ))}
      </div>
    </div>
  );
}
export function GroupTable({
  rows,
  workflow,
  groupBy = "",
}: {
  rows: Group[];
  workflow: boolean;
  groupBy?: string;
}) {
  const [sort, setSort] = useState<"usd" | "missions" | "mean" | "median">(
    "usd",
  );
  const sorted = [...rows].sort(
    (a, b) => b[sort] - a[sort] || a.label.localeCompare(b.label),
  );
  const max = Math.max(0, ...rows.map((r) => r.usd));
  return (
    <>
      <div className="table-tools">
        <span>{rows.length} groups</span>
        <label>
          Sort by{" "}
          <select
            value={sort}
            onChange={(e) => setSort(e.target.value as typeof sort)}
          >
            <option value="usd">Total cost</option>
            <option value="missions">Mission count</option>
            {workflow && (
              <>
                <option value="mean">Mean cost</option>
                <option value="median">Median cost</option>
              </>
            )}
          </select>
        </label>
      </div>
      <div className="scroll">
        <table>
          <thead>
            <tr>
              <th>
                {workflow
                  ? "Observed planning → implementation"
                  : `Observed ${groupBy ? groupBy.replace("-", " / ") : "model / effort / phase"}`}
              </th>
              <th>Missions</th>
              <th>Recorded cost</th>
              {workflow ? (
                <>
                  <th>Mean</th>
                  <th>Median</th>
                  <th>Planning share</th>
                </>
              ) : (
                <th>Tokens</th>
              )}
            </tr>
          </thead>
          <tbody>
            {sorted.map((g) => (
              <tr key={g.label}>
                <td className="group-label">
                  {g.label}
                  <Bars group={g} max={max} />
                </td>
                <td>{g.missions}</td>
                <td>
                  {dollars(g.usd)}
                  {g.incomplete > 0 && (
                    <span className="badge">incomplete</span>
                  )}
                </td>
                {workflow ? (
                  <>
                    <td>{dollars(g.mean)}</td>
                    <td>{dollars(g.median)}</td>
                    <td>
                      {g.planningShare === null
                        ? "—"
                        : `${(g.planningShare * 100).toFixed(1)}%`}
                    </td>
                  </>
                ) : (
                  <td>
                    {Object.values(g.tokens)
                      .reduce((a, b) => a + b, 0)
                      .toLocaleString()}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {rows.length === 0 && (
        <div className="empty">
          No measured missions match these filters. Try a wider date range or
          include ongoing missions.
        </div>
      )}
    </>
  );
}
function Mission({ detail }: { detail: Detail }) {
  const r = detail.record;
  return (
    <details className="mission">
      <summary>
        <span>
          <strong>{r.name}</strong>
          <small>
            {r.operationName} · {r.tool} · {r.status}
            {r.deleted ? " · deleted" : ""}
          </small>
        </span>
        <span>
          {dollars(detail.usage.usd)}
          {detail.usage.unpriced ? "+" : ""}
        </span>
      </summary>
      <p className="muted">Observed: {detail.workflow}</p>
      <p className="muted">
        Configured plan: {r.planModel || r.model || "agent default"} /{" "}
        {r.planModel ? r.planEffort || "default" : r.effort || "default"} ·
        Implement: {r.model || "agent default"} / {r.effort || "default"}
      </p>
      <p className="muted">
        Started{" "}
        {r.startedAt ? new Date(r.startedAt).toLocaleString() : "unknown"} ·
        Last measurement{" "}
        {detail.usage.at
          ? new Date(detail.usage.at).toLocaleString()
          : "unknown"}
      </p>
      <div className="scroll">
        <table>
          <thead>
            <tr>
              <th>Model</th>
              <th>Effort</th>
              <th>Phase</th>
              <th>Cost</th>
              <th>Fresh input</th>
              <th>Output</th>
              <th>Cache read</th>
              <th>Cache write 5m / 1h</th>
            </tr>
          </thead>
          <tbody>
            {detail.breakdown?.map((b, i) => (
              <tr key={i}>
                <td>{b.model || "unknown"}</td>
                <td>{b.effort || "unknown"}</td>
                <td>{b.phase || "unknown"}</td>
                <td>
                  {dollars(b.usd)}
                  {b.unpriced ? "+" : ""}
                </td>
                <td>{(b.tokens.input || 0).toLocaleString()}</td>
                <td>{(b.tokens.output || 0).toLocaleString()}</td>
                <td>{(b.tokens.cacheRead || 0).toLocaleString()}</td>
                <td>
                  {(b.tokens.cacheWrite5m || 0).toLocaleString()} /{" "}
                  {(b.tokens.cacheWrite1h || 0).toLocaleString()}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {!detail.breakdown?.length && (
        <p>No usage measured. This is not a zero-cost measurement.</p>
      )}
    </details>
  );
}
export function App() {
  const [filters, setFilters] = useState<Record<string, string>>({
    from: "",
    to: "",
    status: "",
    deleted: "",
    operation: "",
    tool: "",
    model: "",
    effort: "",
    phase: "",
    groupBy: "",
    undated: "false",
  });
  const [page, setPage] = useState(1),
    [tab, setTab] = useState("workflows"),
    [data, setData] = useState<Result | null>(null),
    [error, setError] = useState(""),
    [refresh, setRefresh] = useState(0),
    [checked, setChecked] = useState("");
  const update = (key: string, value: string) => {
    setFilters((f) => ({ ...f, [key]: value }));
    setPage(1);
  };
  useEffect(() => {
    const controller = new AbortController();
    let busy = false;
    const load = async () => {
      if (busy) return;
      busy = true;
      try {
        const params = new URLSearchParams();
        for (const [k, v] of Object.entries(filters)) {
          if (!v) continue;
          params.set(
            k,
            k === "from"
              ? dateBoundary(v)
              : k === "to"
                ? dateBoundary(v, true)
                : v,
          );
        }
        params.set("page", String(page));
        const response = await fetch("/api/query?" + params, {
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
  }, [filters, page, refresh]);
  const select = (key: string, title: string, options: [string, string][]) => (
    <label key={key}>
      {title}
      <select
        value={filters[key]}
        onChange={(e) => update(key, e.target.value)}
      >
        <option value="">
          {key === "groupBy" ? "Model / effort / phase" : "All"}
        </option>
        {options.map(([v, l]) => (
          <option key={v} value={v}>
            {l}
          </option>
        ))}
      </select>
    </label>
  );
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
        <h1>What does your workflow cost?</h1>
        <p>
          Compare planning and implementation across the models and effort
          levels you actually used.
        </p>
      </section>
      <section className="filters">
        <label>
          Started from
          <input
            aria-label="Started from"
            type="date"
            value={filters.from}
            onChange={(e) => update("from", e.target.value)}
          />
        </label>
        <label>
          Through
          <input
            aria-label="Through"
            type="date"
            value={filters.to}
            onChange={(e) => update("to", e.target.value)}
          />
        </label>
        {select("status", "Mission status", [
          ["closed", "Completed (closed)"],
          ["ongoing", "Ongoing"],
          ["briefing", "Briefing"],
          ["active", "Active"],
          ["awaiting", "Awaiting"],
          ["debrief", "Debrief"],
        ])}
        {select(
          "operation",
          "Operation",
          Object.entries(data?.operations || {}),
        )}
        {select(
          "tool",
          "Agent",
          (data?.facets.tool || []).map((v) => [v, v]),
        )}
        {select(
          "model",
          "Observed model",
          (data?.facets.model || []).map((v) => [v, v]),
        )}
        {select(
          "effort",
          "Observed effort",
          (data?.facets.effort || []).map((v) => [v, v]),
        )}
        {select("phase", "Phase", [
          ["plan", "Plan"],
          ["implement", "Implement"],
          ["unknown", "Unknown"],
        ])}
        {select("groupBy", "Breakdown grouping", [
          ["model", "Model"],
          ["effort", "Effort"],
          ["phase", "Phase"],
          ["model-effort", "Model / effort"],
        ])}
        {select("deleted", "Retention", [
          ["false", "Existing missions"],
          ["true", "Deleted missions"],
        ])}
        <label className="check">
          <input
            type="checkbox"
            checked={filters.undated === "true"}
            onChange={(e) => update("undated", String(e.target.checked))}
          />
          Include undated
        </label>
      </section>
      {error && (
        <div role="alert" className="error">
          {error}{" "}
          {data
            ? "Showing the last successful result; it may reflect earlier filters."
            : ""}
        </div>
      )}
      {data ? (
        <>
          {data.matched === 0 && (
            <div role="status" className="notice">
              {data.facets.tool.length === 0
                ? "No mission history is stored on this host. Existing missions are imported when the updated daemon starts; missions deleted before the upgrade cannot be recovered. If this is a paired host, check that sync has completed."
                : "No dated missions match these filters. Clear the dates and status, or include undated missions."}
            </div>
          )}
          {data.matched > 0 && data.measured === 0 && (
            <div role="status" className="notice">
              Missions are present, but none has measured usage. Only metered
              agent sessions have a cost estimate.
            </div>
          )}
          <section className="metrics">
            <article>
              <span>Whole-mission cost</span>
              <strong>
                {dollars(data.totalUSD)}
                {data.unpriced ? "+" : ""}
              </strong>
              <small>Recorded API-equivalent estimate</small>
            </article>
            <article>
              <span>Matching missions</span>
              <strong>{data.matched}</strong>
              <small>{data.measured} with measured usage</small>
            </article>
            <article>
              <span>Unknown effort</span>
              <strong>{data.unknownEffort}</strong>
              <small>Missions with attribution gaps</small>
            </article>
            <article>
              <span>Unpriced usage</span>
              <strong>{data.unpriced}</strong>
              <small>Mission totals shown as a floor</small>
            </article>
          </section>
          <nav aria-label="Analytics views">
            {[
              ["workflows", "Workflow comparison"],
              ["breakdown", "Spend breakdown"],
              ["missions", "Mission details"],
            ].map(([id, label]) => (
              <button
                key={id}
                className={tab === id ? "selected" : ""}
                onClick={() => setTab(id)}
              >
                {label}
              </button>
            ))}
          </nav>
          <section className="panel">
            <div className="panel-heading">
              <div>
                <h2>
                  {tab === "workflows"
                    ? "Compare complete workflows"
                    : tab === "breakdown"
                      ? "Follow the spend"
                      : "Inspect individual missions"}
                </h2>
                <p>
                  {tab === "breakdown"
                    ? "Only contributions matching the model, effort, and phase filters are included."
                    : "Model, effort, and phase filters select missions; costs include each whole mission."}
                </p>
              </div>
              <div className="legend">
                <span className="plan" /> Plan <span className="implement" />{" "}
                Implement <span className="unknown" /> Unknown
              </div>
            </div>
            {tab === "missions" ? (
              <>
                {data.missions.map((m) => (
                  <Mission key={m.record.id} detail={m} />
                ))}
                {!data.missions.length && (
                  <div className="empty">No matching missions.</div>
                )}
                <div className="pagination">
                  <button
                    disabled={page <= 1}
                    onClick={() => setPage((p) => p - 1)}
                  >
                    ← Previous
                  </button>
                  <span>
                    Page {page} of {data.pages}
                  </span>
                  <button
                    disabled={page >= data.pages}
                    onClick={() => setPage((p) => p + 1)}
                  >
                    Next →
                  </button>
                </div>
              </>
            ) : (
              <GroupTable
                key={tab}
                rows={tab === "workflows" ? data.workflows : data.breakdown}
                workflow={tab === "workflows"}
                groupBy={filters.groupBy}
              />
            )}
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
