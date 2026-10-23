import React, { useState } from "react";
import { dollars } from "./format";
import type { Mission as Fact, Row } from "./pivot";

export function Mission({
  mission: m,
  matched,
}: {
  mission: Fact;
  matched: number;
}) {
  const c = m.configured;
  return (
    <details className="mission">
      <summary>
        <span>
          <strong>{m.name}</strong>
          <small>
            {m.operationName} · {m.tool} · {m.status}
            {m.deleted ? " · deleted" : ""}
          </small>
        </span>
        <span className="amounts">
          {dollars(matched)}
          <small>
            of {dollars(m.usd)}
            {m.unpriced ? "+" : ""} whole mission
          </small>
        </span>
      </summary>
      <p className="muted">
        Observed: {m.workflow} · iterate: {m.iterate}
      </p>
      <p className="muted">
        Configured plan: {c.planModel || c.model || "agent default"} /{" "}
        {c.planModel ? c.planEffort || "default" : c.effort || "default"} ·
        Implement: {c.model || "agent default"} / {c.effort || "default"}
      </p>
      <p className="muted">
        Started{" "}
        {m.startedAt ? new Date(m.startedAt).toLocaleString() : "unknown"}
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
            {m.slices.map((b, i) => (
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
    </details>
  );
}

const PAGE = 25;

// Missions behind a selection, ranked by what they spent within it.
export function Missions({ rows }: { rows: Row[] }) {
  const [shown, setShown] = useState(PAGE);
  const byMission = new Map<string, { mission: Fact; usd: number }>();
  for (const r of rows) {
    const e = byMission.get(r.mission.id);
    if (e) e.usd += r.slice.usd;
    else byMission.set(r.mission.id, { mission: r.mission, usd: r.slice.usd });
  }
  const list = [...byMission.values()].sort(
    (a, b) => b.usd - a.usd || a.mission.id.localeCompare(b.mission.id),
  );
  if (!list.length) return <div className="empty">No matching missions.</div>;
  return (
    <>
      {list.slice(0, shown).map((e) => (
        <Mission key={e.mission.id} mission={e.mission} matched={e.usd} />
      ))}
      {list.length > shown && (
        <div className="pagination">
          <button onClick={() => setShown((n) => n + PAGE)}>
            Show more ({list.length - shown} left)
          </button>
        </div>
      )}
    </>
  );
}
