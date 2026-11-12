# Homelab Autoscaler Documentation Website

This website is built using [Docusaurus 3](https://docusaurus.io/), a modern static website generator.

## Installation

```bash
cd website
npm install
```

## Local Development

```bash
npm start
```

This command starts a local development server and opens up a browser window. Most changes are reflected live without having to restart the server.

## Build

```bash
npm run build
```

This command generates static content into the `build` directory and can be served using any static contents hosting service.

## Deployment

### GitHub Pages

The site is configured for GitHub Pages deployment. To deploy:

```bash
npm run deploy
```

This will build the site and push it to the `gh-pages` branch.

### Manual Deployment

If you prefer to deploy manually:

1. Build the site: `npm run build`
2. Copy the contents of the `build` directory to your web server

## Configuration

### Site Configuration

The main configuration is in [`docusaurus.config.js`](./docusaurus.config.js). Key settings include:

- **Site metadata**: Title, tagline, URL, and base URL
- **GitHub Pages**: Organization name, project name, and deployment branch
- **Documentation**: Path to docs directory (../docs) and sidebar configuration
- **Theming**: Color scheme and navigation setup

### Sidebar Configuration

The documentation sidebar is configured in [`sidebars.js`](./sidebars.js). Every doc in `../docs` is
listed there — a page in the tree and not in the sidebar is a page nothing links to. The categories
follow the order an operator meets the material in:

- Getting Started (`intro.md` is the docs root: its front matter carries `slug: /`)
- Concepts
- Operations
- Reference (CRDs, examples, events, changelog, upgrade)
- Contributing (setup, CRD sync, state management, the NodeClaim design, the e2e flow)

### Versioning

The docs are not versioned: there is one set of pages, and `versions.current.label` is the label the
navbar dropdown shows. The `docs-deploy` workflow rewrites that label from `dist/chart/Chart.yaml` on
every release. Its pattern is anchored to the start of a line — `/^(\s*)label: 'v[^']*'/m`, first
match, indentation captured and put back — and the step exits non-zero when no line matches, so the
label has to stay a field on its own line and a comment quoting the shape of the field can never be
patched in its place. The checked-in label matches the current chart, `0.2.3`.

To start real versioning — a snapshot per release — run:

```bash
npm run docusaurus docs:version 0.2.3
```

That creates `website/versioned_docs/` and `website/versioned_sidebars.json`, and adds the version to
`versions.json`. Nothing does that today.

## Documentation Structure

The documentation is read from the `../docs` directory and organized as follows:

```
docs/
├── intro.md                    # slug: / — the docs root
├── getting-started/
│   ├── overview.md
│   ├── installation.md
│   └── quick-start.md
├── concepts/
│   ├── nodeclaim-model.md
│   ├── node-labels.md
│   └── pool-at-a-glance.md
├── architecture/
│   └── overview.md
├── reference/
│   └── events.md
├── api-reference/
│   ├── crds/
│   └── examples/
├── releases/
│   ├── changelog.md
│   └── upgrade-0.1-to-0.2.md
├── troubleshooting/
│   ├── common-issues.md
│   ├── debugging-guide.md
│   └── known-issues.md
└── development/
    ├── setup.md
    ├── crd-sync.md
    ├── state.md
    ├── nodeclaim.md
    └── e2e-test-flow.md
```

## Customization

### Styling

Custom CSS can be added to [`src/css/custom.css`](./src/css/custom.css).

### Homepage

The homepage is defined in [`src/pages/index.js`](./src/pages/index.js) and features:

- Hero section with project title and tagline
- Feature highlights showcasing key capabilities
- Call-to-action button linking to getting started guide

### Components

Custom React components are located in the `src/components/` directory:

- `HomepageFeatures`: Feature cards displayed on the homepage

## Assets

Static assets are stored in the `static/` directory:

- `img/logo.png`: the project logo. It is also the site `favicon` and the navbar logo; there is no
  `favicon.ico` in the repository, and `docusaurus.config.js` points at this file
- `img/*.svg`: feature icons for the homepage

There is no social card, and the site publishes no `og:image`.
[`docusaurus.config.js`](./docusaurus.config.js) leaves `themeConfig.image` unset, so a link to the
docs shared on Slack, GitHub, X, or Discord renders as a text link with no preview image — the page
itself is fine, the card is empty. To add one: put the image in `static/img/`, set
`themeConfig.image: 'img/<name>.png'`, and Docusaurus emits the meta tags on every page.

## Search

Search is enabled. [`docusaurus.config.js`](./docusaurus.config.js) carries the DocSearch credentials
under `themeConfig.algolia` — the application ID, the public API key, the index name — and the navbar
renders the search box. The index itself is built by the [DocSearch
crawler](https://docsearch.algolia.com/docs/what-is-docsearch/), which runs from the DocSearch
dashboard against `https://autoscaler.homecluster.dev`; nothing in this repository crawls anything, and
the crawl only works while the deployed site is reachable.

Two keys in that block are load-bearing. `externalUrlRegex` makes hits on this domain open in the
existing page instead of a full navigation. There is deliberately **no** `replaceSearchResultPathname`:
the index holds `/docs/…` URLs and the site serves `/docs/…`, so rewriting `/docs/` to `/` sends every
hit to a 404. Add that key only when a second deployment reuses this index under a different `baseUrl`.

## Contributing

When adding new documentation:

1. Add markdown files to the appropriate directory in `../docs`
2. Update `sidebars.js` if adding new sections
3. Test locally with `npm start`
4. Deploy with `npm run deploy`

## Troubleshooting

### Common Issues

1. **Build failures**: Check that all referenced files exist and paths are correct
2. **Broken links**: Ensure all internal links use the correct relative paths
3. **Missing images**: Verify images are in the `static/img/` directory

### Development Server Issues

If the development server fails to start:

1. Clear the cache: `npm run clear`
2. Reinstall dependencies: `rm -rf node_modules && npm install`
3. Check Node.js version (requires Node.js >= 18.0)

## Resources

- [Docusaurus Documentation](https://docusaurus.io/docs)
- [Markdown Features](https://docusaurus.io/docs/markdown-features)
- [Deployment Guide](https://docusaurus.io/docs/deployment)