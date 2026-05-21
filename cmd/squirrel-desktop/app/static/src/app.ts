import "@hotwired/turbo";
import { Application } from "@hotwired/stimulus";
import "basecoat-css/all";

import NcduController from "./controllers/ncdu_controller";
import AutorefreshController from "./controllers/autorefresh_controller";

const stim = Application.start();
stim.register("ncdu", NcduController);
stim.register("autorefresh", AutorefreshController);
