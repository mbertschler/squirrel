// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";

// https://astro.build/config
export default defineConfig({
  // GitHub Pages project site. If you later serve the docs from a custom
  // domain or the repository root, set `site` to that origin and `base` to "/".
  site: "https://mbertschler.github.io",
  base: "/squirrel",
  integrations: [
    starlight({
      title: "Squirrel",
      description:
        "Content-addressed backup tool for your own NAS plus cloud offsite storage. Every upload is verified; destinations are append-only.",
      social: [
        {
          icon: "github",
          label: "GitHub",
          href: "https://github.com/mbertschler/squirrel",
        },
      ],
      editLink: {
        baseUrl:
          "https://github.com/mbertschler/squirrel/edit/main/docs/",
      },
      lastUpdated: true,
      tableOfContents: { minHeadingLevel: 2, maxHeadingLevel: 3 },
      sidebar: [
        {
          label: "Start Here",
          items: [
            { label: "Introduction", link: "/" },
            { label: "The core principle", link: "/start/principle/" },
            { label: "Installation", link: "/start/install/" },
            { label: "Quickstart", link: "/start/quickstart/" },
          ],
        },
        {
          label: "Configuration",
          items: [
            { label: "Config file & volumes", link: "/configuration/config-file/" },
            { label: "Destinations & secrets", link: "/configuration/destinations/" },
            { label: "Index snapshots", link: "/configuration/index-snapshots/" },
          ],
        },
        {
          label: "Destination Layouts",
          items: [
            { label: "Mirror (default)", link: "/layouts/mirror/" },
            { label: "Encrypted (crypt)", link: "/layouts/encrypted/" },
            { label: "Kopia", link: "/layouts/kopia/" },
            { label: "Content-addressed", link: "/layouts/content-addressed/" },
            { label: "Packed", link: "/layouts/packed/" },
          ],
        },
        {
          label: "Guides",
          items: [
            { label: "Indexing", link: "/guides/indexing/" },
            { label: "Syncing & first use", link: "/guides/syncing/" },
            { label: "Offsite verification", link: "/guides/verification/" },
            { label: "Offloading", link: "/guides/offloading/" },
            { label: "Restoring", link: "/guides/restore/" },
            { label: "Recovery & disaster runbooks", link: "/guides/recovery/" },
            { label: "Hooks", link: "/guides/hooks/" },
            { label: "Auditing for drift", link: "/guides/auditing/" },
            { label: "Peer sync", link: "/guides/peer-sync/" },
            { label: "The agent", link: "/guides/agent/" },
            { label: "Terminal UI", link: "/guides/tui/" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "CLI reference", link: "/reference/cli/" },
            { label: "Configuration reference", link: "/reference/configuration/" },
            { label: "On-disk layouts", link: "/reference/on-disk-layout/" },
            { label: "Manifest & pack formats", link: "/reference/formats/" },
          ],
        },
        {
          label: "Concepts",
          items: [
            { label: "Content, not paths", link: "/concepts/content-model/" },
            { label: "Runs & the audit trail", link: "/concepts/runs/" },
          ],
        },
      ],
    }),
  ],
});
