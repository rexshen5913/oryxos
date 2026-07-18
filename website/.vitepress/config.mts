import { defineConfig } from 'vitepress'
import { DOCS } from '../../scripts/docs-manifest.mjs'

const SITE = 'https://rexshen5913.github.io/oryxos'
const REPO = 'https://github.com/rexshen5913/oryxos'

/**
 * 規劃文檔為繁中撰寫，兩個語系指向同一份內容，但各自有實際存在的路徑
 * （由 scripts/sync-docs.mjs 產生），語系切換才不會 404。
 */
const sidebarFor = (prefix: string) => [
  {
    text: '規劃文檔',
    items: DOCS.map((d) => ({ text: d.title, link: `/${prefix}docs/${d.slug}` })),
  },
]

/*
 * 刻意不設 editLink。
 *
 * docs 頁全是 scripts/sync-docs.mjs 從 repo 正本產生的複本，站台路徑與正本路徑
 * 對不上（/docs/technical-solution → docs/TechnicalSolution.md；
 * /docs/constitution → 根目錄 constitution.md），而且編輯複本本身沒有意義。
 * 改由 sync-docs.mjs 在每頁頂端注入指向正本的連結，路徑由 manifest 保證正確。
 *
 * 註：VitePress 會把 editLink 的 pattern 函式序列化進 client bundle，
 * 閉包捕捉的 import 無法還原，因此「用函式反查正本」這條路本來就走不通。
 */

export default defineConfig({
  title: 'OryxOS',
  description: '用 Go 打造的企業級 Agent OS — 完全可控、單一靜態二進制部署的 Agent 運行時內核。',
  cleanUrls: true,
  lastUpdated: true,
  base: '/oryxos/',
  // 不設 ignoreDeadLinks：站內壞連結應該擋下建置，而不是靜默帶進產物。
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/oryxos/logo.svg' }],
    ['meta', { name: 'theme-color', content: '#00ADD8' }],
    ['meta', { name: 'keywords', content: 'OryxOS, Agent OS, Go, ReAct, LLM, MCP, agent runtime, 智能體' }],
    ['meta', { name: 'robots', content: 'index, follow' }],
    ['meta', { property: 'og:type', content: 'website' }],
    ['meta', { property: 'og:site_name', content: 'OryxOS' }],
    ['meta', { property: 'og:title', content: 'OryxOS — Agent OS built in Go' }],
    ['meta', { property: 'og:description', content: 'A fully controllable Agent runtime kernel. Single static binary, no framework magic.' }],
    ['meta', { property: 'og:url', content: SITE }],
    ['meta', { name: 'twitter:card', content: 'summary_large_image' }],
    ['link', { rel: 'canonical', href: SITE }],
  ],

  sitemap: { hostname: SITE },

  themeConfig: {
    logo: '/logo.svg',
    siteTitle: 'OryxOS',
    socialLinks: [{ icon: 'github', link: REPO }],
    search: { provider: 'local' },
    outline: { level: [2, 3] },
  },

  locales: {
    root: {
      label: 'English',
      lang: 'en-US',
      themeConfig: {
        nav: [
          { text: 'Docs (中文)', link: '/docs/constitution' },
          { text: 'Contributing', link: `${REPO}/blob/main/CONTRIBUTING.md` },
        ],
        sidebar: { '/docs/': sidebarFor('') },
        docFooter: { prev: 'Previous', next: 'Next' },
        darkModeSwitchLabel: 'Appearance',
        returnToTopLabel: 'Return to top',
        lastUpdatedText: 'Last updated',
      },
    },
    zh: {
      label: '繁體中文',
      lang: 'zh-TW',
      link: '/zh/',
      themeConfig: {
        nav: [
          { text: '文檔', link: '/zh/docs/constitution' },
          { text: '參與貢獻', link: `${REPO}/blob/main/CONTRIBUTING.md` },
        ],
        sidebar: { '/zh/docs/': sidebarFor('zh/') },
        docFooter: { prev: '上一頁', next: '下一頁' },
        outline: { label: '本頁目錄', level: [2, 3] },
        darkModeSwitchLabel: '外觀',
        returnToTopLabel: '回到頂部',
        lastUpdatedText: '最後更新',
      },
    },
  },
})
