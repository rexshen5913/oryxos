// 把 repo 根目錄的規劃文檔與圖片同步進 website/，供 VitePress 建置。
//
// 為什麼需要這一步：`docs/` 與 `constitution.md` 是 repo 的正本，README 與 CLAUDE.md
// 都引用它們的根目錄路徑；但 VitePress 只能服務 site root（website/）底下的檔案。
// 與其搬動正本、破壞既有引用，不如在建置時顯式複製過去。
//
// 兩個語系各產生一份：文檔為繁中撰寫、內容相同，但 VitePress 的語系切換器會依當前
// 路徑推導對應語系的路徑，兩邊都必須實際存在，否則切換連結會 404。
//
// 產物（website/docs/、website/zh/docs/、website/public/*.svg）皆為生成物，已 gitignore。

import { copyFile, mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { ASSETS, DOCS, LOCALE_PREFIXES } from './docs-manifest.mjs'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const SITE = join(ROOT, 'website')
const REPO = 'https://github.com/rexshen5913/oryxos'

/**
 * 文檔正本以 GitHub 為閱讀情境撰寫，直接餵給 VitePress 會有兩個問題：
 * 1. 內部連結指向 `./docs/Xxx.md` 或 `./constitution.md`，在站台上會 404
 * 2. 圖片路徑 `docs/images/*.svg` 在站台上位於 `/`
 * 這裡做最小幅度的路徑改寫，不動內容。連結需帶上語系前綴，否則從 /zh/ 底下的
 * 頁面點進去會跳出該語系。
 */
function rewrite(md, localePrefix) {
  let out = md

  for (const { source, slug } of DOCS) {
    const file = source.split('/').pop()
    const target = `/${localePrefix}docs/${slug}`
    for (const form of [`(./docs/${file})`, `(./${file})`, `(docs/${file})`, `(${file})`]) {
      out = out.replaceAll(form, `(${target})`)
    }
  }

  // 圖片：docs/images/foo.svg → /foo.svg（public/ 不分語系）
  out = out.replaceAll('](docs/images/', '](/')
  out = out.replaceAll('](./images/', '](/')
  out = out.replaceAll('](images/', '](/')

  return out
}

async function main() {
  await mkdir(join(SITE, 'public'), { recursive: true })

  let written = 0
  for (const prefix of LOCALE_PREFIXES) {
    await mkdir(join(SITE, prefix, 'docs'), { recursive: true })
    for (const { source, slug, title } of DOCS) {
      const raw = await readFile(join(ROOT, source), 'utf8')
      // 補 frontmatter，讓 VitePress 的頁面標題與 SEO 正確
      const front = `---\ntitle: ${title}\n---\n\n`
      // 本頁是複本，站台上沒有可編輯的檔案。明確指向正本，取代 VitePress 的 editLink
      // （其路徑推導無法對應改名後的複本，見 config.mts 的說明）。
      const origin =
        `> 本頁由正本 [\`${source}\`](${REPO}/blob/main/${source}) 自動同步產生，` +
        `請於正本編輯。\n\n`
      await writeFile(
        join(SITE, prefix, 'docs', `${slug}.md`),
        front + origin + rewrite(raw, prefix),
        'utf8',
      )
      written++
    }
  }

  for (const { source, to } of ASSETS) {
    await copyFile(join(ROOT, source), join(SITE, 'public', to))
  }

  console.log(
    `sync-docs: ${written} 個文檔頁（${DOCS.length} 份 × ${LOCALE_PREFIXES.length} 語系）、` +
      `${ASSETS.length} 個資源已同步至 website/`,
  )
}

main().catch((err) => {
  console.error('sync-docs 失敗:', err)
  process.exit(1)
})
