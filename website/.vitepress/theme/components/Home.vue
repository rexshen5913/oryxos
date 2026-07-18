<script setup>
import { computed } from 'vue'
import { useData, withBase } from 'vitepress'

const { lang } = useData()
const isZh = computed(() => lang.value === 'zh-TW')
/** 雙語並置：一份 template，兩種語言字串就近比對，不會漏翻。 */
const t = (zh, en) => (isZh.value ? zh : en)

const REPO = 'https://github.com/rexshen5913/oryxos'

const problems = computed(() => [
  {
    n: '01',
    q: t('執行流程被框架接管，出事無從追起', 'The framework owns the loop — and you can’t see inside it'),
    a: t(
      'Agent 框架把 ReAct 循環包成黑箱，工具調度、重試、上下文組裝都藏在抽象層之後。生產環境出問題時，你除錯的是別人的控制流。',
      'Agent frameworks wrap the ReAct loop in a black box. Tool dispatch, retries and context assembly all hide behind abstractions — in production you end up debugging someone else’s control flow.',
    ),
  },
  {
    n: '02',
    q: t('部署帶著整套運行時，難進企業環境', 'Deployment drags a whole runtime along'),
    a: t(
      '直譯語言的 Agent 服務要帶解譯器、虛擬環境與數十個相依套件。在受管制的內網環境，光是通過部署審查就足以拖垮專案。',
      'Agent services in interpreted languages ship an interpreter, a virtualenv and dozens of transitive dependencies. In a locked-down enterprise network, deployment review alone can sink the project.',
    ),
  },
])

const compare = computed(() => ({
  todayTitle: t('常見做法', 'Common practice'),
  today: [
    t('框架的自動執行 Agent 抽象接管循環', 'Framework’s auto-executing Agent abstraction owns the loop'),
    t('反射／型別掃描自動裝配工具與模型', 'Tools and models wired by reflection or type scanning'),
    t('解譯器 ＋ 虛擬環境 ＋ 相依樹一起部署', 'Interpreter + virtualenv + dependency tree deployed together'),
    t('審計靠日誌，事後難以還原決策路徑', 'Auditing via logs — decision paths hard to reconstruct after the fact'),
  ],
  oursTitle: 'OryxOS',
  ours: [
    t('ReAct 循環自實現，每一步都可讀可改', 'ReAct loop implemented in-house — every step readable and modifiable'),
    t('Provider 與 Tool 一律顯式註冊，無魔法', 'Providers and Tools registered explicitly — no magic'),
    t('CGO_ENABLED=0 編出單一靜態二進制', 'A single static binary via CGO_ENABLED=0'),
    t('工具調用與 LLM 呼叫 day-one 落庫', 'Tool invocations and LLM calls persisted to the database on day one'),
  ],
}))

const capabilities = computed(() => [
  {
    no: 'I',
    title: t('對接 LLM', 'LLM Providers'),
    meta: 'go-openai · OpenAI-compatible · provider abstraction',
    desc: t(
      '以薄包裝接 OpenAI 兼容協議，配上自實現的 Provider 抽象，跨 provider 統一介面。go-openai 只做協議轉換與 tool schema 生成，不碰調度。',
      'A thin wrapper over the OpenAI-compatible protocol plus an in-house Provider abstraction. go-openai handles protocol translation and tool schema generation only — never dispatch.',
    ),
  },
  {
    no: 'II',
    title: t('ReAct 循環', 'ReAct Loop'),
    meta: 'ReActLoop · ToolExecutor · fully self-implemented',
    desc: t(
      'Agent 的大腦。循環由 ReActLoop 與 ToolExecutor 完全自實現、完全可控，不採用任何框架的自動執行 Agent 抽象。',
      'The brain. The loop is implemented entirely by ReActLoop and ToolExecutor — fully controllable, with no framework auto-execution abstraction anywhere.',
    ),
  },
  {
    no: 'III',
    title: t('三層記憶', 'Three-tier Memory'),
    meta: t('短期 · 工作 · 長期 · SQLite ＋ MEMORY.md', 'short-term · working · long-term · SQLite + MEMORY.md'),
    desc: t(
      '短期／工作／長期三層統一門面，讓 Agent 記得住上下文。核心階段以 SQLite 與 MEMORY.md 落地，向量檢索放擴展階段。',
      'A unified facade over short-term, working and long-term memory. The core phase ships on SQLite and MEMORY.md; vector retrieval lands in the extension phase.',
    ),
  },
  {
    no: 'IV',
    title: t('工具體系', 'Tool System'),
    meta: 'File / Shell / Http · MCP Client · SKILL.md',
    desc: t(
      '內建 Tool 加上 MCP Client 與 Plugin 自定義工具，主推 SKILL.md 與 MCP 的零程式碼接入方式。',
      'Built-in tools plus an MCP client and custom plugin tools — with SKILL.md and MCP as the primary zero-code integration paths.',
    ),
  },
  {
    no: 'V',
    title: t('Web Service', 'Web Service'),
    meta: 'net/http · chi · OpenAPI',
    desc: t(
      '以標準庫 net/http 搭配 chi 對外暴露 HTTP API，供既有業務系統集成，不引入重框架。',
      'HTTP APIs exposed through the standard library’s net/http with chi, ready for integration with existing systems — no heavyweight framework.',
    ),
  },
])

const articles = computed(() => [
  { n: '一', t: t('工程與技術棧', 'Engineering & Stack'), d: t('Go >= 1.24，CGO_ENABLED=0 靜態編譯；禁 cgo 依賴；標準庫優先。', 'Go >= 1.24, statically compiled with CGO_ENABLED=0. No cgo dependencies. Standard library first.') },
  { n: '二', t: t('Agent 核心', 'Agent Core'), d: t('ReAct loop 自實現；LLM 邊界只做協議轉換；顯式優於魔法。', 'ReAct loop implemented in-house. The LLM boundary does protocol translation only. Explicit over magic.') },
  { n: '三', t: t('簡單性原則', 'Simplicity First'), d: t('YAGNI；反過度工程；簡單函式與資料結構優於複雜介面。', 'YAGNI. No over-engineering. Simple functions and data structures over elaborate interfaces.') },
  { n: '四', t: t('測試先行鐵律', 'Test-First Imperative'), d: t('不可協商。所有變更從失敗的測試開始；表格驅動；拒絕堆疊 mock。', 'Non-negotiable. Every change starts with a failing test. Table-driven. No mock stacking.') },
  { n: '五', t: t('明確性原則', 'Clarity & Explicitness'), d: t('錯誤一律以 %w 包裝；無全域變數；阻塞路徑走 context。', 'Errors always wrapped with %w. No global state. Every blocking path carries a context.') },
  { n: '六', t: t('資料與審計', 'Data & Audit'), d: t('SQLite 純 Go 驅動；tool_invocations 與 llm_calls 於核心階段即落庫。', 'Pure-Go SQLite driver. tool_invocations and llm_calls persisted from the core phase onward.') },
])

const stack = computed(() => [
  { k: t('語言', 'Language'), v: 'Go >= 1.24', why: t('基礎設施母語；快啟動、低記憶體、原生併發', 'The native tongue of infrastructure — fast startup, low memory, built-in concurrency') },
  { k: t('部署', 'Deployment'), v: 'CGO_ENABLED=0', why: t('go build 直出單一靜態二進制，無需額外運行時', 'go build emits one static binary — no extra runtime required') },
  { k: t('儲存', 'Storage'), v: 'modernc.org/sqlite', why: t('純 Go 驅動，避免 cgo，守住單一二進制', 'Pure Go driver — avoids cgo and preserves the single binary') },
  { k: 'Web', v: 'net/http + chi', why: t('標準庫優先，不引入重框架', 'Standard library first — no heavyweight framework') },
  { k: 'LLM', v: 'go-openai', why: t('只做協議轉換與 tool schema 生成', 'Protocol translation and tool schema generation only') },
  { k: 'CLI', v: 'cobra', why: t('cmd/oryxos 單一 main，約 10ms 啟動', 'A single main under cmd/oryxos, ~10ms startup') },
])

const roadmap = computed(() => [
  {
    phase: t('核心階段', 'Core Phase'),
    state: t('進行中', 'In progress'),
    active: true,
    items: t(
      '執行時內核：五大核心能力 ＋ CLI Channel ＋ SQLite 審計地基。每個 user story 完成後有可演示 demo。',
      'The runtime kernel: five core capabilities, the CLI channel and the SQLite audit foundation. Every user story ends in a runnable demo.',
    ),
  },
  {
    phase: t('擴展階段', 'Extension Phase'),
    state: t('規劃中', 'Planned'),
    active: false,
    items: t(
      'IM Channel（Slack／Telegram／Discord）、向量檢索（chromem-go／sqlite-vec／pgvector）、更多 Provider adapter。',
      'IM channels (Slack / Telegram / Discord), vector retrieval (chromem-go / sqlite-vec / pgvector) and more provider adapters.',
    ),
  },
  {
    phase: t('治理階段', 'Governance Phase'),
    state: t('規劃中', 'Planned'),
    active: false,
    items: t(
      '多租戶、Tool Policy、完整審計、SSO 等企業級治理能力。',
      'Multi-tenancy, tool policy, full audit and SSO — the enterprise governance layer.',
    ),
  },
])
</script>

<template>
  <div class="oryx">
    <!-- ── 1. HERO ─────────────────────────────────────────── -->
    <section class="ox-hero">
      <div class="ox-hero-inner">
        <img :src="withBase('/logo.svg')" alt="OryxOS" class="ox-logo" width="96" height="96" />

        <span class="ox-badge">
          <i class="ox-dot" />
          {{ t('pre-alpha · 文檔規劃期', 'pre-alpha · planning phase') }}
        </span>

        <h1 class="ox-wordmark">OryxOS</h1>

        <p class="ox-tagline">
          {{ t('用 Go 打造的企業級 Agent OS', 'An enterprise Agent OS, built in Go') }}
        </p>

        <p class="ox-lede">
          {{ t(
            '一個完全可控、單一靜態二進制部署的 Agent 運行時內核。ReAct 循環自實現，Provider 與 Tool 顯式註冊，不靠任何框架魔法。',
            'A fully controllable Agent runtime kernel that deploys as one static binary. The ReAct loop is implemented in-house; providers and tools are registered explicitly. No framework magic.',
          ) }}
        </p>

        <div class="ox-cta-row">
          <a class="ox-btn ox-btn-primary" href="/oryxos/docs/constitution">
            {{ t('讀專案憲法', 'Read the Constitution') }}
          </a>
          <a class="ox-btn" href="/oryxos/docs/technical-solution">
            {{ t('技術方案', 'Technical Design') }}
          </a>
          <a class="ox-btn" :href="REPO" target="_blank" rel="noreferrer">GitHub</a>
        </div>

        <p class="ox-eco">
          Go >= 1.24 · CGO_ENABLED=0 · modernc SQLite · net/http + chi · go-openai · MCP · cobra
        </p>
      </div>
    </section>

    <!-- ── 2. 現況（誠實聲明）──────────────────────────────── -->
    <section class="ox-section ox-status-section">
      <div class="ox-inner ox-status">
        <div class="ox-status-mark">!</div>
        <div>
          <h3 class="ox-status-title">{{ t('這個專案目前還沒有程式碼', 'This project has no code yet') }}</h3>
          <p class="ox-status-body">
            {{ t(
              'OryxOS 處於文檔級規劃期。repo 內尚無 go.mod、cmd/ 或 internal/，目前的「原始碼」是專案憲法與四份規劃文檔。本頁描述的能力與指令屬目標狀態，會隨實作推進逐步落地 —— 我們寧可先把話說清楚，也不想讓你在 clone 之後才發現。',
              'OryxOS is in a documentation-level planning phase. There is no go.mod, cmd/ or internal/ in the repository yet — the current “source” is the project constitution and four planning documents. Everything described on this page is a target state that will land incrementally. We would rather say so up front than let you discover it after cloning.',
            ) }}
          </p>
        </div>
      </div>
    </section>

    <!-- ── 3. 問題 ─────────────────────────────────────────── -->
    <section class="ox-section">
      <div class="ox-inner ox-problem">
        <div class="ox-problem-narrative">
          <span class="ox-label">{{ t('問題', 'The Problem') }}</span>
          <h2 class="ox-h2">{{ t('兩個結構性問題', 'Two structural problems') }}</h2>
          <p class="ox-p">
            {{ t(
              'Agent 要進到企業的生產環境，卡住的往往不是模型能力，而是這兩件事：',
              'What blocks Agents from reaching enterprise production is rarely model capability. It is these two things:',
            ) }}
          </p>

          <div v-for="p in problems" :key="p.n" class="ox-problem-item">
            <h3 class="ox-problem-q"><span class="ox-problem-n">{{ p.n }}</span>{{ p.q }}</h3>
            <p class="ox-problem-a">{{ p.a }}</p>
          </div>

          <p class="ox-p ox-strong">
            {{ t(
              'OryxOS 要解決的就是這兩件事 —— 讓執行流程完全透明，讓部署退化成複製一個檔案。',
              'These are exactly the two problems OryxOS sets out to solve: make execution fully transparent, and reduce deployment to copying one file.',
            ) }}
          </p>
        </div>

        <div class="ox-compare">
          <div class="ox-compare-card">
            <div class="ox-compare-head">{{ compare.todayTitle }}</div>
            <ul>
              <li v-for="(x, i) in compare.today" :key="i"><span class="ox-x">✗</span>{{ x }}</li>
            </ul>
          </div>
          <div class="ox-compare-card ox-compare-card-ours">
            <div class="ox-compare-head">{{ compare.oursTitle }}</div>
            <ul>
              <li v-for="(x, i) in compare.ours" :key="i"><span class="ox-check">✓</span>{{ x }}</li>
            </ul>
          </div>
        </div>
      </div>
    </section>

    <!-- ── 4. 架構圖 ───────────────────────────────────────── -->
    <section class="ox-section ox-alt">
      <div class="ox-inner">
        <div class="ox-section-head">
          <span class="ox-label">{{ t('架構', 'Architecture') }}</span>
          <h2 class="ox-h2">{{ t('一個 ReAct 循環，向下驅動三大服務', 'One ReAct loop driving three services') }}</h2>
        </div>
        <img :src="withBase('/architecture.svg')" :alt="t('OryxOS 架構圖', 'OryxOS architecture')" class="ox-arch" />
      </div>
    </section>

    <!-- ── 5. 五大核心能力 ─────────────────────────────────── -->
    <section class="ox-section">
      <div class="ox-inner">
        <div class="ox-section-head">
          <span class="ox-label">{{ t('核心能力', 'Core Capabilities') }}</span>
          <h2 class="ox-h2">{{ t('五大核心能力', 'Five core capabilities') }}</h2>
        </div>
        <div class="ox-caps">
          <article v-for="c in capabilities" :key="c.no" class="ox-cap">
            <div class="ox-cap-no">{{ c.no }}</div>
            <h3 class="ox-cap-title">{{ c.title }}</h3>
            <p class="ox-cap-meta">{{ c.meta }}</p>
            <p class="ox-cap-desc">{{ c.desc }}</p>
          </article>
        </div>
      </div>
    </section>

    <!-- ── 6. 專案憲法 ─────────────────────────────────────── -->
    <section class="ox-section ox-alt">
      <div class="ox-inner">
        <div class="ox-section-head">
          <span class="ox-label">{{ t('憲法', 'Constitution') }}</span>
          <h2 class="ox-h2">{{ t('六條不可協商的原則', 'Six non-negotiable principles') }}</h2>
          <p class="ox-p ox-center">
            {{ t(
              '專案憲法的效力高於任何工具設定或單次討論。與憲法牴觸的 PR 不會被合併。',
              'The constitution outranks any tool config or one-off discussion. Pull requests that contradict it are not merged.',
            ) }}
          </p>
        </div>
        <div class="ox-articles">
          <article v-for="a in articles" :key="a.n" class="ox-article">
            <div class="ox-article-n">{{ a.n }}</div>
            <div>
              <h3 class="ox-article-t">{{ a.t }}</h3>
              <p class="ox-article-d">{{ a.d }}</p>
            </div>
          </article>
        </div>
        <div class="ox-center-row">
          <a class="ox-btn" href="/oryxos/docs/constitution">{{ t('讀完整憲法', 'Read the full constitution') }}</a>
        </div>
      </div>
    </section>

    <!-- ── 7. 技術棧 ───────────────────────────────────────── -->
    <section class="ox-section">
      <div class="ox-inner">
        <div class="ox-section-head">
          <span class="ox-label">{{ t('技術棧', 'Stack') }}</span>
          <h2 class="ox-h2">{{ t('每一項選型都有理由', 'Every choice, justified') }}</h2>
        </div>
        <div class="ox-stack">
          <div v-for="s in stack" :key="s.k" class="ox-stack-row">
            <div class="ox-stack-k">{{ s.k }}</div>
            <code class="ox-stack-v">{{ s.v }}</code>
            <div class="ox-stack-why">{{ s.why }}</div>
          </div>
        </div>
      </div>
    </section>

    <!-- ── 8. 路線圖 ───────────────────────────────────────── -->
    <section class="ox-section ox-alt">
      <div class="ox-inner">
        <div class="ox-section-head">
          <span class="ox-label">{{ t('路線圖', 'Roadmap') }}</span>
          <h2 class="ox-h2">{{ t('三個階段', 'Three phases') }}</h2>
        </div>
        <div class="ox-roadmap">
          <article v-for="r in roadmap" :key="r.phase" class="ox-phase" :class="{ 'is-active': r.active }">
            <div class="ox-phase-head">
              <h3 class="ox-phase-t">{{ r.phase }}</h3>
              <span class="ox-phase-s">{{ r.state }}</span>
            </div>
            <p class="ox-phase-d">{{ r.items }}</p>
          </article>
        </div>
      </div>
    </section>

    <!-- ── 9. CTA ──────────────────────────────────────────── -->
    <section class="ox-section ox-cta-section">
      <div class="ox-inner ox-cta-inner">
        <h2 class="ox-h2">{{ t('現在最需要的是質疑', 'What this project needs most right now is scrutiny') }}</h2>
        <p class="ox-p">
          {{ t(
            '文檔規劃期沒有程式碼可以貢獻，但架構決策正在定型 —— 這是影響力最大的時候。讀完規劃文檔後，對任何一個決策提出反對意見，都比日後改程式碼便宜得多。',
            'There is no code to contribute during the planning phase, but the architectural decisions are still setting — which is exactly when input matters most. Reading the planning docs and arguing against any decision is far cheaper now than rewriting code later.',
          ) }}
        </p>
        <div class="ox-cta-row ox-center-row">
          <a class="ox-btn ox-btn-primary" :href="`${REPO}/discussions`" target="_blank" rel="noreferrer">
            {{ t('開一個 Discussion', 'Start a Discussion') }}
          </a>
          <a class="ox-btn" :href="`${REPO}/blob/main/CONTRIBUTING.md`" target="_blank" rel="noreferrer">
            {{ t('貢獻指南', 'Contributing Guide') }}
          </a>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.oryx {
  color: var(--oryx-fg);
  background: var(--oryx-bg);
}

/* ── 版面骨架 ── */
.ox-section {
  padding: 76px 24px;
  border-top: 1px solid var(--oryx-border);
}
.ox-alt {
  background: var(--oryx-bg-alt);
}
.ox-inner {
  max-width: 1060px;
  margin: 0 auto;
}
.ox-section-head {
  text-align: center;
  margin-bottom: 44px;
}
.ox-label {
  display: inline-block;
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.11em;
  text-transform: uppercase;
  color: var(--oryx-accent-deep);
  background: var(--oryx-accent-soft);
  border-radius: 20px;
  padding: 5px 12px;
  margin-bottom: 14px;
}
.ox-h2 {
  font-size: clamp(22px, 4vw, 31px);
  font-weight: 700;
  letter-spacing: -0.02em;
  line-height: 1.3;
  margin: 0;
  border: 0;
  padding: 0;
}
.ox-p {
  font-size: 15px;
  line-height: 1.75;
  color: var(--oryx-fg-2);
  margin: 14px 0 0;
}
.ox-center {
  max-width: 620px;
  margin-left: auto;
  margin-right: auto;
}
.ox-strong {
  color: var(--oryx-fg);
  font-weight: 600;
}
.ox-center-row {
  display: flex;
  justify-content: center;
  margin-top: 36px;
}

/* ── HERO ── */
.ox-hero {
  padding: 88px 24px 76px;
  text-align: center;
}
.ox-hero-inner {
  max-width: 780px;
  margin: 0 auto;
}
/* VitePress 基礎樣式把 img 設為 display:block，固定寬度時不受 text-align 影響，需顯式置中 */
.ox-logo {
  display: block;
  width: 96px;
  height: 96px;
  margin: 0 auto 22px;
}
.ox-badge {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  font-size: 12px;
  font-weight: 600;
  color: var(--oryx-fg-2);
  background: var(--oryx-bg-alt);
  border: 1px solid var(--oryx-border);
  border-radius: 20px;
  padding: 5px 13px;
  margin-bottom: 20px;
}
.ox-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--oryx-accent);
  animation: ox-pulse 2s ease-in-out infinite;
}
@keyframes ox-pulse {
  0%, 100% { opacity: 1; transform: scale(1); }
  50% { opacity: 0.35; transform: scale(1.35); }
}
.ox-wordmark {
  font-size: clamp(56px, 11vw, 96px);
  font-weight: 900;
  letter-spacing: -0.035em;
  line-height: 1;
  margin: 0 0 16px;
  border: 0;
  padding: 0;
  background: linear-gradient(120deg, var(--oryx-fg) 30%, var(--oryx-accent-deep));
  -webkit-background-clip: text;
  background-clip: text;
  color: transparent;
}
.ox-tagline {
  font-size: 18px;
  color: var(--oryx-fg-2);
  margin: 0 0 14px;
}
.ox-lede {
  font-size: 15px;
  line-height: 1.75;
  color: var(--oryx-fg-3);
  max-width: 620px;
  margin: 0 auto 30px;
}
.ox-cta-row {
  display: flex;
  gap: 10px;
  justify-content: center;
  flex-wrap: wrap;
}
.ox-btn {
  display: inline-block;
  font-size: 14px;
  font-weight: 600;
  padding: 10px 20px;
  border-radius: var(--oryx-r-md);
  border: 1px solid var(--oryx-border-strong);
  color: var(--oryx-fg);
  background: var(--oryx-bg);
  transition: all 0.18s ease;
  text-decoration: none;
}
.ox-btn:hover {
  border-color: var(--oryx-accent);
  color: var(--oryx-accent-deep);
  transform: translateY(-1px);
}
.ox-btn-primary {
  background: var(--oryx-accent-deep);
  border-color: var(--oryx-accent-deep);
  color: #fff;
}
.ox-btn-primary:hover {
  background: var(--oryx-accent);
  border-color: var(--oryx-accent);
  color: #fff;
}
.ox-eco {
  font-size: 12px;
  color: var(--oryx-fg-4);
  margin: 28px 0 0;
  line-height: 1.9;
}

/* ── 現況聲明 ── */
.ox-status-section {
  padding: 40px 24px;
  background: var(--oryx-accent-soft);
}
.ox-status {
  display: flex;
  gap: 16px;
  align-items: flex-start;
  max-width: 860px;
}
.ox-status-mark {
  flex: none;
  width: 26px;
  height: 26px;
  border-radius: 50%;
  background: var(--oryx-accent-deep);
  color: #fff;
  font-weight: 800;
  font-size: 15px;
  display: grid;
  place-items: center;
  margin-top: 2px;
}
.ox-status-title {
  font-size: 16px;
  font-weight: 700;
  margin: 0 0 6px;
}
.ox-status-body {
  font-size: 14px;
  line-height: 1.75;
  color: var(--oryx-fg-2);
  margin: 0;
}

/* ── 問題 ── */
.ox-problem {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 52px;
  align-items: start;
}
.ox-problem-item {
  margin-top: 26px;
}
.ox-problem-q {
  font-size: 15px;
  font-weight: 700;
  margin: 0 0 6px;
  display: flex;
  gap: 10px;
}
.ox-problem-n {
  color: var(--oryx-accent);
  font-variant-numeric: tabular-nums;
  flex: none;
}
.ox-problem-a {
  font-size: 14px;
  line-height: 1.7;
  color: var(--oryx-fg-3);
  margin: 0 0 0 30px;
}
.ox-compare {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.ox-compare-card {
  border: 1px solid var(--oryx-border);
  border-radius: var(--oryx-r-lg);
  background: var(--oryx-bg-alt);
  padding: 20px 22px;
}
.ox-compare-card-ours {
  background: var(--oryx-bg);
  border-color: var(--oryx-accent);
}
.ox-compare-head {
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--oryx-fg-3);
  margin-bottom: 12px;
}
.ox-compare-card-ours .ox-compare-head {
  color: var(--oryx-accent-deep);
}
.ox-compare ul {
  list-style: none;
  padding: 0;
  margin: 0;
}
.ox-compare li {
  display: flex;
  gap: 9px;
  font-size: 13.5px;
  line-height: 1.6;
  color: var(--oryx-fg-2);
  padding: 6px 0;
}
.ox-x {
  color: var(--oryx-fg-4);
  flex: none;
}
.ox-check {
  color: var(--oryx-accent);
  font-weight: 700;
  flex: none;
}

/* ── 架構圖 ── */
/* 架構圖 SVG 的線條與文字為深色，底色固定留白，深色模式下才不會整張看不見 */
.ox-arch {
  width: 100%;
  border: 1px solid var(--oryx-border);
  border-radius: var(--oryx-r-lg);
  background: #ffffff;
  padding: 20px;
}

/* ── 核心能力 ── */
.ox-caps {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 16px;
}
.ox-cap {
  border: 1px solid var(--oryx-border);
  border-radius: var(--oryx-r-xl);
  background: var(--oryx-bg-card);
  padding: 24px;
  transition: border-color 0.18s ease, box-shadow 0.18s ease;
}
.ox-cap:hover {
  border-color: var(--oryx-accent);
  box-shadow: 0 4px 18px rgba(0, 173, 216, 0.08);
}
.ox-cap-no {
  font-size: 13px;
  font-weight: 800;
  letter-spacing: 0.08em;
  color: var(--oryx-accent);
  margin-bottom: 12px;
}
.ox-cap-title {
  font-size: 17px;
  font-weight: 700;
  margin: 0 0 8px;
}
.ox-cap-meta {
  font-family: var(--vp-font-family-mono);
  font-size: 11.5px;
  color: var(--oryx-fg-4);
  margin: 0 0 12px;
  line-height: 1.6;
}
.ox-cap-desc {
  font-size: 13.5px;
  line-height: 1.7;
  color: var(--oryx-fg-2);
  margin: 0;
}

/* ── 憲法 ── */
.ox-articles {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 14px 40px;
}
.ox-article {
  display: flex;
  gap: 14px;
  align-items: flex-start;
  padding: 14px 0;
  border-top: 1px solid var(--oryx-border);
}
.ox-article-n {
  flex: none;
  width: 30px;
  height: 30px;
  border-radius: 50%;
  border: 1px solid var(--oryx-accent);
  color: var(--oryx-accent-deep);
  font-size: 13px;
  font-weight: 700;
  display: grid;
  place-items: center;
}
.ox-article-t {
  font-size: 15px;
  font-weight: 700;
  margin: 3px 0 4px;
}
.ox-article-d {
  font-size: 13.5px;
  line-height: 1.7;
  color: var(--oryx-fg-3);
  margin: 0;
}

/* ── 技術棧 ── */
.ox-stack {
  border: 1px solid var(--oryx-border);
  border-radius: var(--oryx-r-lg);
  overflow: hidden;
}
.ox-stack-row {
  display: grid;
  grid-template-columns: 120px 200px 1fr;
  gap: 18px;
  align-items: center;
  padding: 15px 22px;
  border-bottom: 1px solid var(--oryx-border);
  background: var(--oryx-bg);
}
.ox-stack-row:last-child {
  border-bottom: 0;
}
.ox-stack-k {
  font-size: 14px;
  font-weight: 700;
}
.ox-stack-v {
  font-family: var(--vp-font-family-mono);
  font-size: 12.5px;
  color: var(--oryx-accent-deep);
  background: var(--oryx-accent-soft);
  border-radius: var(--oryx-r-xs);
  padding: 4px 9px;
  white-space: nowrap;
  justify-self: start;
}
.ox-stack-why {
  font-size: 13.5px;
  color: var(--oryx-fg-3);
  line-height: 1.6;
}

/* ── 路線圖 ── */
.ox-roadmap {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 16px;
}
.ox-phase {
  border: 1px solid var(--oryx-border);
  border-radius: var(--oryx-r-lg);
  background: var(--oryx-bg);
  padding: 22px;
}
.ox-phase.is-active {
  border-color: var(--oryx-accent);
}
.ox-phase-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 10px;
}
.ox-phase-t {
  font-size: 16px;
  font-weight: 700;
  margin: 0;
}
.ox-phase-s {
  font-size: 11px;
  font-weight: 700;
  padding: 3px 9px;
  border-radius: 20px;
  background: var(--oryx-bg-alt);
  color: var(--oryx-fg-3);
  border: 1px solid var(--oryx-border);
}
.ox-phase.is-active .ox-phase-s {
  background: var(--oryx-accent-soft);
  color: var(--oryx-accent-deep);
  border-color: var(--oryx-accent);
}
.ox-phase-d {
  font-size: 13.5px;
  line-height: 1.7;
  color: var(--oryx-fg-2);
  margin: 0;
}

/* ── CTA ── */
.ox-cta-section {
  background: var(--oryx-bg-alt);
  text-align: center;
}
.ox-cta-inner {
  max-width: 680px;
}

/* ── 響應式：多欄一律在 860px 塌成單欄 ── */
@media (max-width: 860px) {
  .ox-problem,
  .ox-articles,
  .ox-caps,
  .ox-roadmap {
    grid-template-columns: 1fr;
  }
  .ox-section {
    padding: 52px 20px;
  }
  .ox-hero {
    padding: 64px 20px 52px;
  }
  .ox-stack-row {
    grid-template-columns: 1fr;
    gap: 8px;
  }
}
</style>
