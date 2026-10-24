// View-only responsive controller. Keep the existing send/queue/auth handlers.
import { bus } from './core.js?v=raven-execution-20261006a';
import { getTheme, setTheme } from './theme.js';
import { setRightPanelOpen } from './rightpanel.js';
import { initMobileReading } from './mobile_reading.js?v=mobile-reading-20260929a';

let initialized = false;
export function initMobileShell() {
  if (initialized) return;
  initialized = true;
  const mq = window.matchMedia(
    '(max-width: 767px), (max-width: 1024px) and (pointer: coarse)',
  );
  const $ = (id) => document.getElementById(id);
  const sidebar = $('sidebar');
  const main = $('main');
  const panel = $('rightPanel');
  const toggle = $('btnSideToggle');
  let desktopCollapsed = document.body.classList.contains('side-collapsed');
  let previousMobile = false;
  let activeSurface = null;
  let returnFocus = null;
  let revealComposer = () => {};

  const button = (id, text, label) => {
    const node = document.createElement('button');
    if (id) node.id = id;
    node.type = 'button';
    node.className = 'mobile-only';
    node.textContent = text;
    node.setAttribute('aria-label', label || text);
    return node;
  };
  // Use the same controls/handlers in both layouts; only move their DOM nodes.
  const icon = (name) => {
    const node = document.createElement('span');
    node.className = `mobile-only mobile-icon mobile-icon-${name}`;
    node.setAttribute('aria-hidden', 'true');
    return node;
  };
  const more = button('mobileMore', '', '設定');
  more.title = '設定';
  more.append(icon('sliders'));
  more.setAttribute('aria-haspopup', 'dialog');
  const work = button('mobileWorkToggle', '', '成果與進度');
  work.title = '成果與進度';
  work.append($('btnProgress').querySelector('svg').cloneNode(true));
  work.setAttribute('aria-expanded', 'false');
  work.setAttribute('aria-controls', 'rightPanel');
  $('headerRight').append(work, more);
  toggle.append(icon('menu'));
  const heading = document.createElement('strong');
  heading.id = 'mobileHeading';
  heading.className = 'mobile-only';
  heading.textContent = 'Meadow';
  $('header').append(heading);
  const preferences = document.createElement('div');
  preferences.id = 'mobileComposerPreferences';
  preferences.className = 'mobile-only';
  preferences.setAttribute('aria-label', '本次對話設定');
  $('taskQueue').before(preferences);
  $('attachImageBtn').append(icon('plus'));
  const brand = document.createElement('span');
  brand.className = 'mobile-only mobile-nav-brand';
  brand.textContent = 'Meadow Code';
  sidebar.querySelector('.side-head').append(brand);
  const navClose = button('mobileNavClose', '×', '關閉導覽');
  sidebar.querySelector('.side-head').append(navClose);
  const backdrop = button('mobileBackdrop', '', '關閉選單');
  backdrop.tabIndex = -1;
  backdrop.hidden = true;
  const actions = document.createElement('section');
  actions.id = 'mobileActions';
  actions.className = 'mobile-only';
  actions.hidden = true;
  actions.setAttribute('role', 'dialog');
  actions.setAttribute('aria-modal', 'true');
  actions.setAttribute('aria-labelledby', 'mobileActionsTitle');
  const head = document.createElement('div');
  head.className = 'mobile-actions-head';
  const title = document.createElement('h2');
  title.id = 'mobileActionsTitle';
  title.textContent = '工具與設定';
  const close = button('mobileActionsClose', '×', '關閉更多操作');
  head.append(title, close);
  actions.append(head);
  const project = document.createElement('details');
  project.id = 'mobileProjectDetails';
  const projectTitle = document.createElement('summary');
  projectTitle.textContent = '工作區與用量';
  project.append(projectTitle);
  actions.append(project);
  const placements = [
    ['pillMode', preferences],
    ['pillModel', preferences],
    ['folderAbove', project],
    ['checkoutRow', project],
    ['ctxMeter', project],
  ].map(([id, destination]) => {
    const node = $(id);
    const marker = document.createComment(`desktop-position:${id}`);
    node.before(marker);
    return { node, marker, destination };
  });
  function placeControls(mobile) {
    for (const { node, marker, destination } of placements) {
      if (mobile) destination.append(node);
      else marker.after(node);
    }
  }
  const actionTargets = [
    ['新聊天', 'newChatBtn'],
    ['終端機', 'btnTerminal'],
    ['設定', 'btnSettings'],
  ];
  for (const [label, id] of actionTargets) {
    const item = button('', label);
    item.className = 'mobile-action';
    item.dataset.target = id;
    item.addEventListener('click', () => {
      closeActions(false);
      if (id === 'btnSettings') more.focus({ preventScroll: true });
      $(id)?.click();
    });
    actions.append(item);
  }
  const theme = button('', '切換亮色');
  theme.removeAttribute('aria-label');
  theme.className = 'mobile-action';
  theme.addEventListener('click', () => {
    setTheme(getTheme() === 'dark' ? 'light' : 'dark');
    theme.textContent = getTheme() === 'dark' ? '切換亮色' : '切換暗色';
  });
  actions.append(theme);
  document.body.append(backdrop, actions);
  // Workspace/usage controls move into this sheet on phones. Keep an explicit
  // route to it while the header settings button retains its direct action.
  const tools = button('mobileTools', '工具與工作區');
  tools.setAttribute('aria-haspopup', 'dialog');
  tools.setAttribute('aria-controls', 'mobileActions');
  tools.setAttribute('aria-expanded', 'false');
  $('sideFoot').prepend(tools);
  tools.addEventListener('click', () => {
    collapse(true);
    setRightPanelOpen(false);
    actions.hidden = false;
    theme.textContent = getTheme() === 'dark' ? '切換亮色' : '切換暗色';
    sync(false);
  });

  function collapse(value) {
    window.__setMeadowSidebarCollapsed?.(value);
  }
  function closeActions(restore = true) {
    actions.hidden = true;
    sync(restore);
  }
  function sync(restore = true) {
    const mobile = mq.matches;
    tools.setAttribute('aria-expanded', String(mobile && !actions.hidden));
    const navOpen =
      mobile && !document.body.classList.contains('side-collapsed');
    if (navOpen && panel.classList.contains('open')) setRightPanelOpen(false);
    const panelOpen = mobile && panel.classList.contains('open');
    // Work is below the persistent toolbar; its toggle remains reachable.
    const workOpen = mobile && panelOpen;
    if (document.body.classList.contains('mobile-work-open') !== workOpen)
      document.body.classList.toggle('mobile-work-open', workOpen);
    work.setAttribute('aria-expanded', String(workOpen));
    work.setAttribute(
      'aria-label',
      workOpen ? '關閉成果與進度，返回對話' : '成果與進度',
    );
    $('scroll').inert = workOpen;
    $('composerWrap').inert = workOpen;
    const nextSurface = mobile
      ? !actions.hidden
        ? actions
        : navOpen
          ? sidebar
          : null
      : null;
    // Capture the opener before making its parent inert (which blurs it).
    if (nextSurface && !activeSurface) returnFocus = document.activeElement;
    backdrop.hidden = !mobile || (!navOpen && actions.hidden);
    sidebar.inert = mobile && !navOpen;
    sidebar.setAttribute('aria-hidden', String(mobile && !navOpen));
    if (navOpen) {
      sidebar.setAttribute('role', 'dialog');
      sidebar.setAttribute('aria-modal', 'true');
      sidebar.setAttribute('aria-label', '對話與專案');
    } else {
      sidebar.removeAttribute('role');
      sidebar.removeAttribute('aria-modal');
    }
    main.inert = Boolean(nextSurface);
    panel.inert = mobile && (!panelOpen || navOpen || !actions.hidden);
    if (nextSurface !== activeSurface) {
      if (nextSurface) {
        const focusTarget =
          nextSurface === sidebar
            ? navClose
            : nextSurface === actions
              ? close
              : $('diffClose');
        // Wait for the drawer's visibility and inert updates to be applied.
        requestAnimationFrame(() => {
          if (activeSurface === nextSurface)
            focusTarget?.focus({ preventScroll: true });
        });
      } else if (restore && activeSurface) {
        // A sidebar action may already have opened and focused another dialog.
        // Closing this drawer must not steal focus from that new owner.
        const focused = document.activeElement;
        const focusMovedElsewhere =
          focused &&
          focused !== document.body &&
          !activeSurface.contains(focused) &&
          !focused.closest('[hidden], [inert]');
        if (!focusMovedElsewhere) {
          const target =
            returnFocus?.isConnected && returnFocus.getClientRects().length
              ? returnFocus
              : toggle;
          target?.focus({ preventScroll: true });
        }
      }
      activeSurface = nextSurface;
    }
  }
  function resize() {
    $('diffClose').textContent = '✕';
    $('diffClose').setAttribute('aria-label', '關閉右側面板');
    if (mq.matches !== previousMobile) {
      placeControls(mq.matches);
      if (mq.matches) {
        desktopCollapsed = document.body.classList.contains('side-collapsed');
        collapse(true);
      } else {
        actions.hidden = true;
        collapse(desktopCollapsed);
      }
      previousMobile = mq.matches;
    }
    const vv = window.visualViewport;
    // Ignore pinch zoom: users retain browser magnification and panning.
    if (mq.matches && (!vv || vv.scale === 1)) {
      document.body.style.setProperty(
        '--mobile-vv-height',
        `${Math.round(vv?.height || window.innerHeight)}px`,
      );
      document.body.style.setProperty(
        '--mobile-vv-top',
        `${Math.round(vv?.offsetTop || 0)}px`,
      );
      // The keyboard/browser may already cover the layout viewport's safe area.
      // Only reserve the part still inside the visible viewport, never both.
      const coveredBottom = vv
        ? Math.max(
            0,
            document.documentElement.clientHeight - vv.height - vv.offsetTop,
          )
        : 0;
      document.body.style.setProperty(
        '--mobile-bottom-occlusion',
        `${Math.round(coveredBottom)}px`,
      );
    }
    sync();
  }
  navClose.addEventListener('click', () => collapse(true));
  work.addEventListener('click', () => {
    collapse(true);
    setRightPanelOpen(!panel.classList.contains('open'));
  });
  close.addEventListener('click', () => closeActions());
  backdrop.addEventListener('click', () => {
    closeActions();
    collapse(true);
  });
  more.addEventListener('click', () => {
    collapse(true);
    setRightPanelOpen(false);
    closeActions(false);
    // Safari taps need not focus buttons. Keep the visible opener as the
    // settings modal's return target, instead of the hidden desktop button.
    more.focus({ preventScroll: true });
    $('btnSettings')?.click();
  });
  bus.on('thread:switched', () => {
    revealComposer();
    if (mq.matches) collapse(true);
  });
  sidebar.addEventListener('click', (event) => {
    if (
      mq.matches &&
      event.target.closest('#newChatBtn, #sideSearch button, #sideFoot button')
    )
      collapse(true);
  });
  document.addEventListener('keydown', (event) => {
    if (!mq.matches || !activeSurface) return;
    // A nested settings/account/palette dialog owns its own keyboard handling.
    if (
      document.activeElement !== document.body &&
      !activeSurface.contains(document.activeElement)
    )
      return;
    if (event.key === 'Escape') {
      event.preventDefault();
      if (activeSurface === actions) closeActions();
      else if (activeSurface === sidebar) collapse(true);
      else setRightPanelOpen(false);
    }
    if (event.key === 'Tab') {
      const items = [
        ...activeSurface.querySelectorAll(
          'button, a[href], input, select, textarea, [tabindex="0"]',
        ),
      ].filter(
        (node) =>
          !node.disabled &&
          node.getClientRects().length &&
          !node.closest('[hidden], [inert]'),
      );
      const first = items[0];
      const last = items.at(-1);
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last?.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first?.focus();
      }
    }
  });
  new MutationObserver(() => sync()).observe(document.body, {
    attributes: true,
    attributeFilter: ['class'],
  });
  document.addEventListener('rightpanel:layout', () => {
    if (mq.matches && panel.classList.contains('open')) {
      actions.hidden = true;
      collapse(true);
    }
    sync();
  });
  mq.addEventListener('change', resize);
  window.addEventListener('resize', resize);
  window.visualViewport?.addEventListener('resize', resize);
  window.visualViewport?.addEventListener('scroll', resize);
  resize();
  revealComposer = initMobileReading(mq);
  // Publish readiness only after the first mobile collapse and inert sync.
  document.body.classList.add('mobile-shell-ready');
}
