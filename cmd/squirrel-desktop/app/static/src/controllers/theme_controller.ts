import { Controller } from "@hotwired/stimulus";

// ThemeController owns the user-facing theme toggle. Three modes form
// a cycle: auto (follow OS prefers-color-scheme) → light → dark → auto.
// The selection is persisted under "squirrel-theme" in localStorage, the
// same key the inline boot script in layout.templ reads to avoid FOUC.
// Both code paths share the same resolution rule: explicit "light" or
// "dark" wins; anything else falls back to the OS preference.
//
// While the controller is in "auto" mode, it subscribes to the OS
// preference media query so the UI flips immediately when the host
// theme changes — once the user picks an explicit mode that listener
// is detached.
type ThemeMode = "auto" | "light" | "dark";

const STORAGE_KEY = "squirrel-theme";
const MODE_ORDER: ThemeMode[] = ["auto", "light", "dark"];

function readStoredMode(): ThemeMode {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw === "light" || raw === "dark" || raw === "auto") return raw;
  } catch {
    // localStorage can throw in private-mode webviews; fall through.
  }
  return "auto";
}

function effectiveDark(mode: ThemeMode): boolean {
  if (mode === "dark") return true;
  if (mode === "light") return false;
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

export default class extends Controller<HTMLElement> {
  static targets = ["iconAuto", "iconLight", "iconDark", "label", "button"];
  static values = { mode: String };

  declare readonly iconAutoTarget: HTMLElement;
  declare readonly iconLightTarget: HTMLElement;
  declare readonly iconDarkTarget: HTMLElement;
  declare readonly labelTarget: HTMLElement;
  declare readonly hasLabelTarget: boolean;
  declare readonly buttonTarget: HTMLButtonElement;
  declare readonly hasButtonTarget: boolean;
  declare modeValue: string;

  private media: MediaQueryList | null = null;
  private mediaListener: ((e: MediaQueryListEvent) => void) | null = null;

  connect() {
    this.modeValue = readStoredMode();
    this.apply();
  }

  disconnect() {
    this.detachMedia();
  }

  cycle() {
    const idx = MODE_ORDER.indexOf(this.modeValue as ThemeMode);
    const next = MODE_ORDER[(idx + 1) % MODE_ORDER.length];
    this.setMode(next);
  }

  setAuto() {
    this.setMode("auto");
  }

  setLight() {
    this.setMode("light");
  }

  setDark() {
    this.setMode("dark");
  }

  private setMode(mode: ThemeMode) {
    this.modeValue = mode;
    try {
      localStorage.setItem(STORAGE_KEY, mode);
    } catch {
      // Ignore — the in-memory mode still applies for this session.
    }
    this.apply();
  }

  private apply() {
    const mode = this.modeValue as ThemeMode;
    document.documentElement.classList.toggle("dark", effectiveDark(mode));
    this.syncUI(mode);
    if (mode === "auto") {
      this.attachMedia();
    } else {
      this.detachMedia();
    }
  }

  private syncUI(mode: ThemeMode) {
    this.iconAutoTarget.classList.toggle("hidden", mode !== "auto");
    this.iconAutoTarget.classList.toggle("inline-flex", mode === "auto");
    this.iconLightTarget.classList.toggle("hidden", mode !== "light");
    this.iconLightTarget.classList.toggle("inline-flex", mode === "light");
    this.iconDarkTarget.classList.toggle("hidden", mode !== "dark");
    this.iconDarkTarget.classList.toggle("inline-flex", mode === "dark");
    if (this.hasLabelTarget) {
      this.labelTarget.textContent =
        mode === "auto" ? "Auto" : mode === "light" ? "Light" : "Dark";
    }
    if (this.hasButtonTarget) {
      this.buttonTarget.setAttribute(
        "aria-label",
        `Theme: ${mode} (click to change)`,
      );
    }
  }

  private attachMedia() {
    if (this.media) return;
    this.media = window.matchMedia("(prefers-color-scheme: dark)");
    this.mediaListener = () => {
      if (this.modeValue === "auto") this.apply();
    };
    this.media.addEventListener("change", this.mediaListener);
  }

  private detachMedia() {
    if (this.media && this.mediaListener) {
      this.media.removeEventListener("change", this.mediaListener);
    }
    this.media = null;
    this.mediaListener = null;
  }
}
