// @ts-check
// Note: type annotations allow type checking and IDEs autocompletion

const lightCodeTheme = require('prism-react-renderer').themes.github;
const darkCodeTheme = require('prism-react-renderer').themes.dracula;

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'Homelab Autoscaler',
  tagline: 'Kubernetes autoscaling for physical homelab infrastructure',
  favicon: 'img/logo.png',

  // Set the production url of your site here
  url: 'https://autoscaler.homecluster.dev',
  // Set the /<baseUrl>/ pathname under which your site is served
  // For GitHub pages deployment, it is often '/<projectName>/'
  baseUrl: '/',

  // GitHub pages deployment config.
  // If you aren't using GitHub pages, you don't need these.
  organizationName: 'homecluster-dev', // Usually your GitHub org/user name.
  projectName: 'homelab-autoscaler', // Usually your repo name.
  deploymentBranch: 'gh-pages',
  // The Docusaurus default. GitHub Pages serves `<dir>/index.html` for `<dir>/`, and a route with no
  // trailing slash redirect to it, so the docs root (`/docs/`, the `slug: /` of docs/intro.md) is a
  // real file: with `false` the route is `docs.html`, `/docs/` has nothing to serve, and the documented
  // URL of the site 404s.
  trailingSlash: true,

  onBrokenLinks: 'throw',

  // Even if you don't use internalization, you can use this field to set useful
  // metadata like html lang. For example, if your site is Chinese, you may want
  // to replace "en" with "zh-Hans".
  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: '../docs',
          routeBasePath: 'docs',
          sidebarPath: require.resolve('./sidebars.js'),
          editUrl: 'https://github.com/homecluster-dev/homelab-autoscaler/edit/main/',
          showLastUpdateAuthor: true,
          showLastUpdateTime: true,
          versions: {
            current: {
              // The deploy workflow rewrites this label from dist/chart/Chart.yaml on every release,
              // so it stays in step with the chart. Keep the `label: 'v...'` shape it patches.
              label: 'v0.2.3',
              path: '',
            },
          },
        },
        blog: false, // Disable blog
        theme: {
          customCss: require.resolve('./src/css/custom.css'),
        },
      }),
    ],
  ],

  markdown: {
    mermaid: true,
    // Docusaurus 3.10 moved this here: the top-level `onBrokenMarkdownLinks` is the option its own
    // build warning calls deprecated. `markdown.hooks` is the current location, and 'warn' keeps a
    // broken relative link in a docs page visible in CI without failing the site the way
    // `onBrokenLinks: 'throw'` does for a broken route.
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  themes: ['@docusaurus/theme-mermaid'],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      navbar: {
        title: 'Homelab Autoscaler',
        logo: {
          alt: 'Homelab Autoscaler Logo',
          src: 'img/logo.png',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'tutorialSidebar',
            position: 'left',
            label: 'Documentation',
          },
          {
            type: 'docsVersionDropdown',
            position: 'right',
            dropdownActiveClassDisabled: true,
          },
          {
            href: 'https://github.com/homecluster-dev/homelab-autoscaler',
            label: 'GitHub',
            position: 'right',
          },
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            title: 'Documentation',
            items: [
              {
                label: 'Start Here',
                to: '/',
              },
              {
                label: 'The NodeClaim Model',
                to: '/docs/concepts/nodeclaim-model',
              },
              {
                label: 'Architecture',
                to: '/docs/architecture/overview',
              },
              {
                label: 'API Reference',
                to: '/docs/api-reference/crds/group',
              },
            ],
          },
          {
            title: 'Operations',
            items: [
              {
                label: 'Read The Pool',
                to: '/docs/concepts/pool-at-a-glance',
              },
              {
                label: 'Common Issues',
                to: '/docs/troubleshooting/common-issues',
              },
              {
                label: 'Known Issues',
                to: '/docs/troubleshooting/known-issues',
              },
              {
                label: 'Events',
                to: '/docs/reference/events',
              },
            ],
          },
          {
            title: 'Development',
            items: [
              {
                label: 'Setup Guide',
                to: '/docs/development/setup',
              },
              {
                label: 'CRD Sync',
                to: '/docs/development/crd-sync',
              },
              {
                label: 'NodeClaim Design',
                to: '/docs/development/nodeclaim',
              },
              {
                label: 'GitHub',
                href: 'https://github.com/homecluster-dev/homelab-autoscaler',
              },
            ],
          },
        ],
        copyright: `Copyright © ${new Date().getFullYear()} Homelab Autoscaler. Built with Docusaurus.`,
      },
      prism: {
        theme: lightCodeTheme,
        darkTheme: darkCodeTheme,
        additionalLanguages: ['yaml', 'bash', 'go'],
      },
      algolia: {
        // The application ID provided by Algolia
        appId: '3Y0BV0S84E',
        // Public API key: it is safe to commit it
        apiKey: '7b817b82c15002bcd233cb8d08c45e77',
        indexName: 'Homecluster Autoscaler',
        // Optional: see doc section below
        contextualSearch: true,
        // Optional: Specify domains where the navigation should occur through window.location instead on history.push
        externalUrlRegex: 'autoscaler\\.homecluster\\.dev',
        // Optional: Replace parts of the item URLs from Algolia. Useful when using the same search index for multiple deployments using a different baseUrl
        replaceSearchResultPathname: {
          from: '/docs/', // or as RegExp: /\/docs\//
          to: '/',
        },
        // Optional: Algolia search parameters
        searchParameters: {},
        // The site's own search page, disabled. `false` is the boolean: the string 'false' is a
        // path, and the navbar renders a link to /false.
        searchPagePath: false,
      },
    }),
};

module.exports = config;