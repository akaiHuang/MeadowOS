// product_experience.js — first-run product introduction only.
//
// This module deliberately owns no authentication, trial, entitlement or
// purchase state. Those remain server-authoritative in commercial.js. The only
// persisted value here is a versioned, non-security UI preference recording
// that the introduction has already been seen.

import { createBenefitsScene } from './ascii_benefits.js?v=20260926c';
import { getTheme, setTheme } from './theme.js';

const ONBOARDING_KEY = 'meadow.product-experience.onboarding.v2';
const SEEN_VALUE = 'seen';
const $ = (selector, root = document) => root.querySelector(selector);

const slides = Object.freeze([
  {
    eyebrow: '創作加乘',
    scene: 'create',
    artLabel: 'ASCII 樹從筆電萌芽，葉片輕輕搖曳',
    title: '把想法，\n變成作品。',
    copy: '從一點靈感到完成作品，讓 Meadow 為你的創作添一把力。',
  },
  {
    eyebrow: '把時間還給你',
    scene: 'time',
    artLabel: 'ASCII 時鐘與三張完成的待辦，指針緩緩轉動',
    title: '日常瑣碎，\n交給我試試。',
    copy: '整理資料、查找資訊、處理待辦，把時間留給你更想做的事。',
  },
  {
    eyebrow: '留一點空間給自己',
    scene: 'calm',
    artLabel: 'ASCII 思緒舒展成葉片，水面漣漪輕輕起伏',
    title: '有煩惱，\n就來聊聊。',
    copy: '聊聊心事、整理思緒，不必急著找到答案，先給自己一點喘息空間。',
  },
  {
    eyebrow: '自在開口',
    scene: 'talk',
    artLabel: 'ASCII 對話框寫著 Hello 與你好，字元柔和浮現',
    title: '想怎麼聊，\n先陪你練。',
    copy: '從全英聊天到破冰、社交與戀愛話題，陪你練習表達，找到自己的說法。',
  },
]);

function hasSeenOnboarding() {
  try {
    return localStorage.getItem(ONBOARDING_KEY) === SEEN_VALUE;
  } catch {
    return false;
  }
}

function markOnboardingSeen() {
  try {
    localStorage.setItem(ONBOARDING_KEY, SEEN_VALUE);
  } catch {
    // Private browsing may reject persistence. The current interaction can
    // still finish normally; the tour may reappear on a later launch.
  }
}

function focusable(root) {
  return [
    ...root.querySelectorAll(
      'button:not([disabled]):not([hidden]), [href], [tabindex]:not([tabindex="-1"])',
    ),
  ].filter((node) => node.getClientRects().length > 0);
}

function createOnboarding() {
  const root = document.createElement('section');
  root.id = 'meadowOnboarding';
  root.className = 'meadow-onboarding-overlay';
  root.dataset.tour = 'benefits';
  root.setAttribute('role', 'dialog');
  root.setAttribute('aria-modal', 'true');
  root.setAttribute('aria-labelledby', 'meadowOnboardingTitle-0');
  root.tabIndex = -1;
  root.dataset.step = '0';
  root.innerHTML = `
    <div class="meadow-onboarding-card">
      <header class="meadow-onboarding-masthead">
        <strong>Meadow</strong>
        <div class="meadow-onboarding-header-actions">
          <button id="onboardingThemeToggle" type="button" aria-label="切換亮色" title="切換亮色"><span aria-hidden="true">☼</span></button>
          <button id="onboardingClose" class="meadow-onboarding-close" type="button" aria-label="關閉特色介紹"></button>
        </div>
      </header>
      <div class="meadow-onboarding-slides">
        ${slides
          .map(
            (slide, index) => `
              <article class="meadow-onboarding-slide" data-onboarding-step="${index}" aria-hidden="${index === 0 ? 'false' : 'true'}">
                <div class="meadow-onboarding-copy">
                  <p><span class="meadow-onboarding-number"><b>0${index + 1}</b> / 04</span>${slide.eyebrow}</p>
                  <h2 id="meadowOnboardingTitle-${index}">${slide.title.replaceAll('\n', '<br />')}</h2>
                </div>
                <div class="meadow-onboarding-visual"><canvas data-ascii-scene="${slide.scene}" role="img" aria-label="${slide.artLabel}"></canvas></div>
                <p class="meadow-onboarding-description">${slide.copy}</p>
              </article>`,
          )
          .join('')}
      </div>
      <footer class="meadow-onboarding-nav">
        <div id="onboardingDots" class="meadow-onboarding-dots" aria-label="特色介紹進度"></div>
        <button id="onboardingNext" class="meadow-onboarding-next" type="button"><span>繼續</span><span aria-hidden="true">→</span></button>
      </footer>
    </div>`;
  return root;
}

/**
 * Show the first-run introduction.
 * @returns {'skipped'|'dismissed'|'login'}
 */
export function showProductOnboarding({
  force = false,
  returnFocus = null,
} = {}) {
  const existing = $('#meadowOnboarding');
  if (existing?.__resultPromise) return existing.__resultPromise;
  if (!force && hasSeenOnboarding()) return Promise.resolve('skipped');

  const previouslyFocused =
    returnFocus instanceof HTMLElement ? returnFocus : document.activeElement;
  const root = createOnboarding();
  let step = 0;
  let resolveResult;
  const resultPromise = new Promise((resolve) => {
    resolveResult = resolve;
  });
  root.__resultPromise = resultPromise;
  document.body.append(root);
  // The shell can finish mounting while this view-only tour is already open.
  // Do not let its composer autofocus move keyboard input behind the dialog.
  const keepFocusInside = (event) => {
    if (root.isConnected && !root.contains(event.target)) {
      root.focus({ preventScroll: true });
    }
  };
  document.addEventListener('focusin', keepFocusInside);

  const next = $('#onboardingNext', root);
  const close = $('#onboardingClose', root);
  const dots = $('#onboardingDots', root);
  const motion = window.matchMedia('(prefers-reduced-motion: reduce)');
  const scenes = new Map();
  const themeToggle = $('#onboardingThemeToggle', root);
  const updateTheme = () => {
    const theme = getTheme();
    root.dataset.theme = theme;
    const label = theme === 'light' ? '切換暗色' : '切換亮色';
    themeToggle.setAttribute('aria-label', label);
    themeToggle.title = label;
    themeToggle.firstElementChild.textContent = theme === 'light' ? '☾' : '☼';
    for (const controller of scenes.values()) controller.setTheme(theme);
  };
  themeToggle.addEventListener('click', () =>
    setTheme(getTheme() === 'dark' ? 'light' : 'dark'),
  );
  window.addEventListener('meadow:themechange', updateTheme);
  updateTheme();
  const updateMotion = () => {
    // Always plays; the system preference lowers amplitude, never strands
    // this no-controls illustration on a frozen first frame.
    for (const controller of scenes.values())
      controller.setReducedMotion(motion.matches);
  };
  const onMotionChange = updateMotion;
  motion.addEventListener('change', onMotionChange);

  const render = () => {
    root.dataset.step = String(step);
    root.setAttribute('aria-labelledby', `meadowOnboardingTitle-${step}`);
    for (const panel of root.querySelectorAll('[data-onboarding-step]')) {
      const active = Number(panel.dataset.onboardingStep) === step;
      panel.setAttribute('aria-hidden', String(!active));
      panel.inert = !active;
      const canvas = panel.querySelector('canvas');
      if (active && !scenes.has(canvas)) {
        const controller = createBenefitsScene(canvas, {
          scene: canvas.dataset.asciiScene,
          theme: getTheme(),
        });
        controller.setReducedMotion(motion.matches);
        scenes.set(canvas, controller);
      }
      scenes.get(canvas)?.setActive(active);
    }
    [...dots.children].forEach((dot, index) => {
      dot.classList.toggle('active', index === step);
      dot.setAttribute('aria-current', index === step ? 'step' : 'false');
    });
    next.firstElementChild.textContent =
      step === slides.length - 1 ? '開始使用' : '繼續';
    $('.meadow-onboarding-slides', root).scrollTop = 0;
  };

  slides.forEach((slide, index) => {
    const dot = document.createElement('button');
    dot.type = 'button';
    dot.setAttribute('aria-label', `前往第 ${index + 1} 頁：${slide.eyebrow}`);
    dot.addEventListener('click', () => {
      step = index;
      render();
    });
    dots.append(dot);
  });

  // Horizontal touch gestures change pages; vertical scrolling is left native.
  let gesture = null;
  const slideArea = $('.meadow-onboarding-slides', root);
  slideArea.addEventListener('pointerdown', (event) => {
    if (event.pointerType === 'touch' && event.isPrimary)
      gesture = { id: event.pointerId, x: event.clientX, y: event.clientY };
  });
  slideArea.addEventListener('pointercancel', () => {
    gesture = null;
  });
  slideArea.addEventListener('pointerup', (event) => {
    if (!gesture || gesture.id !== event.pointerId) return;
    const dx = event.clientX - gesture.x,
      dy = event.clientY - gesture.y;
    gesture = null;
    if (Math.abs(dx) < 55 || Math.abs(dx) < Math.abs(dy) * 1.6) return;
    step = Math.max(0, Math.min(slides.length - 1, step + (dx < 0 ? 1 : -1)));
    render();
  });

  const finish = (result) => {
    if (!root.isConnected) return;
    markOnboardingSeen();
    motion.removeEventListener('change', onMotionChange);
    window.removeEventListener('meadow:themechange', updateTheme);
    document.removeEventListener('focusin', keepFocusInside);
    for (const controller of scenes.values()) controller.destroy();
    scenes.clear();
    root.remove();
    if (
      previouslyFocused instanceof HTMLElement &&
      previouslyFocused.isConnected
    )
      previouslyFocused.focus();
    resolveResult(result);
  };

  next.addEventListener('click', () => {
    if (step < slides.length - 1) {
      step += 1;
      render();
      return;
    }
    finish('login');
  });
  close.addEventListener('click', () => finish('dismissed'));
  root.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      finish('dismissed');
      return;
    }
    if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
      event.preventDefault();
      step = Math.max(
        0,
        Math.min(
          slides.length - 1,
          step + (event.key === 'ArrowRight' ? 1 : -1),
        ),
      );
      render();
      return;
    }
    if (event.key !== 'Tab') return;
    const items = focusable(root);
    if (!items.length) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (
      event.shiftKey &&
      (document.activeElement === first || document.activeElement === root)
    ) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  });

  updateMotion();
  render();
  root.focus({ preventScroll: true });
  return resultPromise;
}

export function resetProductOnboarding() {
  try {
    localStorage.removeItem(ONBOARDING_KEY);
  } catch {
    // Non-security preference only.
  }
}

window.MeadowProductExperience = Object.freeze({
  showOnboarding: (options) => showProductOnboarding(options),
  resetOnboarding: resetProductOnboarding,
  hasSeenOnboarding,
});
