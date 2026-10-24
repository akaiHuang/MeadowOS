// main.js — boot sequence: model status polling → projects store
// (localStorage "meadow.projects", legacy "meadow.cwd" migration, first-run
// folder picker) → createSession on the active project → SSE → modules.
import './theme.js';
import {
  daemon,
  ensureSession,
  bus,
  state,
  assertClientAuthenticated,
  hasClientAccess,
  clientAccountGeneration,
  installAccountRequestGuard,
} from './core.js?v=raven-execution-20261006a';
import { initAccountAccessGuard } from './account_access.js';
import {
  initSidebar,
  loadProjects,
  saveProjects,
  getStoredActive,
  setActiveCwd,
  canonicalizeActive,
} from './sidebar.js';
import { initTimeline } from './timeline.js?v=nearby-google-link-20260930a';
// Keep this module's URL revisioned with the shell. WKWebView can retain an
// earlier composer module across a same-bundle-id Desktop upgrade, leaving the
// visible UI present but its send listeners disconnected.
import { initComposer } from './composer.js?v=raven-execution-20261006a';
import { initHarnessControls } from './harness_controls.js?v=raven-execution-20261006a';
import { authorizeNewConversation } from './commercial.js?v=desktop-send-20260904a';
import { initTerminal } from './terminal.js';
import { initGitbar } from './gitbar.js';
import { initDiffPanel } from './diffpanel.js?v=desktop-conversation-save-20260909a';
import { initRightPanel } from './rightpanel.js';
import { initPreview } from './preview.js?v=desktop-conversation-save-20260909a';
import { initBrowserDrawer } from './browserDrawer.js?v=desktop-browser-drawer-20260904c';
import { initExpertLens } from './expertlens.js';
import { initPalette } from './palette.js';
import { initOutputTabs } from './outputtabs.js?v=desktop-conversation-save-20260909a';
import { initSettings } from './settings.js?v=welcome-benefits-20260928a';
import { initWorkCenter } from './workcenter.js?v=task-todo-20261004b';
import { initContextMenu } from './contextmenu.js';
import { initForegroundState } from './foregroundstate.js';
import { initMobileShell } from './mobile.js?v=mobile-reading-20260929a';
import {
  commercialSessionReady,
  requireAuthenticatedSession,
  requestAccountLogin,
} from './commercial.js?v=desktop-send-20260904a';
import { showProductOnboarding } from './product_experience.js?v=welcome-benefits-20260928a';
import {
  initPortfolioDemo,
  seedPortfolioDemoProjects,
} from './portfolioDemo.js';

const $ = (sel) => document.querySelector(sel);
const CWD_KEY = 'meadow.cwd'; // legacy key; composer folder pill writes it then reloads

function initHoverDiagnostics() {
  const params = new URLSearchParams(window.location.search);
  if (!params.has('hoverDebug')) return;

  const trace = [];
  const panel = document.createElement('pre');
  panel.id = 'hoverDebugOverlay';
  panel.setAttribute('aria-label', '原生 hover 事件紀錄');
  panel.style.cssText = [
    'position:fixed',
    'left:12px',
    'top:60px',
    'z-index:2147483647',
    'width:min(620px,calc(100vw - 24px))',
    'max-height:190px',
    'overflow:auto',
    'margin:0',
    'padding:10px 12px',
    'border:1px solid rgba(99,235,157,.5)',
    'border-radius:10px',
    'background:rgba(7,12,9,.94)',
    'color:#75efa8',
    'font:11px/1.45 ui-monospace,monospace',
    'pointer-events:none',
    'white-space:pre-wrap',
  ].join(';');
  panel.textContent = 'HOVER DEBUG BUILD · waiting for native pointer events';
  document.body.appendChild(panel);
  let lastSignature = '';
  const describe = (node) => {
    if (!(node instanceof Element)) return String(node?.nodeName || 'none');
    const id = node.id ? `#${node.id}` : '';
    const classes = [...node.classList]
      .slice(0, 3)
      .map((name) => `.${name}`)
      .join('');
    return `${node.tagName.toLowerCase()}${id}${classes}`;
  };
  const record = (event) => {
    const hit = document.elementFromPoint(event.clientX, event.clientY);
    const path = event.composedPath?.().slice(0, 5).map(describe) || [];
    const signature = `${event.type}|${describe(event.target)}|${describe(hit)}`;
    if (event.type === 'pointermove' && signature === lastSignature) return;
    lastSignature = signature;
    const entry = {
      type: event.type,
      x: Math.round(event.clientX),
      y: Math.round(event.clientY),
      target: describe(event.target),
      hit: describe(hit),
      path,
      buttons: event.buttons ?? 0,
      pickerOpen: !document.getElementById('modelModal')?.hidden,
    };
    trace.push(entry);
    if (trace.length > 200) trace.shift();
    panel.textContent = trace
      .slice(-8)
      .map(
        (item, index) =>
          `${String(trace.length - Math.min(8, trace.length) + index + 1).padStart(3, '0')} ${item.type.padEnd(12)} target=${item.target} hit=${item.hit} buttons=${item.buttons}`,
      )
      .join('\n');
    console.log('[hover-debug]', entry);
    void fetch('/native/hover-debug', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(entry),
    }).catch(() => {});
  };

  Object.defineProperty(window, '__meadowHoverTrace', {
    configurable: true,
    get: () => trace.slice(),
  });
  for (const type of [
    'pointerover',
    'pointermove',
    'mouseover',
    'mouseenter',
  ]) {
    document.addEventListener(type, record, true);
  }
  console.log('[hover-debug] capture listeners ready', {
    pointer: window.matchMedia('(hover: hover) and (pointer: fine)').matches,
    userAgent: navigator.userAgent,
  });
}

const shortName = (p) => (p || '').split('/').filter(Boolean).pop() || p || '';
// Poll /model/status into the sidebar footer; re-broadcast on the bus so
// other modules (composer model pill) can follow along.
function startModelPolling() {
  const tick = async () => {
    if (!hasClientAccess()) return;
    const generation = clientAccountGeneration();
    try {
      const s = await daemon.modelStatus();
      if (!hasClientAccess() || generation !== clientAccountGeneration()) return;
      // The active thread owns its selected model. Global resident-model
      // polling may report another background thread's model and must not
      // silently rewrite this conversation's selection.
      if (!state.model && s.model) state.model = s.model;
      bus.emit('model_status', s);
    } catch {
      if (!hasClientAccess() || generation !== clientAccountGeneration()) return;
      bus.emit('model_status', {
        status: 'error',
        model: '',
        detail: 'daemon 離線',
      });
    }
  };
  tick();
  setInterval(tick, 2000);
}

// First-run card in #pendingArea; resolves with the picked folder path.
function promptForFolder() {
  return new Promise((resolve) => {
    const card = document.createElement('div');
    card.className = 'card';
    card.innerHTML = `
      <div class="card-title">選擇第一個專案資料夾</div>
      <div class="card-body">Meadow 會在這個資料夾內讀寫檔案與執行指令，之後可在側欄加入更多專案。</div>
      <div class="card-actions"><button class="btn primary">瀏覽資料夾…</button></div>`;
    card.querySelector('button').addEventListener('click', async () => {
      try {
        const r = await daemon.pickFolder();
        if (r && r.path) {
          card.remove();
          resolve(r.path);
        }
      } catch {
        /* daemon offline — keep the card so the user can retry */
      }
    });
    $('#pendingArea').appendChild(card);
  });
}

// Decide the project list + active cwd from storage. Handles three states:
// fresh install (no keys), legacy single "meadow.cwd" (migrate into the
// projects list), and a composer folder-pill pick before reload
// ("meadow.cwd" ≠ activeCwd → adopt it as a project and activate it).
function resolveProjects() {
  const projects = loadProjects();
  let legacy = '';
  try {
    legacy = localStorage.getItem(CWD_KEY) || '';
  } catch {
    /* private mode */
  }
  if (legacy && !projects.some((p) => p.cwd === legacy)) {
    projects.push({ cwd: legacy });
    saveProjects(projects);
  }
  let active = getStoredActive();
  if (legacy && legacy !== active) active = legacy; // newest explicit pick wins
  if (!projects.some((p) => p.cwd === active))
    active = projects.length ? projects[0].cwd : '';
  return { projects, active };
}

let shellInitialized = false;
let workspaceSelection = null;
let workspaceStart = null;
let automationWorkspace = '';
let automationWorkspaceResolved = false;
let authorizedShellStart = null;

function initProductShell() {
  if (shellInitialized) return;
  shellInitialized = true;
  initSidebar();
  initTimeline();
  initComposer();
  initHarnessControls();
  initTerminal();
  initGitbar();
  initRightPanel();
  initDiffPanel();
  initWorkCenter();
  initContextMenu();
  initPreview();
  initBrowserDrawer();
  initExpertLens();
  initPalette();
  initOutputTabs();
  initSettings();
  initPortfolioDemo();
  initMobileShell();
}

async function resolveAutomationWorkspace() {
  // Automated Desktop runs use a stable Application Support workspace. This
  // opt-in path avoids macOS Desktop/Documents permission prompts and lets
  // Computer Use or Playwright relaunch the real app without clicking a folder
  // picker on every build. Normal product launches receive an empty response.
  try {
    const bootstrap = await daemon.workspaceBootstrap();
    automationWorkspaceResolved = true;
    if (bootstrap && bootstrap.automation && bootstrap.path) {
      automationWorkspace = bootstrap.path;
      // Automation must not rehydrate previously opened Desktop/Documents
      // projects. Merely rendering those stale rows makes WebKit inspect the
      // protected folders and causes macOS to ask for access on every rebuilt
      // app. Keep the automated run in its single Application Support
      // workspace; normal launches still preserve the full project list.
      saveProjects([{ cwd: automationWorkspace }]);
      setActiveCwd(automationWorkspace);
    }
  } catch {
    // Older service versions do not expose the automation bootstrap contract.
  }
}

async function ensureWorkspaceSelected() {
  await assertClientAuthenticated();
  if (state.cwd) return state.cwd;
  if (workspaceSelection) return workspaceSelection;
  workspaceSelection = (async () => {
    let { projects, active } = resolveProjects();
    if (automationWorkspace) {
      projects = [{ cwd: automationWorkspace }];
      active = automationWorkspace;
    }
    if (!projects.length) {
      // The product introduction and account surface remain usable without a
      // folder. A native picker is only required when the user is actually
      // ready to start a workspace session.
      const cwd = await promptForFolder();
      projects = [{ cwd }];
      saveProjects(projects);
      active = cwd;
    }
    if (!active) active = projects[0]?.cwd || '';
    if (active) setActiveCwd(active);
    return active;
  })();
  try {
    return await workspaceSelection;
  } finally {
    workspaceSelection = null;
  }
}

async function startAuthorizedWorkspace() {
  if (workspaceStart) return workspaceStart;
  workspaceStart = (async () => {
    if (!(await requireAuthenticatedSession({ prompt: false }))) return false;
    if (state.sessionId) return true;
    // Protected first launch can reject workspace bootstrap until login has
    // completed. Retry that read-only discovery once authorization exists so
    // automation remains in its stable Application Support workspace.
    if (!automationWorkspaceResolved) await resolveAutomationWorkspace();
    const active = await ensureWorkspaceSelected();
    if (!active) return false;

    // agentd deliberately exposes its socket before a large cold model has
    // finished converging. A one-shot request can race that cold startup, so
    // retry only transient transport failures. Authentication and membership
    // denials remain user-visible and are never converted into retries.
    for (let attempt = 0; attempt < 60 && !state.sessionId; attempt += 1) {
      try {
        const authorization = await authorizeNewConversation();
        const session = await ensureSession(active, authorization);
        if (session.workspaceCwd && session.workspaceCwd !== state.cwd)
          canonicalizeActive(session.workspaceCwd);
      } catch (error) {
        const status = Number(error?.status || 0);
        const transient =
          !error?.userMessage &&
          (status === 0 || status === 502 || status === 503 || status === 504);
        if (!transient || attempt === 59) {
          console.error('createSession failed:', error);
          return false;
        }
        await new Promise((resolve) => setTimeout(resolve, 1000));
      }
    }
    if (state.sessionId) commercialSessionReady();
    return Boolean(state.sessionId);
  })();
  try {
    return await workspaceStart;
  } finally {
    workspaceStart = null;
  }
}

async function activateAuthorizedShell() {
  if (!hasClientAccess()) return false;
  if (!authorizedShellStart) authorizedShellStart = (async () => {
    initForegroundState();
    initHoverDiagnostics();
    startModelPolling();
    seedPortfolioDemoProjects();
    const { active } = resolveProjects();
    if (active && !state.cwd) setActiveCwd(active);
    initProductShell();
    await showProductOnboarding();
  })();
  await authorizedShellStart;
  return hasClientAccess() ? startAuthorizedWorkspace() : false;
}

async function boot() {
  installAccountRequestGuard();
  initAccountAccessGuard({ isAllowed: hasClientAccess, onDenied: requestAccountLogin });
  // Only the parsed private admission state unlocks the shell. An event's
  // public payload is never an authorization source.
  window.addEventListener('commercial:change', () => {
    if (hasClientAccess()) void activateAuthorizedShell();
  });
  window.MeadowDesktop = Object.freeze({
    ensureWorkspaceSelected,
    startAuthorizedWorkspace,
  });
  const authenticated = await requireAuthenticatedSession({ prompt: true });
  if (authenticated) await activateAuthorizedShell();
}

boot();
