# Squirrel documentation site

The squirrel documentation, built with [Astro](https://astro.build) +
[Starlight](https://starlight.astro.build). The full README in the repository
root remains the canonical single-file reference; this site is a restructured,
navigable presentation of the same material.

## Local development

This site uses [Bun](https://bun.sh) (matching the `squirrel-desktop` frontend).

```sh
cd docs
bun install
bun run dev      # serve at http://localhost:4321/squirrel
```

## Build

```sh
bun run build    # output to docs/dist
bun run preview  # preview the production build locally
```

## Structure

- `src/content/docs/` — one Markdown/MDX file per page, grouped into
  `start/`, `configuration/`, `layouts/`, `guides/`, `reference/`, and
  `concepts/`.
- `astro.config.mjs` — site/base URL and the sidebar navigation.

## Deployment

`.github/workflows/docs.yml` builds this site and deploys it to GitHub Pages on
every push to `main` that touches `docs/**`. The site is served under the
`/squirrel` base path (`site` + `base` in `astro.config.mjs`); change both if you
move the docs to a custom domain or the repository root.

## Notes on links

Because the site is served under a base path, internal links in Markdown include
the `/squirrel/` prefix (e.g. `/squirrel/guides/syncing/`). Sidebar links in
`astro.config.mjs` omit the prefix — Starlight adds the base automatically. If
you change `base`, update the in-content links too.
