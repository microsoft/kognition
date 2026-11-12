// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { defineConfig } = require("@playwright/test");

module.exports = defineConfig({
  testDir: "./cmd/kognitionctl/cmd/dashboard_assets",
  testMatch: "browser.test.js",
  outputDir: "./test-results/playwright",
  fullyParallel: true,
  reporter: "line",
  use: {
    baseURL: "http://127.0.0.1:4173",
    colorScheme: "light",
    screenshot: "only-on-failure",
  },
  projects: [
    { name: "desktop", use: { viewport: { width: 1440, height: 900 } } },
    { name: "mobile", use: { viewport: { width: 390, height: 844 } } },
  ],
  webServer: {
    command: "http-server cmd/kognitionctl/cmd/dashboard_assets -p 4173 -c-1",
    url: "http://127.0.0.1:4173",
    reuseExistingServer: false,
  },
});