// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { defineConfig } = require("@playwright/test");

module.exports = defineConfig({
  testDir: "./cmd/kognitionctl/cmd/studio_assets",
  testMatch: "browser.test.js",
  outputDir: "./test-results/playwright-studio",
  fullyParallel: true,
  reporter: "line",
  use: {
    baseURL: "http://127.0.0.1:4174",
    colorScheme: "light",
    screenshot: "only-on-failure",
  },
  projects: [
    { name: "desktop", use: { viewport: { width: 1440, height: 900 } } },
    { name: "mobile", use: { viewport: { width: 390, height: 844 } } },
  ],
  webServer: {
    command: "http-server cmd/kognitionctl/cmd/studio_assets -p 4174 -c-1",
    url: "http://127.0.0.1:4174",
    reuseExistingServer: false,
  },
});