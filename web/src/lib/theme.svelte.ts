/**
 * The theme the interface is drawn in.
 */

/** Theme is what a person can choose. */
export type Theme = "dark" | "light" | "system";

/** Resolved is what ends up on screen once `system` has been asked. */
export type Resolved = "dark" | "light";

const storageKey = "weg:theme";

function read(): Theme {
  try {
    const v = localStorage.getItem(storageKey);
    if (v === "dark" || v === "light") return v;
  } catch {
    // Storage can be denied; the system theme is a fine answer.
  }
  return "system";
}

const light = typeof window === "undefined" ? null : window.matchMedia("(prefers-color-scheme: light)");

let choice = $state<Theme>(typeof window === "undefined" ? "system" : read());
let systemIsLight = $state(light?.matches ?? false);

light?.addEventListener("change", (e) => {
  systemIsLight = e.matches;
});

/** theme is the single place the interface's appearance is decided. */
export const theme = {
  /** What was chosen, including `system`. */
  get choice(): Theme {
    return choice;
  },

  /** What is on screen right now. */
  get resolved(): Resolved {
    if (choice !== "system") return choice;
    return systemIsLight ? "light" : "dark";
  },

  /** set records the choice and applies it. */
  set(next: Theme) {
    choice = next;
    const root = document.documentElement;
    try {
      if (next === "system") {
        delete root.dataset.theme;
        localStorage.removeItem(storageKey);
      } else {
        root.dataset.theme = next;
        localStorage.setItem(storageKey, next);
      }
    } catch {
      // Applying it still worked; only remembering it did not.
    }
  },

  /**
   * toggle switches to the opposite of what is currently on screen. It never
   * lands on `system`: somebody reaching for the control wants the other one,
   * and cycling through three states to get there is a puzzle.
   */
  toggle() {
    this.set(this.resolved === "dark" ? "light" : "dark");
  },
};
