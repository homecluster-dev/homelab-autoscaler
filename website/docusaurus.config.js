// @ts-check
// Note: type annotations allow type checking and IDEs autocompletion

const lightCodeTheme = require('prism-react-renderer').themes.github;
const darkCodeTheme = require('prism-react-renderer').themes.dracula;

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'Homelab Autoscaler',
  tagline: 'Kubernetes autoscaling for physical homelab infrastructure',
  favicon: 'img/favicon.ico',

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
  trailingSlash: false,

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
          editUrl: 'https://github.com/homecluster-dev/homelab-autoscaler/tree/main/',
          showLastUpdateAuthor: true,
          showLastUpdateTime: true,
          versions: {
            current: {
              label: 'v0.1.9',
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
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  themes: ['@docusaurus/theme-mermaid'],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      // Replace with your project's social card
      image: 'img/homelab-autoscaler-social-card.jpg',
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
                label: 'Getting Started',
                to: '/docs/getting-started/overview',
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
                label: 'GitHub',
                href: 'https://github.com/homecluster-dev/homelab-autoscaler',
              },
            ],
          },
          {
            title: 'Support',
            items: [
              {
                label: 'Troubleshooting',
                to: '/docs/troubleshooting/debugging-guide',
              },
              {
                label: 'Known Issues',
                to: '/docs/troubleshooting/known-issues',
              },
              {
                label: 'GitHub Issues',
                href: 'https://github.com/homecluster-dev/homelab-autoscaler/issues',
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
        indexName: 'homelab-autoscaler',
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
        // Optional: path for search page that enabled by default (`false` to disable it)
        searchPagePath: 'false',
      },
    }),
};

module.exports = config;