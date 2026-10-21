// @vitest-environment jsdom
import React from "react";
import { afterEach, describe, it, expect, vi } from "vitest";
import {
  cleanup,
  render,
  screen,
  fireEvent,
  waitFor,
  within,
} from "@testing-library/react";
import { facts } from "./fixture";

// Canvas rendering is exercised in the browser test; here the chart only needs
// to report clicks the way ECharts would.
vi.mock("./chart", () => ({
  palette: [],
  colorOf: () => "#000",
  Chart: ({ onSelect }: { onSelect: (x: string, s: string) => void }) => (
    <button onClick={() => onSelect("plan", "opus")}>chart bar</button>
  ),
}));
const { App } = await import("./main");

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  history.replaceState(null, "", "#");
});

const serve = () =>
  vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue({ ok: true, json: async () => facts } as Response);

describe("analytics", () => {
  it("pivots, applies presets and drills into a cell", async () => {
    history.replaceState(null, "", "#from=&to=&undated=1");
    serve();
    render(<App />);
    await screen.findByText("$25.00+", { selector: "strong" });
    fireEvent.change(screen.getByLabelText("X axis"), {
      target: { value: "phase" },
    });
    fireEvent.change(screen.getByLabelText("Series"), {
      target: { value: "model" },
    });
    expect(location.hash).toContain("x=phase");
    const table = screen.getByRole("table", { name: "Pivot table" });
    const plan = within(table).getByRole("row", { name: /^plan/ });
    expect(plan.textContent).toContain("$3.00");
    fireEvent.click(screen.getByText("chart bar"));
    expect(
      screen.getByRole("heading", { name: "Missions in plan · opus" }),
    ).toBeTruthy();
    expect(screen.getByText("No matching missions.")).toBeTruthy();
    fireEvent.click(within(plan).getAllByRole("button").at(-1)!);
    expect(screen.getByText("Mission c")).toBeTruthy();
    expect(screen.queryByText("Mission b")).toBeNull();
    fireEvent.click(screen.getByText("Model × effort"));
    expect((screen.getByLabelText("Measure") as HTMLSelectElement).value).toBe(
      "mean",
    );
    expect(location.hash).toContain("f.status=closed");
  });

  it("retains the last result when the daemon goes away", async () => {
    history.replaceState(null, "", "#from=&to=&undated=1");
    const fetchMock = serve();
    render(<App />);
    await screen.findByText("$25.00+", { selector: "strong" });
    fetchMock.mockResolvedValue({
      ok: false,
      text: async () => "daemon unavailable",
    } as Response);
    fireEvent.click(screen.getByText("Refresh ↻"));
    expect(await screen.findByRole("alert")).toBeTruthy();
    await waitFor(() =>
      expect(screen.getByText("$25.00+", { selector: "strong" })).toBeTruthy(),
    );
  });
});
