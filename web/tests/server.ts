/**
 * A real weg, for the browser to talk to.
 */

import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL(".", import.meta.url));

/** binary is the weg that gets started. `make build` puts it there. */
export const binary = join(here, "..", "..", "bin", "weg");

/** handle is where the running server is described for the tests. */
const handle = join(here, ".server.json");

/** Server is what a test needs to reach the one that was started. */
export interface Server {
  /** url is the API and the interface, which share a port. */
  url: string;
  /** dns is where this server answers queries, as host:port. */
  dns: string;
  /** token is the administrator token this server minted on its first start. */
  token: string;
}

interface State extends Server {
  pid: number;
  home: string;
}

/** started reads what globalSetup left behind. */
export function started(): Server {
  return JSON.parse(readFileSync(handle, "utf8")) as State;
}

/**
 * start brings a server up and waits until it says where it is.
 */
export async function start(): Promise<void> {
  const home = mkdtempSync(join(tmpdir(), "weg-browser-"));

  const child: ChildProcess = spawn(
    binary,
    [
      "serve",
      // Ports the kernel picks, so a developer's own weg is not in the way.
      "--listen",
      "127.0.0.1:0",
      "--api-listen",
      "127.0.0.1:0",
      "--db",
      join(home, "weg.db"),
    ],
    { stdio: ["ignore", "pipe", "pipe"] },
  );

  const said: string[] = [];
  const ready = new Promise<Server>((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`the server did not report an address in ten seconds:\n${said.join("")}`)),
      10_000,
    );

    let url: string | undefined;
    let dns: string | undefined;
    let token: string | undefined;

    const read = (chunk: Buffer) => {
      const text = chunk.toString();
      said.push(text);
      const all = said.join("");
      url ??= /the API is on (http:\/\/\S+)/.exec(all)?.[1];
      dns ??= /answering on (\S+?) —/.exec(all)?.[1];
      token ??= /\b(weg_[A-Za-z0-9_-]+)/.exec(all)?.[1];
      if (url && dns && token) {
        clearTimeout(timer);
        resolve({ url, dns, token });
      }
    };

    child.stdout?.on("data", read);
    child.stderr?.on("data", read);
    child.on("exit", (code) => {
      clearTimeout(timer);
      reject(new Error(`the server exited with ${code} before it was ready:\n${said.join("")}`));
    });
  });

  const server = await ready;
  child.unref();

  const state: State = { ...server, pid: child.pid ?? 0, home };
  writeFileSync(handle, JSON.stringify(state));
}

/** stop takes the server down and removes what it wrote. */
export function stop(): void {
  let state: State;
  try {
    state = JSON.parse(readFileSync(handle, "utf8")) as State;
  } catch {
    return; // never started; nothing to clean up
  }

  try {
    process.kill(state.pid, "SIGTERM");
  } catch {
    // Already gone, which is the state this function wanted anyway.
  }
  rmSync(state.home, { recursive: true, force: true });
  rmSync(handle, { force: true });
}
