import "@hotwired/turbo";
import { Application } from "@hotwired/stimulus";
import "basecoat-css/all";

import NcduController from "./controllers/ncdu_controller";
import SseController from "./controllers/sse_controller";
import MenubarRefreshController from "./controllers/menubar_refresh_controller";
import ThemeController from "./controllers/theme_controller";

const stim = Application.start();
stim.register("ncdu", NcduController);
stim.register("sse", SseController);
stim.register("menubar-refresh", MenubarRefreshController);
stim.register("theme", ThemeController);
