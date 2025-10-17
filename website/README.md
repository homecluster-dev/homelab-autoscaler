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

The documentation sidebar is configured in [`sidebars.js`](./sidebars.js). It organizes the documentation into logical sections:

- Getting Started
- Architecture
- API Reference
- Development
- Troubleshooting

### Versioning

The site supports versioning for documentation. The current version is extracted from the Helm chart version (0.1.9).

To create a new version:

```bash
npm run docusaurus docs:version 0.2.0
```

## Documentation Structure

The documentation is read from the `../docs` directory and organized as follows:

```
docs/
├── getting-started/
│   ├── overview.md
│   ├── installation.md
│   └── quick-start.md
├── architecture/
│   ├── overview.md
│   └── state.md
├── api-reference/
│   ├── crds/
│   └── examples/
├── development/
│   ├── setup.md
│   └── crd-sync.md
└── troubleshooting/
    ├── debugging-guide.md
    └── known-issues.md
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

- `img/logo.png`: Project logo (copied from docs/images/logo.png)
- `img/*.svg`: Feature icons for the homepage

## Search

The site is configured for Algolia DocSearch. Update the search configuration in `docusaurus.config.js` with your Algolia credentials when ready to enable search.

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