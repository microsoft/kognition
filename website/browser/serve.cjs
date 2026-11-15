// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const path = require("node:path");
const { createServer } = require("http-server");

async function main() {
  const { site } = await import("../site.mjs");
  const server = createServer({
    root: path.resolve(__dirname, "..", "dist"),
    cache: -1,
    showDir: false,
    before: [(request, response) => {
      const url = new URL(request.url, "http://127.0.0.1:4178");
      if (!url.pathname.startsWith(site.base)) {
        response.writeHead(404);
        response.end("Outside documentation base.");
        return;
      }
      request.url = `/${url.pathname.slice(site.base.length)}${url.search}`;
      response.emit("next");
    }],
  });
  server.server.on("error", error => { console.error(error.message); process.exitCode = 1; });
  server.listen(4178, "127.0.0.1", () => console.log(`Documentation test server: http://127.0.0.1:4178${site.base}`));
}

main().catch(error => { console.error(error.message); process.exitCode = 1; });
