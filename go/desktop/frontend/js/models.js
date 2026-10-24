// models.js — first-run model download overlay. Shows downloadable models
// (catalog + presence + RAM recommendation), drives a resumable pull, and polls
// progress. Self-contained: build it, wire maybeShowModelSetup() at startup.

import { daemon } from './core.js?v=raven-execution-20261006a';

let ref = null; // { wrap, grid, meta }
let pollTimer = null;
let pulling = null; // served name currently downloading
let vaultMode = false;
let vaultInstalledCount = 0;
let vaultOperation = '';

function fmtBytes(b) {
  b = Number(b) || 0;
  if (b < 1024) return `${b} B`;
  const u = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do {
    b /= 1024;
    i++;
  } while (b >= 1024 && i < u.length - 1);
  return `${b.toFixed(1)} ${u[i]}`;
}

function build() {
  const wrap = document.createElement('div');
  wrap.id = 'modelSetup';
  wrap.hidden = true;
  wrap.innerHTML =
    `<div class="ms-card" role="dialog" aria-modal="true" aria-label="模型狀態">` +
    `<div class="ms-head"><span class="ms-title">歡迎使用 Meadow Code</span>` +
    `<button class="ms-x" aria-label="關閉">✕</button></div>` +
    `<p class="ms-sub">官方模型由 Meadow Agent 下載、驗證與管理。</p>` +
    `<div class="ms-meta" id="msMeta"></div>` +
    `<div class="ms-grid" id="msGrid"></div>` +
    `<p class="ms-foot">自訂模型匯入需等待 opaque file capability，目前不支援 URL 或路徑匯入。</p>` +
    `</div>`;
  document.body.appendChild(wrap);
  wrap
    .querySelector('.ms-x')
    .addEventListener('click', () => (wrap.hidden = true));
  wrap.addEventListener('mousedown', (e) => {
    if (e.target === wrap && !pulling) wrap.hidden = true;
  });
  ref = {
    wrap,
    grid: wrap.querySelector('#msGrid'),
    meta: wrap.querySelector('#msMeta'),
  };
}

function card(m) {
  if (m.official) return vaultCard(m);
  const c = document.createElement('div');
  c.className = 'ms-model' + (m.recommended ? ' rec' : '');
  c.dataset.model = m.servedName;
  const badge = m.recommended
    ? `<span class="ms-badge">建議・你的機器</span>`
    : '';
  const desc = modelDescription(m);
  let action;
  if (m.delivery === 'embedded') {
    action = `<div class="ms-ok"><span class="ms-tick">✓</span> 內建於 App</div>`;
  } else if (m.present) {
    action = `<div class="ms-ok"><span class="ms-tick">✓</span> 已下載</div>`;
  } else if (pulling === m.servedName) {
    action =
      `<div class="ms-bar"><div class="ms-fill" style="width:0%"></div></div>` +
      `<div class="ms-prog">準備中…</div>` +
      `<button class="ms-cancel">取消</button>`;
  } else {
    action = `<button class="ms-dl"${pulling ? ' disabled' : ''}>下載</button>`;
  }
  c.innerHTML =
    badge +
    `<div class="ms-row"><span class="ms-name">${m.servedName}</span>` +
    `<span class="ms-size">${m.approxGB} GB</span></div>` +
    `<p class="ms-desc">${desc}</p>` +
    action;
  const dl = c.querySelector('.ms-dl');
  if (dl) dl.addEventListener('click', () => startPull(m.servedName));
  const cancel = c.querySelector('.ms-cancel');
  if (cancel) cancel.addEventListener('click', () => daemon.pullCancel());
  return c;
}

function vaultErrorText(error) {
  switch (error && error.code) {
    case 'MODEL_REMOVAL_DENIED':
      return '移除授權未核准';
    case 'MODEL_NOT_FOUND':
      return '找不到官方模型';
    case 'MODEL_REJECTED':
      return '模型安裝或驗證未通過';
    default:
      return '模型操作失敗';
  }
}

function vaultCard(model) {
  const key = `${model.modelId}@${model.version}`;
  const card = document.createElement('div');
  card.className = 'ms-model vault-model';
  card.dataset.model = key;
  const row = document.createElement('div');
  row.className = 'ms-row';
  const name = document.createElement('span');
  name.className = 'ms-name';
  name.textContent = model.modelId;
  const version = document.createElement('span');
  version.className = 'ms-size';
  version.textContent = `v${model.version}`;
  row.append(name, version);
  const description = document.createElement('p');
  description.className = 'ms-desc';
  description.textContent = model.profileId
    ? `受保護設定檔 ${model.profileId}`
    : '官方受保護模型';
  const action = document.createElement('div');
  action.className = 'vault-action';
  card.append(row, description, action);

  if (vaultOperation === key) {
    action.textContent = '正在下載、驗證並安裝…';
    action.classList.add('busy');
    return card;
  }
  if (model.installed && model.runtimeReady) {
    const ready = document.createElement('span');
    ready.className = 'ms-ok';
    ready.textContent = '已安裝並啟動';
    action.appendChild(ready);
    action.appendChild(removeButton(model));
    return card;
  }
  if (model.installed) {
    const start = document.createElement('button');
    start.className = 'ms-dl vault-start';
    start.textContent = '啟動模型';
    start.addEventListener('click', async () => {
      vaultOperation = key;
      await refresh();
      try {
        await daemon.activateInstalledModel(model.modelId);
        vaultOperation = '';
        await refresh();
      } catch (error) {
        vaultOperation = '';
        prependError(vaultErrorText(error));
        await refresh();
      }
    });
    action.append(start, removeButton(model));
    return card;
  }

  const reveal = document.createElement('button');
  reveal.className = 'ms-dl vault-license-reveal';
  reveal.textContent = '檢視授權並安裝';
  reveal.addEventListener('click', () => {
    action.textContent = '';
    const license = document.createElement('div');
    license.className = 'vault-license';
    license.textContent = `授權：${model.licenseId || '需要授權確認'}`;
    const subject = document.createElement('input');
    subject.className = 'vault-subject';
    subject.maxLength = 256;
    subject.placeholder = '授權主體';
    subject.autocomplete = 'username';
    const acceptance = document.createElement('label');
    acceptance.className = 'vault-accept';
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    const acceptanceText = document.createElement('span');
    acceptanceText.textContent = '我已閱讀並接受此模型授權';
    acceptance.append(checkbox, acceptanceText);
    const install = document.createElement('button');
    install.className = 'ms-dl vault-install';
    install.textContent = '下載、驗證並安裝';
    install.addEventListener('click', async () => {
      const licenseSubject = subject.value.trim();
      if (!checkbox.checked || !licenseSubject) {
        prependError('必須填寫授權主體並明確接受授權');
        return;
      }
      if (/[/\\:\u0000-\u001f]/.test(licenseSubject)) {
        prependError('授權主體格式無效');
        return;
      }
      vaultOperation = key;
      await refresh();
      try {
        await daemon.modelVaultInstall(
          model.modelId,
          model.version,
          licenseSubject,
        );
        vaultOperation = '';
        await refresh();
      } catch (error) {
        vaultOperation = '';
        prependError(vaultErrorText(error));
        await refresh();
      }
    });
    action.append(license, subject, acceptance, install);
    subject.focus();
  });
  action.appendChild(reveal);
  return card;
}

function removeButton(model) {
  const button = document.createElement('button');
  button.className = 'ms-cancel vault-remove';
  button.textContent = '移除';
  button.addEventListener('click', async () => {
    if (button.dataset.confirmed !== '1') {
      button.dataset.confirmed = '1';
      button.textContent = '再次點擊確認移除';
      return;
    }
    const key = `${model.modelId}@${model.version}`;
    vaultOperation = key;
    await refresh();
    try {
      await daemon.modelVaultRemove(model.modelId, model.version);
      vaultOperation = '';
      await refresh();
    } catch (error) {
      vaultOperation = '';
      prependError(vaultErrorText(error));
      await refresh();
    }
  });
  return button;
}

function modelDescription(m) {
  const memory = Number(m.minRamGiB) > 0 ? `適合 ${m.minRamGiB} GB 以上` : '';
  if (m.roleLabel && m.roleDescription) {
    return [m.roleLabel, m.roleDescription, memory].filter(Boolean).join('・');
  }
  if (m.servedName === 'Meadow 30B') {
    return '白領主力 Agent・辦公任務、文件、研究整合、長流程與自我迭代・適合 32 GB 以上';
  }
  if (m.servedName === 'Meadow 27B') {
    return 'Agent Coding・讀取 codebase、多檔修改、執行編譯與測試、依診斷迭代修正・適合 24 GB 以上';
  }
  if (m.servedName === 'Meadow Coder 35B') {
    return 'Ornith 1.5 35B-A3B MXFP4・讀 codebase、多檔修改、執行測試、除錯直到驗收・適合 32 GB 以上';
  }
  if (m.servedName === 'Meadow 26B') {
    return 'Chat / Plan・聊天與任務規劃；專屬 Agent pipeline 尚未完成・適合 32 GB 以上';
  }
  if (m.servedName === 'Meadow 12B') {
    return 'Chat / Plan・聊天與任務規劃；專屬 Agent pipeline 尚未完成・適合 16 GB 以上';
  }
  return 'Research Worker・搜尋意圖、Meadow Browser 收集、證據抽取、RAG 與短摘要・適合 8 GB 以上';
}

async function refresh() {
  let protectedMode = true;
  try {
    protectedMode = await daemon.protectedClientMode();
  } catch {
    protectedMode = true;
  }
  try {
    const catalogStatus = await daemon.modelVaultCatalogStatus();
    const [catalog, installed] = await Promise.all([
      daemon.modelVaultCatalog(),
      daemon.modelVaultList(),
    ]);
    vaultMode = true;
    const installedMap = new Map(
      ((installed && installed.models) || []).map((model) => [
        `${model.modelId}\u0000${model.version}`,
        model,
      ]),
    );
    const models = ((catalog && catalog.models) || []).map((model) => ({
      ...model,
      ...(installedMap.get(`${model.modelId}\u0000${model.version}`) || {}),
      official: true,
    }));
    vaultInstalledCount = models.filter((model) => model.installed).length;
    if (!ref) build();
    ref.meta.textContent =
      catalogStatus && catalogStatus.state === 'ready'
        ? vaultInstalledCount
          ? `Model Vault 已就緒 · 已安裝 ${vaultInstalledCount} 個模型`
          : 'Model Vault 已就緒 · 尚未安裝模型'
        : 'Model Vault 目前無法安裝模型';
    ref.grid.textContent = '';
    for (const model of models) ref.grid.appendChild(vaultCard(model));
    return vaultInstalledCount > 0;
  } catch (error) {
    if (protectedMode || !error || error.status !== 404) {
      vaultMode = true;
      if (!ref) build();
      ref.meta.textContent = 'Model Vault 暫時無法使用';
      ref.grid.textContent = '';
      return false;
    }
  }
  vaultMode = false;
  let data;
  try {
    data = await daemon.modelCatalog();
  } catch {
    return false;
  }
  if (!ref) build();
  ref.meta.textContent = data.totalRamGiB
    ? `記憶體 ${data.totalRamGiB} GB`
    : '';
  ref.grid.innerHTML = '';
  for (const m of data.models || []) ref.grid.appendChild(card(m));
  return (data.models || []).every((m) => m.present);
}

function startPull(model) {
  pulling = model;
  daemon.pullModel(model).catch(() => {});
  refresh();
  clearInterval(pollTimer);
  pollTimer = setInterval(tick, 500);
}

async function tick() {
  let st;
  try {
    st = await daemon.pullStatus();
  } catch {
    return;
  }
  const c = ref && ref.grid.querySelector(`[data-model="${st.model}"]`);
  if (c) {
    const fill = c.querySelector('.ms-fill');
    const prog = c.querySelector('.ms-prog');
    const pct = st.totalSize ? (st.totalDone / st.totalSize) * 100 : 0;
    if (fill) fill.style.width = `${Math.min(100, pct).toFixed(1)}%`;
    if (prog)
      prog.textContent = `${fmtBytes(st.totalDone)} / ${fmtBytes(st.totalSize)} · ${fmtBytes(st.bytesPerSec)}/s`;
  }
  if (!st.running) {
    clearInterval(pollTimer);
    pulling = null;
    if (st.error) prependError(st.error);
    refresh();
  }
}

function prependError(msg) {
  if (!ref) return;
  let e = ref.wrap.querySelector('.ms-err');
  if (!e) {
    e = document.createElement('div');
    e.className = 'ms-err';
    ref.meta.after(e);
  }
  e.textContent = `下載失敗：${msg}`;
}

// openModelSetup shows the overlay (manual trigger, e.g. a settings button).
export async function openModelSetup() {
  const allPresent = await refresh();
  if (ref) ref.wrap.hidden = false;
  return allPresent;
}

// maybeShowModelSetup shows the overlay at startup ONLY when no model is on
// disk yet — a fresh install needs to download before anything works.
export async function maybeShowModelSetup() {
  const hasInstalledVaultModel = await refresh();
  if (vaultMode) {
    if (!hasInstalledVaultModel && ref) ref.wrap.hidden = false;
    return;
  }
  let data;
  try {
    data = await daemon.modelCatalog();
  } catch {
    return; // daemon not up yet; the chat's model-status flow handles it
  }
  const anyPresent = (data.models || []).some((m) => m.present);
  if (anyPresent) return;
  await refresh();
  if (ref) ref.wrap.hidden = false;
}
