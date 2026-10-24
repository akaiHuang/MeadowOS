// portfolioDemo.js - presentation-only seeded content for resume iframes.
// Activated only by ?portfolioDemo=1 so the normal Meadow Desktop frontend
// keeps its real first-run and empty-state behavior.
import { state } from './core.js?v=raven-execution-20261006a';

const DEMO_CWD = '/tmp/meadow-uitest-ws/picked';
const SCENES = [
  {
    project: 'Client Briefing',
    thread: 'Executive summary from meeting notes',
    usage: '18.4k tokens - 6 turns',
    requestLabel:
      '幫我把今天的客戶訪談整理成決策摘要，順便列出下週要追的事項。',
    status: 'White-collar workflow',
    summary: '我把訪談整理成三個可交付輸出：決策摘要、待辦清單、英文追蹤信。',
    bullets: [
      '從逐字稿抓出 owner、deadline、decision 和 open question。',
      '把會議內容改成可以直接寄給 stakeholder 的版本。',
      '保留來源脈絡，適合客戶資料不離開本機的工作流。',
    ],
    tools: [
      ['ok', 'read_files', 'meeting_notes.txt, stakeholder.md'],
      ['ok', 'extract_actions', 'owners, dates, decisions'],
      ['run', 'write_file', 'meeting-summary.md'],
      ['wait', 'draft_email', 'client-follow-up-email.md'],
    ],
    outputs: [
      ['meeting-summary.md', '+42', '-4'],
      ['client-follow-up-email.md', '+24', '-2'],
      ['slide-outline.md', '+36', '-6'],
    ],
    diff: [
      ['hunk', '@@ client briefing package @@'],
      [
        'add',
        '+ Decision: prioritize onboarding flow before pricing page rewrite.',
      ],
      [
        'add',
        '+ Action: send stakeholder recap before 18:00 and assign owners.',
      ],
      ['ctx', '  Tone: clear, professional, safe for client-facing work.'],
      [
        'del',
        '- Raw transcript fragments without owner, deadline, or context.',
      ],
    ],
  },
  {
    project: 'Semester Research',
    thread: 'Turn five papers into a report outline',
    usage: '9.8k tokens - 4 turns',
    requestLabel:
      '把這五篇 paper 變成學生期末報告可以用的版本，附上提綱和考前複習卡。',
    status: 'Student study workflow',
    summary:
      '我把文獻壓成報告架構、簡報頁次和 flashcards，讓學生可以從閱讀直接進入輸出。',
    bullets: [
      '每篇 paper 產生研究問題、方法、限制與可引用句。',
      '把長摘要改寫成口頭報告講稿。',
      '把關鍵概念轉成考前複習卡和英文問答練習。',
    ],
    tools: [
      ['ok', 'read_pdf', '5 papers, syllabus, grading rubric'],
      ['ok', 'cluster_claims', 'method, result, limitation'],
      ['run', 'write_outline', 'research-outline.md'],
      ['wait', 'make_cards', 'midterm-flashcards.csv'],
    ],
    outputs: [
      ['research-outline.md', '+58', '-3'],
      ['slide-script.md', '+44', '-5'],
      ['midterm-flashcards.csv', '+64', '-0'],
    ],
    diff: [
      ['hunk', '@@ student research pack @@'],
      [
        'add',
        '+ Research question: how local agents compress study context into action.',
      ],
      ['add', '+ Slide 04: compare memory, latency, and user control.'],
      ['add', '+ Flashcard: define world model in one operational sentence.'],
      ['ctx', '  Output style: student-readable, not paper-like jargon.'],
    ],
  },
  {
    project: 'Interview Coach',
    thread: 'English self-introduction practice',
    usage: '7.1k tokens - 5 turns',
    requestLabel:
      '幫我把中文自我介紹改成英文面試版本，語氣自然一點，順便準備追問。',
    status: 'Interview preparation',
    summary: '我把履歷內容拆成 60 秒自介、STAR 回答和可練習的追問題庫。',
    bullets: [
      '把技術經驗改成面試官聽得懂的影響描述。',
      '把語氣從翻譯腔改成自然英文口說稿。',
      '準備遠端職位常見追問：ownership、debugging、shipping。',
    ],
    tools: [
      ['ok', 'read_resume', 'current portfolio and work order'],
      ['ok', 'rewrite_voice', 'concise spoken English'],
      ['run', 'generate_questions', 'remote AI role interview'],
      ['wait', 'export_plan', 'interview-practice-plan.md'],
    ],
    outputs: [
      ['self-introduction.md', '+35', '-8'],
      ['star-answers.md', '+52', '-11'],
      ['interview-practice-plan.md', '+31', '-3'],
    ],
    diff: [
      ['hunk', '@@ interview coach output @@'],
      [
        'add',
        '+ I build local AI runtimes where model speed, UI feedback, and user trust are treated as one system.',
      ],
      [
        'add',
        '+ Example follow-up: how did you verify the optimization changed user experience?',
      ],
      ['ctx', '  Voice: direct, warm, technical, and interview-ready.'],
      ['del', '- I have many AI experience and do many optimize things.'],
    ],
  },
];

const DEMO_PROJECTS = SCENES.map((scene, index) => ({
  name: scene.project,
  usage: scene.usage,
  threads:
    index === 0
      ? [
          [scene.thread, 'now'],
          ['Competitor memo for Monday standup', '12m'],
          ['Follow-up email in polite English', '1h'],
        ]
      : index === 1
        ? [
            [scene.thread, '24m'],
            ['Make flashcards for the midterm', '2h'],
            ['Rewrite abstract for professor review', '1d'],
          ]
        : [
            [scene.thread, '8m'],
            ['STAR answer polishing', '47m'],
            ['Mock interview anxiety notes', '3h'],
          ],
}));

let sceneIndex = 0;
let phaseIndex = 0;
let timerId = 0;

function isDemo() {
  try {
    const q = new URLSearchParams(window.location.search);
    return (
      q.get('portfolioDemo') === '1' ||
      window.location.hash === '#portfolio-demo'
    );
  } catch {
    return false;
  }
}

export function seedPortfolioDemoProjects() {
  if (!isDemo()) return;
  try {
    localStorage.setItem(
      'meadow.projects',
      JSON.stringify([{ cwd: DEMO_CWD }]),
    );
    localStorage.setItem('meadow.activeCwd', DEMO_CWD);
    localStorage.setItem('meadow.cwd', DEMO_CWD);
    localStorage.setItem(
      'meadow.sidebarSort',
      JSON.stringify({ proj: 'added', thread: 'recent' }),
    );
  } catch {
    /* private mode */
  }
}

function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function fileIcon() {
  return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z"/><path d="M14 2v5a1 1 0 0 0 1 1h5"/><path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/></svg>';
}

function ensureDemoStyles() {
  if (document.getElementById('portfolioDemoStyles')) return;
  const style = document.createElement('style');
  style.id = 'portfolioDemoStyles';
  style.textContent = `
    body.portfolio-demo-mode .demo-fade{animation:demoFade .42s cubic-bezier(.16,1,.3,1) both}
    body.portfolio-demo-mode .thread.demo-hot .t-title,
    body.portfolio-demo-mode .proj.demo-hot .p-name{color:var(--primary2)}
    body.portfolio-demo-mode .thread.demo-hot{box-shadow:inset 0 0 0 1px rgba(74,222,128,.22)}
    body.portfolio-demo-mode .urow{align-items:flex-start}
    body.portfolio-demo-mode .ubub{
      display:grid;grid-template-columns:minmax(0,1fr) auto;align-items:baseline;column-gap:16px;
      height:auto!important;min-height:0!important;padding:10px 16px!important;
    }
    body.portfolio-demo-mode .ubub .ufoot{margin-top:0}
    body.portfolio-demo-mode .demo-typing::after{content:"";display:inline-block;width:.55ch;height:1em;margin-left:2px;background:var(--primary);vertical-align:-2px;animation:caret .53s steps(1) infinite}
    body.portfolio-demo-mode .tool .st.wait{color:var(--faint)}
    body.portfolio-demo-mode .tool .st.run{animation:demoPulse 1.1s ease-in-out infinite}
    body.portfolio-demo-mode .rp-file.demo-new,
    body.portfolio-demo-mode .rp-prow.demo-new{animation:demoFileIn .46s cubic-bezier(.16,1,.3,1) both}
    body.portfolio-demo-mode #composer{box-shadow:0 0 0 1px rgba(74,222,128,.08),0 22px 58px rgba(0,0,0,.22)}
    body.portfolio-demo-mode #input{transition:color .2s}
    body.portfolio-demo-mode .demo-status{
      display:inline-flex;align-items:center;gap:8px;margin:8px 0 0;
      font:11px var(--mono);letter-spacing:.07em;text-transform:uppercase;color:var(--primary2);
    }
    body.portfolio-demo-mode .demo-status i{
      width:6px;height:6px;border-radius:50%;background:var(--primary);
      box-shadow:0 0 0 4px rgba(74,222,128,.12);
    }
    @keyframes demoFade{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:translateY(0)}}
    @keyframes demoPulse{0%,100%{opacity:.55}50%{opacity:1}}
    @keyframes demoFileIn{from{opacity:0;transform:translateY(7px)}to{opacity:1;transform:translateY(0)}}
    @media (prefers-reduced-motion: reduce){
      body.portfolio-demo-mode *{animation-duration:1ms!important;animation-iteration-count:1!important}
    }
  `;
  document.head.appendChild(style);
}

function setScene(nextScene) {
  sceneIndex = (nextScene + SCENES.length) % SCENES.length;
  phaseIndex = 0;
  paintDemo();
}

function activeScene() {
  return SCENES[sceneIndex];
}

function phaseOutputCount() {
  if (phaseIndex < 2) return 0;
  if (phaseIndex === 2) return 1;
  if (phaseIndex === 3) return 2;
  return activeScene().outputs.length;
}

function renderProjectList() {
  const host = document.getElementById('threadList');
  if (!host) return;
  host.innerHTML = DEMO_PROJECTS.map((project, projectIndex) => {
    const activeProject = projectIndex === sceneIndex ? ' active demo-hot' : '';
    const threads = project.threads
      .map(([title, time], threadIndex) => {
        const active =
          projectIndex === sceneIndex && threadIndex === 0
            ? ' active demo-hot'
            : '';
        return `
        <div class="thread${active}" role="button" data-demo-scene="${projectIndex}">
          <div class="t-main"><div class="t-title">${esc(title)}</div></div>
          <div class="t-side"><div class="t-time">${esc(time)}</div><button class="t-arch" title="archive">${fileIcon()}</button></div>
        </div>`;
      })
      .join('');
    return `
      <div class="proj${activeProject}" data-demo-scene="${projectIndex}">
        <div class="proj-head" role="button">
          <span class="p-arrow">›</span>
          <span class="p-folder">${fileIcon()}</span>
          <span class="p-name">${esc(project.name)}</span>
          <span class="p-acts"><button class="p-plus">+</button><button class="p-del">x</button></span>
        </div>
        <div class="proj-usage"><span class="pu-text">${esc(project.usage)}</span><span class="pu-bar"><i style="width:${projectIndex === sceneIndex ? 82 : projectIndex === 1 ? 54 : 42}%"></i></span></div>
        <div class="proj-threads">${threads}</div>
      </div>`;
  }).join('');
  host.querySelectorAll('[data-demo-scene]').forEach((el) => {
    el.addEventListener('click', () => setScene(Number(el.dataset.demoScene)));
  });
}

function userBubble(text, time, typing = false) {
  return `
    <div class="urow demo-fade">
      <div class="ubub${typing ? ' demo-typing' : ''}">
        <div class="utext">${esc(text)}</div>
        <div class="ufoot"><span class="uts">${esc(time)}</span></div>
      </div>
    </div>`;
}

function assistantBlock(scene, time, streaming = false) {
  const body =
    phaseIndex < 4
      ? `
        <h3>${esc(scene.status)}</h3>
        正在把資料轉成可交付文件，先抽取脈絡、再生成輸出。${streaming ? '<span class="caret"></span>' : ''}
      `
      : `
        <h3>${esc(scene.status)}</h3>
        ${esc(scene.summary)}<br><br>
        <b>產品特色</b><br>
        ${scene.bullets.map((b) => `• ${esc(b)}`).join('<br>')}
        <div class="demo-status"><i></i><span>Files ready for review</span></div>
      `;
  return `
    <div class="arow demo-fade">
      <div class="amd">${body}</div>
      <div class="ameta"><span class="ats">${esc(time)} - ${phaseIndex < 4 ? 'running' : '2.1s'}</span><button class="copy-btn">複製</button></div>
    </div>`;
}

function workLog(scene) {
  const activeRows = Math.min(scene.tools.length, Math.max(1, phaseIndex + 1));
  return `
    <div class="worklog demo-fade">
      <div class="wh">LOCAL WORKSPACE ACTIONS</div>
      <div class="wbody">
        ${scene.tools
          .map(([stateName, name, chip], i) => {
            const visible = i < activeRows;
            const nextState =
              i === activeRows - 1 && phaseIndex < 4 ? 'run' : stateName;
            return `
          <div class="tool" style="${visible ? '' : 'opacity:.25'}"><span class="st ${visible ? nextState : 'wait'}">${visible ? '✓' : '·'}</span><span class="nm">${esc(name)}</span><span class="chip">${esc(chip)}</span></div>
        `;
          })
          .join('')}
      </div>
    </div>`;
}

function renderTimeline() {
  const tl = document.getElementById('timeline');
  const scroll = document.getElementById('scroll');
  if (!tl || !scroll) return;
  const scene = activeScene();
  document.body.classList.remove('fresh-thread');
  state.hasMessages = true;
  const promptText =
    phaseIndex === 0
      ? scene.requestLabel.slice(
          0,
          Math.max(8, Math.floor(scene.requestLabel.length * 0.58)),
        )
      : scene.requestLabel;
  const blocks = [userBubble(promptText, '09:42:18', phaseIndex === 0)];
  if (phaseIndex >= 1) blocks.push(workLog(scene));
  if (phaseIndex >= 2)
    blocks.push(assistantBlock(scene, '09:42:22', phaseIndex < 4));
  if (phaseIndex >= 4) {
    blocks.push(`
      <div class="divider demo-fade"><span class="ln"></span><span class="pill">完成 - ${scene.outputs.length} files</span><span class="ln"></span></div>
    `);
  }
  tl.innerHTML = blocks.join('');
  scroll.scrollTop = scroll.scrollHeight;
}

function renderOutputs() {
  const scene = activeScene();
  const count = phaseOutputCount();
  const panel = document.getElementById('rightPanel');
  const outEmpty = document.getElementById('rpOutEmpty');
  const outMain = document.getElementById('rpOutMain');
  const fileList = document.getElementById('rpFileList');
  const filesCount = document.getElementById('rpFilesCount');
  const progFiles = document.getElementById('rpProgFiles');
  const diffPath = document.getElementById('diffPath');
  const diffBody = document.getElementById('diffBody');
  if (!panel || !fileList || !diffBody) return;
  panel.classList.add('open');
  panel.setAttribute('aria-hidden', 'false');
  if (outEmpty) outEmpty.hidden = count > 0;
  if (outMain) outMain.hidden = count === 0;
  if (filesCount) filesCount.textContent = `${count} 個 session 輸出`;
  const shown = scene.outputs.slice(0, count);
  fileList.innerHTML = shown
    .map(
      ([name, add, del], i) => `
    <button class="rp-file${i === 0 ? ' sel' : ''} demo-new" type="button" data-demo-file="${i}">
      <span class="rp-fic">${fileIcon()}</span>
      <span class="rp-fp">${esc(name)}</span>
      <span class="rp-stat"><span class="add">${esc(add)}</span> <span class="del">${esc(del)}</span></span>
    </button>`,
    )
    .join('');
  if (progFiles) {
    progFiles.innerHTML = shown.length
      ? shown
          .map(
            ([name, add, del], i) => `
      <button class="rp-prow demo-new" type="button" data-demo-file="${i}">
        <span class="rp-fic">${fileIcon()}</span>
        <span class="rp-fp">${esc(name)}</span>
        <span class="rp-stat lg"><span class="add">${esc(add)}</span> <span class="del">${esc(del)}</span></span>
      </button>`,
          )
          .join('')
      : '<div class="rp-emptyrow">正在產生輸出</div>';
  }
  if (count === 0) {
    if (diffPath) diffPath.textContent = 'waiting for output';
    diffBody.innerHTML =
      '<div class="dp-note">Meadow is reading local context and preparing files. Outputs appear here as soon as each artifact is ready.</div>';
    return;
  }
  if (diffPath) {
    diffPath.textContent = shown[0][0];
    diffPath.title = shown[0][0];
  }
  diffBody.innerHTML = [
    `<div class="dl meta">diff --generated ${esc(shown[0][0])}</div>`,
    ...scene.diff.map(
      ([kind, text]) => `<div class="dl ${kind}">${esc(text)}</div>`,
    ),
  ].join('');
  fileList.querySelectorAll('[data-demo-file]').forEach((button) => {
    button.addEventListener('click', () => {
      fileList
        .querySelectorAll('.rp-file')
        .forEach((row) => row.classList.remove('sel'));
      button.classList.add('sel');
      const idx = Number(button.dataset.demoFile);
      if (diffPath) diffPath.textContent = shown[idx][0];
    });
  });
}

function renderHeaderState() {
  const scene = activeScene();
  const title = document.getElementById('threadTitle');
  const badge = document.getElementById('projBadge');
  const folder = document.getElementById('pillFolderName');
  const folderAbove = document.getElementById('folderAboveName');
  const input = document.getElementById('input');
  const send = document.getElementById('sendBtn');
  const busy = document.getElementById('busy');
  const tabs = document.getElementById('outTabs');
  const model = document.getElementById('pillModelName');
  const mode = document.getElementById('pillModeName');
  if (title) title.textContent = scene.thread;
  if (badge) {
    badge.hidden = false;
    badge.textContent = scene.project;
  }
  if (folder) folder.textContent = scene.project;
  if (folderAbove) folderAbove.textContent = scene.project;
  if (model) model.textContent = 'Meadow 30B';
  if (mode) mode.textContent = '自動編輯';
  if (input) {
    input.value =
      phaseIndex === 0
        ? scene.requestLabel.slice(
            0,
            Math.max(8, Math.floor(scene.requestLabel.length * 0.58)),
          )
        : '';
    input.placeholder =
      phaseIndex === 0
        ? 'Typing a real work request...'
        : 'Ask Meadow to summarize, rewrite, translate, plan, or make study notes...';
  }
  if (send) send.classList.toggle('armed', phaseIndex === 0);
  if (busy)
    busy.innerHTML =
      phaseIndex === 0
        ? '<span class="sp">●</span> composing request'
        : phaseIndex < 4
          ? '<span class="sp">●</span> running local workspace actions'
          : '<span class="sp">●</span> review ready';
  if (tabs) {
    tabs.innerHTML = activeScene()
      .outputs.slice(0, phaseOutputCount())
      .map(
        ([name]) => `
      <button class="out-tab" type="button"><span class="out-ic">${fileIcon()}</span><span class="out-lb">${esc(name)}</span><span class="out-x">x</span></button>
    `,
      )
      .join('');
  }
}

function paintDemo() {
  renderProjectList();
  renderTimeline();
  renderOutputs();
  renderHeaderState();
}

function tickDemo() {
  phaseIndex += 1;
  if (phaseIndex > 5) {
    sceneIndex = (sceneIndex + 1) % SCENES.length;
    phaseIndex = 0;
  }
  paintDemo();
}

export function initPortfolioDemo() {
  if (!isDemo()) return;
  ensureDemoStyles();
  document.body.classList.add('portfolio-demo-mode');
  const paint = () => paintDemo();
  paint();
  setTimeout(paint, 250);
  setTimeout(paint, 900);
  clearInterval(timerId);
  timerId = window.setInterval(tickDemo, 1600);
}
