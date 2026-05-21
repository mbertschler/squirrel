import { Controller } from "@hotwired/stimulus";

// MenubarRefreshController polls a partial URL on an interval and
// swaps the response into the element with id="menubar-frame". Unlike
// the SSE controller this is a plain unconditional poll: the menubar
// panel has no terminal "done" state, it just reflects current live
// data (volume counts, in-flight runs) for as long as it is mounted.
//
// Lives in its own controller (rather than reusing sse_controller)
// because EventSource is overkill for a 2-3s poll, and the menubar
// view doesn't already have a server-push channel feeding it.
export default class extends Controller<HTMLElement> {
  static values = {
    url: String,
    interval: { type: Number, default: 2500 },
  };
  declare readonly urlValue: string;
  declare readonly intervalValue: number;

  private timer: number | null = null;

  connect() {
    if (!this.urlValue) return;
    this.timer = window.setInterval(() => this.refresh(), this.intervalValue);
  }

  disconnect() {
    if (this.timer !== null) {
      window.clearInterval(this.timer);
      this.timer = null;
    }
  }

  private async refresh() {
    // Fire-and-forget: a failed poll (network blip, server restart)
    // simply leaves the previous frame visible until the next tick.
    try {
      const res = await fetch(this.urlValue, {
        headers: { Accept: "text/html" },
        cache: "no-store",
      });
      if (!res.ok) return;
      const html = await res.text();
      const target = document.getElementById("menubar-frame");
      if (!target) return;
      // outerHTML replace keeps the wrapping element id stable so
      // subsequent refreshes target the right node.
      target.outerHTML = html;
    } catch {
      // Swallow: the next interval tick will retry.
    }
  }
}
