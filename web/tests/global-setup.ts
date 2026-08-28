import { existsSync } from "node:fs";

import { binary, start } from "./server";

export default async function setup() {
  if (!existsSync(binary)) {
    throw new Error(
      `the browser tests need a built binary at ${binary}; run \`make build\` (or \`make web-test\`, which does)`,
    );
  }
  await start();
}
