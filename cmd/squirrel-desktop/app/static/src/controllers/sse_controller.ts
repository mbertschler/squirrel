import { Controller } from "@hotwired/stimulus";
import * as Turbo from "@hotwired/turbo";

// SseController opens an EventSource against `data-sse-url-value` and
// hands each event payload to Turbo.renderStreamMessage so the
// server-rendered <turbo-stream> frames mutate the DOM in place.
//
// The controller is the parent of the live-mutated region: when the
// final terminal frame swaps the status panel, the controller's
// element stays mounted, so disconnect only fires on full navigation.
// We also stop on a 'done' stage marker to release the connection
// cleanly without waiting for the browser to time the channel out.
export default class extends Controller<HTMLElement> {
  static values = { url: String };
  declare readonly urlValue: string;

  private source: EventSource | null = null;

  connect() {
    if (!this.urlValue) return;
    this.source = new EventSource(this.urlValue);
    this.source.onmessage = (e) => {
      // Each event's data is one <turbo-stream> fragment (potentially
      // multi-line — the server splits on \n into multiple "data:"
      // lines and the browser concatenates them back here with \n).
      Turbo.renderStreamMessage(e.data);
    };
    this.source.onerror = () => {
      // EventSource auto-reconnects on transport errors; we only
      // intervene if the connection is actually closed, which means
      // the server hung up on a clean end-of-stream (the SSE frame
      // loop returned because the run reached a terminal status and
      // the producer goroutine closed the subscriber channel).
      if (this.source && this.source.readyState === EventSource.CLOSED) {
        this.close();
      }
    };
  }

  disconnect() {
    this.close();
  }

  private close() {
    if (this.source) {
      this.source.close();
      this.source = null;
    }
  }
}
