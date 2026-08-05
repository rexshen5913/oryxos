/**
 * 規劃文檔的單一事實來源：repo 正本 ↔ 站台 slug 的對應。
 *
 * `scripts/sync-docs.mjs`（產生站台檔案）與 `website/.vitepress/config.mts`
 * （產生 sidebar 與 editLink）都從這裡讀，避免兩邊各寫一份而漂移 ——
 * 先前正是因為對應關係寫死在兩處，導致 editLink 指向不存在的正本路徑。
 */

/** @type {{ source: string, slug: string, title: string }[]} */
export const DOCS = [
  { source: 'constitution.md', slug: 'constitution', title: '專案憲法' },
  { source: 'docs/IndustryResearch.md', slug: 'industry-research', title: '業界調研' },
  { source: 'docs/DemandAnalysis.md', slug: 'demand-analysis', title: '需求分析' },
  { source: 'docs/TechnicalSolution.md', slug: 'technical-solution', title: '技術方案' },
  { source: 'docs/AIProgrammingGuide.md', slug: 'ai-programming-guide', title: 'AI 編程指南' },
]

/** 站台語系前綴。文檔為繁中撰寫，兩個語系指向同一份內容，但兩邊路徑都必須實際存在，
 *  否則 VitePress 產生的語系切換連結會指向不存在的頁面。 */
export const LOCALE_PREFIXES = ['', 'zh/']

/** 靜態資源：repo 正本 → website/public/ 下的檔名 */
export const ASSETS = [
  { source: 'docs/images/logo.svg', to: 'logo.svg' },
  { source: 'docs/images/architecture.svg', to: 'architecture.svg' },
]
