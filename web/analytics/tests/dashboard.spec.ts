import { test, expect } from "@playwright/test";
test("explore recorded costs and recover from an unavailable daemon", async ({
  page,
}) => {
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
  let fail = false;
  await page.route("**/api/query?*", async (route) => {
    if (fail) {
      await route.fulfill({ status: 502, body: "daemon unavailable" });
      return;
    }
    await route.fulfill({
      json: {
        updatedAt: "2026-10-20T12:00:00Z",
        matched: 2,
        measured: 2,
        unknownEffort: 0,
        unpriced: 0,
        totalUSD: 12,
        workflows: [group],
        breakdown: [{ ...group, label: "astra / high / plan", usd: 3 }],
        missions: [],
        page: 1,
        pages: 1,
        facets: { model: ["astra"], effort: ["high"], tool: ["codex"] },
        operations: {},
      },
    });
  });
  await page.goto("/");
  await expect(page.getByText("astra / high → sol / low")).toBeVisible();
  await page.getByLabel("Observed model").selectOption("astra");
  await page.getByRole("button", { name: "Spend breakdown" }).click();
  await expect(page.getByText("astra / high / plan")).toBeVisible();
  fail = true;
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(page.getByRole("alert")).toContainText("daemon unavailable");
  fail = false;
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.screenshot({
    path: "test-results/dashboard-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    page.getByRole("heading", { name: "What does your workflow cost?" }),
  ).toBeVisible();
  await page.screenshot({
    path: "test-results/dashboard-mobile.png",
    fullPage: true,
  });
});
