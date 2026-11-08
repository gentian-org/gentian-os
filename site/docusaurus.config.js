// @ts-check
// The site at docs.gentian.org. Content is assembled by prepare.py into
// content/ from the gentian-* repositories; nothing under content/ is edited
// by hand.
import {themes as prismThemes} from 'prism-react-renderer';

const repo = 'https://github.com/gentian-org';
const branch = process.env.GENTIAN_DOCS_BRANCH || 'develop';

// The gentian-os root pages prepare.py places under docs/.
const rootPages = {
  'getting-started.md': 'GETTING-STARTED.md',
  'contributing/contributing.md': 'CONTRIBUTING.md',
  'contributing/licensing.md': 'LICENSING.md',
  'contributing/agents.md': 'AGENTS.md',
  'security/reporting.md': 'SECURITY.md',
};

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'Gentian OS',
  tagline: 'A cloud operating system for open-source business software',
  url: 'https://docs.gentian.org',
  baseUrl: '/',
  trailingSlash: false,
  organizationName: 'gentian-org',
  projectName: 'gentian-os',
  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',
  markdown: {
    format: 'detect', // .md is CommonMark, .mdx is MDX
    mermaid: true,
    hooks: {onBrokenMarkdownLinks: 'throw'},
  },
  i18n: {defaultLocale: 'en', locales: ['en']},
  themes: [
    '@docusaurus/theme-mermaid',
    [
      '@easyops-cn/docusaurus-search-local',
      /** @type {import('@easyops-cn/docusaurus-search-local').PluginOptions} */
      ({
        hashed: true,
        docsRouteBasePath: ['/', 'apps', 'ui'],
        docsDir: ['content/os', 'content/apps', 'content/ui'],
        indexBlog: false,
        indexPages: false,
      }),
    ],
  ],
  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: 'content/os',
          routeBasePath: '/',
          sidebarPath: './sidebars.js',
          editUrl: ({docPath}) => `${repo}/gentian-os/edit/${branch}/${rootPages[docPath] || `docs/${docPath}`}`,
          showLastUpdateTime: false,
        },
        blog: false,
        pages: false,
        theme: {customCss: './src/css/custom.css'},
      }),
    ],
  ],
  plugins: [
    [
      '@docusaurus/plugin-content-docs',
      {
        id: 'apps',
        path: 'content/apps',
        routeBasePath: 'apps',
        sidebarPath: './sidebars-external.js',
        editUrl: ({docPath}) => `${repo}/gentian-apps/edit/${branch}/${docPath === 'index.md' ? 'README.md' : `docs/${docPath}`}`,
      },
    ],
    [
      '@docusaurus/plugin-content-docs',
      {
        id: 'ui',
        path: 'content/ui',
        routeBasePath: 'ui',
        sidebarPath: './sidebars-external.js',
      },
    ],
  ],
  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      colorMode: {respectPrefersColorScheme: true},
      docs: {sidebar: {hideable: true, autoCollapseCategories: true}},
      navbar: {
        title: 'Gentian OS',
        items: [
          {to: '/', label: 'OS', position: 'left', activeBaseRegex: '^/(?!apps|ui)'},
          {to: '/apps/', label: 'Apps', position: 'left'},
          {to: '/ui/', label: 'UI', position: 'left'},
          {href: `${repo}`, label: 'GitHub', position: 'right'},
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            title: 'Repositories',
            items: [
              {label: 'gentian-os', href: `${repo}/gentian-os`},
              {label: 'gentian-apps', href: `${repo}/gentian-apps`},
              {label: 'gentian-ui', href: `${repo}/gentian-ui`},
              {label: 'gentian-deployments', href: `${repo}/gentian-deployments`},
            ],
          },
          {
            title: 'Start',
            items: [
              {label: 'Introduction', to: '/overview/introduction'},
              {label: 'Getting started', to: '/getting-started'},
              {label: 'Security principles', to: '/security/principles'},
              {label: 'llms.txt', href: 'pathname:///llms.txt', target: '_self'},
            ],
          },
        ],
        copyright: `The Gentian OS documentation, built from the ${branch} branch. Text under MPL-2.0 like the code it describes.`,
      },
      prism: {
        theme: prismThemes.github,
        darkTheme: prismThemes.dracula,
        additionalLanguages: ['bash', 'yaml', 'json', 'go', 'sql', 'toml', 'diff'],
      },
      mermaid: {theme: {light: 'neutral', dark: 'dark'}},
    }),
};

export default config;
