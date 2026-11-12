/**
 * The docs sidebar. Categories are the order an operator meets the material in: start, understand,
 * run, look up, contribute. Every doc in `docs/` is listed here — a page in the tree and not in the
 * sidebar is a page nothing links to.
 */

// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  tutorialSidebar: [
    {
      type: 'category',
      label: 'Getting Started',
      collapsed: false,
      items: [
        'intro',
        'getting-started/overview',
        'getting-started/installation',
        'getting-started/quick-start',
      ],
    },
    {
      type: 'category',
      label: 'Concepts',
      collapsed: false,
      items: [
        'concepts/nodeclaim-model',
        'concepts/node-labels',
        'concepts/pool-at-a-glance',
        'architecture/overview',
      ],
    },
    {
      type: 'category',
      label: 'Operations',
      collapsed: false,
      items: [
        'troubleshooting/common-issues',
        'troubleshooting/debugging-guide',
        'troubleshooting/known-issues',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      collapsed: true,
      items: [
        {
          type: 'category',
          label: 'Custom Resource Definitions',
          items: [
            'api-reference/crds/group',
            'api-reference/crds/node',
            'api-reference/crds/nodeclaim',
          ],
        },
        {
          type: 'category',
          label: 'Examples',
          link: {
            type: 'doc',
            id: 'api-reference/examples/index',
          },
          items: [
            'api-reference/examples/k3d-group',
            'api-reference/examples/k3d-node',
            'api-reference/examples/production-examples',
          ],
        },
        'reference/events',
        'releases/changelog',
        'releases/upgrade-0.1-to-0.2',
      ],
    },
    {
      type: 'category',
      label: 'Contributing',
      collapsed: true,
      items: [
        'development/setup',
        'development/crd-sync',
        'development/state',
        'development/nodeclaim',
        'development/e2e-test-flow',
      ],
    },
  ],
};

module.exports = sidebars;
