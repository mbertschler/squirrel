import { Controller } from "@hotwired/stimulus";

// AutorefreshController polls the parent Turbo frame on an interval
// while data-autorefresh-done-value is "false". When the value flips
// to "true" (the run reached a terminal status, etc.) the timer is
// cleared. Used by the run-detail view to surface live progress
// without an SSE channel.
export default class extends Controller<HTMLElement> {
  static values = { interval: Number, done: String };
  declare readonly intervalValue: number;
  declare readonly doneValue: string;

  private timer: ReturnType<typeof setInterval> | null = null;

  connect() {
    if (this.doneValue === "true") return;
    this.timer = setInterval(() => this.reload(), this.intervalValue || 2000);
  }

  disconnect() {
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
  }

  doneValueChanged() {
    if (this.doneValue === "true" && this.timer) {
      clearInterval(this.timer);
      this.timer = null;
    }
  }

  private reload() {
    const frame = this.element.closest("turbo-frame") as HTMLElement | null;
    if (!frame) return;
    // Turbo's TypeScript types expose .reload() on TurboFrameElement
    // but the global declaration isn't in scope here; cast through any.
    (frame as { reload?: () => void }).reload?.();
  }
}
