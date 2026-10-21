import { test, expect } from "@playwright/test";
import { facts } from "../src/fixture";

test("pivot recorded costs and recover from an unavailable daemon", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("console", (m) => m.type() === "error" && errors.push(m.text()));
  let fail = false;
  await page.route("**/api/facts", async (route) => {
    if (fail) {
      await route.fulfill({ status: 502, body: "daemon unavailable" });
      return;
    }
    await route.fulfill({ json: facts });
  });
  await page.goto("/#from=2026-10-01&to=2026-10-31");
  const table = page.getByRole("table", { name: "Pivot table" });
  await expect(table).toContainText("2026-10-05");
  await expect(page.locator(".chart canvas")).toBeVisible();
  await page.getByRole("button", { name: "Model × effort" }).click();
  await expect(page.getByLabel("Measure")).toHaveValue("mean");
  await expect(table.getByRole("row", { name: /^high/ })).toBeVisible();
  // A click on a bar drills into its missions.
  await page.getByLabel("Series").selectOption("none");
  await page.getByLabel("X axis").selectOption("tool");
  await page.locator(".chart").scrollIntoViewIfNeeded();
  const box = (await page.locator(".chart canvas").boundingBox())!;
  await page.mouse.click(box.x + box.width * 0.3, box.y + box.height * 0.6);
  await expect(
    page.getByRole("heading", { name: /^Missions in / }),
  ).toBeVisible();
  fail = true;
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(page.getByRole("alert")).toContainText("daemon unavailable");
  fail = false;
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("button", { name: "Spend by model" }).click();
  await page.screenshot({
    path: "test-results/dashboard-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    page.getByRole("heading", { name: "Where is the money going?" }),
  ).toBeVisible();
  // The chart resizes on its next observer tick, so wait for the page to settle.
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth))
    .toBeLessThanOrEqual(390);
  await page.screenshot({
    path: "test-results/dashboard-mobile.png",
    fullPage: true,
  });
  // The only expected error is the deliberate 502 above; anything else (CSP
  // violations included) fails.
  expect(errors.filter((e) => !e.includes("502"))).toEqual([]);
});
