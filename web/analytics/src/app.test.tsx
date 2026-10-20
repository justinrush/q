// @vitest-environment jsdom
import React from "react";
import { afterEach, describe, it, expect, vi } from "vitest";
import {
  cleanup,
  render,
  screen,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { App, GroupTable, dateBoundary } from "./main";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
const group = {
  label: "astra / high → sol / low",
  missions: 2,
  usd: 12,
  mean: 6,
  median: 6,
  planUSD: 3,
  implementUSD: 9,
  unknownUSD: 0,
  planningShare: 0.25,
  incomplete: 0,
  tokens: { input: 100 },
};
describe("analytics", () => {
  it("renders recorded workflow comparisons and missing planning shares", () => {
    render(
      <GroupTable
        rows={[
          group,
          { ...group, label: "unknown", planningShare: null, incomplete: 1 },
        ]}
        workflow
      />,
    );
    expect(screen.getByText("25.0%")).toBeTruthy();
    expect(screen.getByText("—")).toBeTruthy();
    expect(screen.getByText("incomplete")).toBeTruthy();
  });
  it("uses local calendar boundaries with an exclusive end", () => {
    const d = new Date(dateBoundary("2026-10-20", true));
    expect(d.getDate()).toBe(21);
    expect(d.getHours()).toBe(0);
  });
  it("filters, refreshes and retains the last result on failure", async () => {
    const payload = {
      updatedAt: "2026-10-20T12:00:00Z",
      matched: 2,
      measured: 2,
      unknownEffort: 0,
      unpriced: 0,
      totalUSD: 12,
      workflows: [group],
      breakdown: [group],
      missions: [],
      page: 1,
      pages: 1,
      facets: { model: ["astra"], effort: ["high"], tool: ["codex"] },
      operations: {},
    };
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue({ ok: true, json: async () => payload } as Response);
    render(<App />);
    await screen.findByText("astra / high → sol / low");
    expect(String(fetchMock.mock.calls[0][0])).not.toMatch(
      /(?:from|to|status)=/,
    );
    fireEvent.change(screen.getByLabelText("Mission status"), {
      target: { value: "closed" },
    });
    await waitFor(() =>
      expect(String(fetchMock.mock.calls.at(-1)?.[0])).toContain(
        "status=closed",
      ),
    );
    fireEvent.change(screen.getByLabelText("Observed model"), {
      target: { value: "astra" },
    });
    await waitFor(() =>
      expect(String(fetchMock.mock.calls.at(-1)?.[0])).toContain("model=astra"),
    );
    fetchMock.mockResolvedValue({
      ok: false,
      text: async () => "daemon unavailable",
    } as Response);
    fireEvent.click(screen.getByText("Refresh ↻"));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText("astra / high → sol / low")).toBeTruthy();
  });
  it("explains when no records are stored on the host", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue({
      ok: true,
      json: async () => ({
        updatedAt: "2026-10-20T12:00:00Z",
        matched: 0,
        measured: 0,
        unknownEffort: 0,
        unpriced: 0,
        totalUSD: 0,
        workflows: [],
        breakdown: [],
        missions: [],
        page: 1,
        pages: 1,
        facets: { model: [], effort: [], tool: [] },
        operations: {},
      }),
    } as Response);
    render(<App />);
    expect((await screen.findByRole("status")).textContent).toContain(
      "No mission history is stored on this host",
    );
  });
});
