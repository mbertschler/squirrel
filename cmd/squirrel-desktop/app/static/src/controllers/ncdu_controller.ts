import { Controller } from "@hotwired/stimulus";

// NcduController gives the directory listing ncdu-style keyboard nav:
// j/↓ next, k/↑ prev, Enter/→ descend, ←/Backspace ascend. Each row is a
// data-ncdu-target="row" anchor; the controller focuses the link so the
// webview's default Enter handler navigates as usual.
export default class extends Controller<HTMLElement> {
  static targets = ["row", "up"];
  declare readonly rowTargets: HTMLAnchorElement[];
  declare readonly hasUpTarget: boolean;
  declare readonly upTarget: HTMLAnchorElement;

  private index = 0;

  connect() {
    if (this.rowTargets.length > 0) {
      this.focus(0);
    }
  }

  handleKey(e: KeyboardEvent) {
    if (e.target instanceof HTMLInputElement) return;
    switch (e.key) {
      case "j":
      case "ArrowDown":
        e.preventDefault();
        this.focus(this.index + 1);
        break;
      case "k":
      case "ArrowUp":
        e.preventDefault();
        this.focus(this.index - 1);
        break;
      case "h":
      case "ArrowLeft":
      case "Backspace":
        if (this.hasUpTarget) {
          e.preventDefault();
          this.upTarget.click();
        }
        break;
      case "Enter":
      case "l":
      case "ArrowRight": {
        const current = this.rowTargets[this.index];
        if (current) {
          e.preventDefault();
          current.click();
        }
        break;
      }
    }
  }

  private focus(i: number) {
    const n = this.rowTargets.length;
    if (n === 0) return;
    this.index = ((i % n) + n) % n;
    this.rowTargets[this.index].focus();
  }
}
