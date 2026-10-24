/* native-background-ui */
// Single owner for the drawer's one/two-pane layout. Content stays mounted.
const $ = (id) => document.getElementById(id);
const ratioKey = 'meadow.rpSplitRatio';
let selected = ['rpPageOut'];
let desktopSelection = null;
let ratio = 0.5;
let divider;
let dragging = false;
let userSelected = false;

function notify() {
  document.dispatchEvent(new Event('rightpanel:layout'));
}

export function setRightPanelOpen(open) {
  const panel = $('rightPanel');
  if (!panel) return;
  panel.classList.toggle('open', open);
  panel.setAttribute('aria-hidden', String(!open));
  $('btnDiff')?.classList.toggle('on', open);
  notify();
}

function paint() {
  const wrap = $('rpWrap');
  if (!wrap) return;
  selected = selected.filter((id) => $(id)).slice(0, 2);
  if (!selected.length) selected = ['rpPageOut'];
  const compact = window.matchMedia(
    '(max-width: 767px), (max-width: 1024px) and (pointer: coarse)',
  ).matches;
  if (compact) selected = selected.slice(-1);
  $('rpTabs').setAttribute('aria-multiselectable', String(!compact));
  const split = selected.length === 2;
  wrap.classList.toggle('rp-split', split);
  wrap.style.setProperty('--rp-top', `${ratio}fr`);
  wrap.style.setProperty('--rp-bottom', `${1 - ratio}fr`);
  for (const page of wrap.querySelectorAll('.rp-page')) {
    const index = selected.indexOf(page.id);
    page.classList.toggle('on', index !== -1);
    page.setAttribute('aria-hidden', String(index === -1));
    page.style.gridRow = index === 1 ? '4' : '2';
  }
  for (const tab of $('rpTabs').querySelectorAll('.rp-tab')) {
    const index = selected.indexOf(tab.getAttribute('aria-controls'));
    tab.classList.toggle('on', index !== -1);
    tab.setAttribute('aria-selected', String(index !== -1));
    tab.dataset.pane = index === -1 ? '' : String(index + 1);
    tab.title =
      index === -1
        ? split
          ? '在下方面板顯示'
          : compact
            ? '切換面板'
            : '加入第二個面板'
        : split
          ? `${index === 0 ? '上方' : '下方'}面板（點擊收起）`
          : '目前面板';
  }
  if (divider) {
    divider.hidden = !split;
    divider.setAttribute('aria-valuenow', String(Math.round(ratio * 100)));
    divider.setAttribute('aria-controls', selected.join(' '));
  }
  notify();
}

export function showRightPane(id, { open = true, automatic = false } = {}) {
  if (!$(id)) return;
  if (automatic && !$('rightPanel')?.classList.contains('open')) return;
  if (automatic && userSelected && !selected.includes(id)) return;
  if (!selected.includes(id)) selected = [selected[0], id].filter(Boolean);
  paint();
  if (open) setRightPanelOpen(true);
}

export function resetRightPanes() {
  desktopSelection = null;
  selected = ['rpPageOut'];
  userSelected = false;
  paint();
}

export function hasUserRightPaneSelection() {
  return userSelected;
}

export function hideRightPane(id) {
  if (desktopSelection)
    desktopSelection = desktopSelection.filter((value) => value !== id);
  selected = selected.filter((value) => value !== id);
  paint();
}

export function toggleRightPane(id) {
  userSelected = true;
  if (selected.includes(id)) {
    if (selected.length === 2) hideRightPane(id);
  } else showRightPane(id);
  setRightPanelOpen(true);
  return selected.includes(id);
}

export function initRightPanel() {
  const wrap = $('rpWrap');
  if (!wrap || divider) return;
  try {
    const saved = Number(localStorage.getItem(ratioKey));
    if (saved >= 0.1 && saved <= 0.9) ratio = saved;
  } catch {
    /* private storage */
  }
  $('rpTabs').setAttribute('aria-multiselectable', 'true');
  window
    .matchMedia('(max-width: 767px), (max-width: 1024px) and (pointer: coarse)')
    .addEventListener('change', ({ matches }) => {
      if (matches) desktopSelection = selected.slice();
      else if (desktopSelection) {
        selected = desktopSelection;
        desktopSelection = null;
      }
      paint();
    });
  divider = document.createElement('div');
  divider.id = 'rpSplitResizer';
  divider.tabIndex = 0;
  divider.setAttribute('role', 'separator');
  divider.setAttribute('aria-label', '調整上下視窗大小');
  divider.setAttribute('aria-orientation', 'horizontal');
  divider.setAttribute('aria-valuemin', '10');
  divider.setAttribute('aria-valuemax', '90');
  divider.title = '拖拉調整上下視窗；雙擊恢復各半（方向鍵亦可調整）';
  wrap.appendChild(divider);
  const apply = (next) => {
    const available = wrap.clientHeight - $('rpHead').offsetHeight - 8;
    const minimum = Math.min(0.5, Math.max(0.1, 100 / Math.max(1, available)));
    ratio = Math.max(minimum, Math.min(1 - minimum, next));
    divider.setAttribute('aria-valuemin', String(Math.round(minimum * 100)));
    divider.setAttribute(
      'aria-valuemax',
      String(Math.round((1 - minimum) * 100)),
    );
    paint();
  };
  const persist = () => {
    try {
      localStorage.setItem(ratioKey, String(ratio));
    } catch {
      /* private storage */
    }
  };
  const end = () => {
    if (!dragging) return;
    dragging = false;
    wrap.classList.remove('rp-dragging');
    persist();
    notify();
  };
  divider.addEventListener('pointerdown', (event) => {
    if (event.button !== 0) return;
    event.preventDefault();
    divider.focus();
    divider.setPointerCapture(event.pointerId);
    dragging = true;
    wrap.classList.add('rp-dragging');
    notify();
  });
  divider.addEventListener('pointermove', (event) => {
    if (!dragging) return;
    const start = $('rpHead').getBoundingClientRect().bottom;
    const available = wrap.getBoundingClientRect().bottom - start - 8;
    apply((event.clientY - start - 4) / Math.max(1, available));
  });
  for (const type of ['pointerup', 'pointercancel', 'lostpointercapture'])
    divider.addEventListener(type, end);
  window.addEventListener('blur', end);
  divider.addEventListener('dblclick', () => {
    apply(0.5);
    persist();
  });
  divider.addEventListener('keydown', (event) => {
    const values = {
      ArrowUp: ratio - 0.05,
      ArrowDown: ratio + 0.05,
      Home: 0.1,
      End: 0.9,
    };
    if (!(event.key in values)) return;
    event.preventDefault();
    apply(values[event.key]);
    persist();
  });
  new ResizeObserver(() => {
    if (wrap.clientHeight) apply(ratio);
  }).observe(wrap);
  paint();
}
