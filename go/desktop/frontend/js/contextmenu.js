// contextmenu.js — a single product-owned right-click surface. WebKit's
// dictionary/search/inspect menu is suppressed everywhere except selected
// text, where Meadow exposes only Copy. File and project rows receive the
// actions that make sense for those objects.
import { daemon, bus, state } from './core.js?v=raven-execution-20261006a';

let menu = null;

function closeMenu() {
  if (menu) menu.remove();
  menu = null;
}

function showError(message) {
  let host = document.getElementById('gitToasts');
  if (!host) {
    host = document.createElement('div');
    host.id = 'gitToasts';
    document.body.appendChild(host);
  }
  const toast = document.createElement('div');
  toast.className = 'git-toast bad';
  toast.textContent = message;
  host.appendChild(toast);
  setTimeout(() => toast.remove(), 6000);
}

function selectedText(target) {
  if (
    target instanceof HTMLInputElement ||
    target instanceof HTMLTextAreaElement
  ) {
    const a = Number(target.selectionStart);
    const b = Number(target.selectionEnd);
    if (Number.isFinite(a) && Number.isFinite(b) && b > a)
      return target.value.slice(a, b);
  }
  return String(window.getSelection()?.toString() || '').trim();
}

async function copyText(text) {
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const box = document.createElement('textarea');
    box.value = text;
    box.style.position = 'fixed';
    box.style.opacity = '0';
    document.body.appendChild(box);
    box.select();
    document.execCommand('copy');
    box.remove();
  }
}

function positionMenu(event) {
  const gap = 8;
  const x = Math.min(event.clientX, window.innerWidth - menu.offsetWidth - gap);
  const y = Math.min(
    event.clientY,
    window.innerHeight - menu.offsetHeight - gap,
  );
  menu.style.left = `${Math.max(gap, x)}px`;
  menu.style.top = `${Math.max(gap, y)}px`;
}

function item(label, action, opts = {}) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'ctx-item' + (opts.danger ? ' danger' : '');
  button.textContent = label;
  button.addEventListener('click', async (event) => {
    event.stopPropagation();
    if (opts.confirm && button.dataset.armed !== '1') {
      button.dataset.armed = '1';
      button.textContent = opts.confirm;
      return;
    }
    closeMenu();
    try {
      await action();
    } catch (error) {
      showError((error && error.message) || String(error));
    }
  });
  return button;
}

function openMenu(event, items) {
  closeMenu();
  menu = document.createElement('div');
  menu.className = 'meadow-context-menu';
  menu.setAttribute('role', 'menu');
  items.forEach((entry) => menu.appendChild(entry));
  document.body.appendChild(menu);
  positionMenu(event);
}

export function initContextMenu() {
  document.addEventListener(
    'contextmenu',
    (event) => {
      event.preventDefault();
      const target =
        event.target instanceof Element ? event.target : document.body;
      const file = target.closest('[data-file-path]');
      if (file && file.dataset.filePath) {
        const path = file.dataset.filePath;
        openMenu(event, [
          item('在 Finder 中顯示', () =>
            daemon.revealPath(path, state.cwd, state.sessionId),
          ),
          item('複製路徑', () => copyText(path)),
        ]);
        return;
      }

      // Conversation rows are nested inside a project group. Resolve them
      // before the project branch so right-clicking a title copies the exact
      // persisted chat/session identifier instead of opening the project
      // menu. `data-sid` is the stable id used by /session/resume; the live
      // resumed id is intentionally not exposed here.
      const session = target.closest('[data-sid]');
      if (session && session.dataset.sid) {
        const sessionId = session.dataset.sid;
        openMenu(event, [item('複製 Session ID', () => copyText(sessionId))]);
        return;
      }

      const project = target.closest('[data-project-cwd]');
      if (project && project.dataset.projectCwd) {
        const cwd = project.dataset.projectCwd;
        const pinned = project.dataset.projectPinned === 'true';
        openMenu(event, [
          item(pinned ? '取消釘選專案' : '釘選專案', () =>
            bus.emit('project:pin', { cwd }),
          ),
          item('在 Finder 中顯示', () =>
            daemon.revealPath(cwd, cwd, state.sessionId),
          ),
          item('編輯專案', () => bus.emit('project:edit', { cwd })),
          item('封存對話', () => bus.emit('project:archive', { cwd })),
          item('移除', () => bus.emit('project:remove', { cwd }), {
            danger: true,
            confirm: '再點一次確認移除',
          }),
        ]);
        return;
      }

      const text = selectedText(target);
      if (text) openMenu(event, [item('複製', () => copyText(text))]);
      else closeMenu();
    },
    true,
  );
  document.addEventListener('pointerdown', (event) => {
    if (menu && !menu.contains(event.target)) closeMenu();
  });
  window.addEventListener('blur', closeMenu);
  window.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') closeMenu();
  });
}
