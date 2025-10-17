/**
 * Creating a sidebar enables you to:
 - create an ordered group of docs
 - render a sidebar for each doc of that group
 - provide next/previous navigation

 The sidebars can be generated from the filesystem, or explicitly defined here.

 Create as many sidebars as you want.
 */

// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  // By default, Docusaurus generates a sidebar from the docs folder structure
  tutorialSidebar: [
    {
      type: 'category',
      label: 'Getting Started',
      items: [
        'getting-started/overview',
        'getting-started/installation',
        'getting-started/quick-start',
      ],
      collapsed: false,
    },
    {
      type: 'category',
      label: 'Architecture',
      items: [
        'architecture/overview',
        'architecture/state',
      ],
      collapsed: false,
    },
    {
      type: 'category',
      label: 'API Reference',
      items: [
        {
          type: 'category',
          label: 'Custom Resource Definitions',
          items: [
            'api-reference/crds/group',
            'api-reference/crds/node',
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
      ],
      collapsed: true,
    },
    {
      type: 'category',
      label: 'Development',
      items: [
        'development/setup',
        'development/crd-sync',
      ],
      collapsed: true,
    },
    {
      type: 'category',
      label: 'Troubleshooting',
      items: [
        'troubleshooting/debugging-guide',
        'troubleshooting/known-issues',
      ],
      collapsed: true,
    },
  ],

  // But you can create a sidebar manually
  /*
  tutorialSidebar: [
    'intro',
    'hello',
    {
      type: 'category',
      label: 'Tutorial',
      items: ['tutorial-basics/create-a-document'],
    },
  ],
   */
};

module.exports = sidebars;