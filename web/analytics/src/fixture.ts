import type { Facts, Mission, Slice } from "./pivot";

export const slice = (
  model: string,
  effort: string,
  phase: string,
  usd: number,
  unpriced = false,
): Slice => ({
  model,
  effort,
  phase,
  usd,
  unpriced,
  tokens: { input: 100, output: 10 },
});

export function mission(
  id: string,
  startedAt: string | undefined,
  slices: Slice[],
  over: Partial<Mission> = {},
): Mission {
  const label = (p: string) => {
    const s = new Set(
      slices
        .filter((x) => x.phase === p)
        .map((x) => `${x.model} / ${x.effort}`),
    );
    return s.size === 0 ? "none" : s.size > 1 ? "mixed" : [...s][0];
  };
  const plan = label("plan"),
    implement = label("implement"),
    iterate = label("iterate");
  return {
    id,
    name: `Mission ${id}`,
    operationId: "op",
    operationName: "Operation",
    tool: "claude",
    status: "closed",
    deleted: false,
    startedAt,
    configured: {
      model: "",
      effort: "",
      planModel: "",
      planEffort: "",
      planMode: false,
    },
    plan,
    implement,
    iterate,
    workflow: `${plan} → ${implement}`,
    usd: slices.reduce((a, s) => a + s.usd, 0),
    unpriced: slices.some((s) => s.unpriced),
    slices,
    ...over,
  };
}

// Local-noon timestamps keep day bucketing independent of the test timezone.
const day = (d: string) => new Date(`${d}T12:00:00`).toISOString();

export const facts: Facts = {
  updatedAt: "2026-10-20T12:00:00Z",
  unmeasured: 1,
  missions: [
    mission("a", day("2026-10-05"), [
      slice("astra", "medium", "plan", 1),
      slice("astra", "low", "implement", 3),
    ]),
    mission("b", day("2026-10-06"), [slice("astra", "low", "implement", 5)], {
      tool: "codex",
    }),
    mission("c", day("2026-10-14"), [
      slice("fable", "high", "plan", 2),
      slice("opus", "high", "implement", 10),
    ]),
    mission("d", undefined, [slice("opus", "", "implement", 4, true)]),
  ],
};
