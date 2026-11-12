// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { defineConfig } = require("@playwright/test");
const path = require("node:path");

const profile = path.join(__dirname, "test-results", "tutorial-profile");

module.exports = defineConfig({
  testDir: "./cmd/kognitionctl/cmd/studio_assets",
  testMatch: "tutorial.browser.test.js",
  outputDir: "./test-results/playwright-tutorial",
  workers: 1,
  fullyParallel: false,
  reporter: "line",
  use: {
    baseURL: "http://127.0.0.1:4186",
    colorScheme: "light",
    screenshot: "only-on-failure",
  },
  projects: [
    { name: "desktop", use: { viewport: { width: 1440, height: 900 } } },
    { name: "mobile", use: { viewport: { width: 390, height: 844 } } },
  ],
  webServer: {
    command: `go run "${path.join(__dirname, "cmd", "kognitionctl")}" studio --tutorial --mode kubernetes --port 4186`,
    url: "http://127.0.0.1:4186/api/meta",
    reuseExistingServer: false,
    timeout: 180000,
    env: {
      APPDATA: profile,
      XDG_CONFIG_HOME: profile,
      KUBECONFIG: path.join(profile, "nonexistent-kubeconfig"),
    },
  },
});
