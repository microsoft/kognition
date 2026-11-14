// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { defineConfig } = require("@playwright/test");
const path = require("node:path");
const base = process.env.DOCS_BASE_PATH || "/kognition/";

module.exports = defineConfig({
  testDir: "./website/browser",
  testMatch: "tutorial.spec.cjs",
  outputDir: "./test-results/playwright-docs",
  workers: 2,
  reporter: "line",
  use: {
    baseURL: `http://127.0.0.1:4178${base}`,
    screenshot: "only-on-failure",
  },
  projects: [
    { name: "desktop", use: { viewport: { width: 1440, height: 900 } } },
    { name: "mobile", use: { viewport: { width: 390, height: 844 } } },
  ],
  webServer: {
    command: `node "${path.join(__dirname, "website", "browser", "serve.cjs")}"`,
    url: `http://127.0.0.1:4178${base}guides/video-tutorial/`,
    reuseExistingServer: false,
    timeout: 60000,
  },
});
