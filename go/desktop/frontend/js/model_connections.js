// Provider credentials stay in this short-lived form until verification. The
// native service owns persistence; browser storage and model messages never do.
import { bus } from './core.js?v=raven-execution-20261006a';

const PROVIDERS_PATH = '/native/model-providers';
const PROVIDER_NAMES = { xai: 'xAI / Grok', deepseek: 'DeepSeek' };
// Provider choices describe a connection, never an invented model catalog.
const CLOUD_PROVIDERS = [
  {
    id: 'openai',
    name: 'OpenAI',
    badge: 'O',
    baseUrl: 'https://api.openai.com/v1',
    protocol: 'openai-compatible',
  },
  {
    id: 'anthropic',
    name: 'Anthropic',
    badge: 'A',
    baseUrl: 'https://api.anthropic.com',
    protocol: 'anthropic',
  },
  {
    id: 'xai',
    name: 'xAI / Grok',
    badge: 'x',
    baseUrl: 'https://api.x.ai/v1',
    protocol: 'openai-compatible',
  },
  {
    id: 'deepseek',
    name: 'DeepSeek',
    badge: 'D',
    baseUrl: 'https://api.deepseek.com/v1',
    protocol: 'openai-compatible',
  },
  {
    id: 'other',
    name: '其他 API',
    badge: '+',
    baseUrl: '',
    protocol: 'openai-compatible',
  },
];
const SOURCE_CHOICES = [
  {
    group: '雲端',
    id: 'managed-api',
    title: 'Meadow API',
    description: '使用會員模型與 API 額度',
    icon: 'meadow',
  },
  {
    group: '雲端',
    id: 'provider-api',
    title: '其他供應商',
    description: 'OpenAI、Anthropic、Grok、DeepSeek',
    icon: 'cloud',
  },
  {
    group: '地端',
    id: 'meadow-core',
    title: 'Meadow Core',
    description: '偵測這台電腦，或連接另一台主機',
    icon: 'meadow',
  },
  {
    group: '地端',
    id: 'local-api',
    title: '其他本地 API',
    description: '連接既有的模型服務',
    icon: 'chip',
  },
];
const CONNECTION_NAMES = {
  'managed-api': 'Meadow 會員 API',
  'provider-api': '其他供應商 API',
  'local-api': '現有本地 API',
  'meadow-core': 'Meadow Core',
};
const RESTART_NOTICE =
  '設定已儲存。請重新啟動 Meadow Desktop 後使用，執行中的模型不會立即切換。';

function sameCoreEndpoint(left, right) {
  const canonical = (value) => {
    try {
      const url = new URL(value);
      if (['localhost', '127.0.0.1', '[::1]'].includes(url.hostname.toLowerCase())) url.hostname = 'localhost';
      url.pathname = url.pathname.replace(/\/+$/, '');
      return url.href.replace(/\/+$/, '');
    } catch { return ''; }
  };
  const key = canonical(left);
  return Boolean(key) && key === canonical(right);
}

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function connectionIcon(kind, badge = '') {
  const wrapper = element('span', 'model-source-icon');
  wrapper.setAttribute('aria-hidden', 'true');
  if (badge) {
    wrapper.textContent = badge;
    return wrapper;
  }
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.8');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  const paths = {
    meadow: ['M3 9c3-5 6 5 9 0s6 5 9 0', 'M3 15c3-5 6 5 9 0s6 5 9 0'],
    cloud: ['M7 18a5 5 0 0 1-1-10 6 6 0 0 1 11-2 5 5 0 0 1 1 12Z'],
    chip: [
      'M7 7h10v10H7Z',
      'M9 3v4m6-4v4M9 17v4m6-4v4M3 9h4m10 0h4M3 15h4m10 0h4',
    ],
  };
  for (const value of paths[kind] || paths.cloud) {
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', value);
    svg.appendChild(path);
  }
  wrapper.appendChild(svg);
  return wrapper;
}

function button(text, id) {
  const node = element('button', '', text);
  node.type = 'button';
  if (id) node.id = id;
  return node;
}

async function providerRequest(path, body, signal) {
  const response = await fetch(path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Meadow-Settings': '1' },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    signal,
  });
  let result;
  try {
    result = await response.json();
  } catch {
    result = null;
  }
  if (!response.ok) {
    // Never propagate provider response text, which can contain credentials or
    // request details. Only a status and a bounded known code leave this layer.
    const error = new Error('model_provider_request_failed');
    error.status = response.status;
    error.code =
      typeof result?.error?.code === 'string' ? result.error.code : '';
    throw error;
  }
  return result;
}

function safeError(error, action) {
  const codeMessages = {
    native_settings_only: '請從 Meadow Desktop 的設定開啟模型管理。',
    model_provider_disabled: '目前啟動方式未開放自行連接模型供應商。',
    catalog_changed: '模型設定已變更，請重新開啟此頁再試。',
    catalog_unavailable: '無法讀取模型設定，請重新開啟此頁再試。',
    multiple_credentials: '此供應商有多組金鑰，請輸入要使用的 API key。',
    invalid_api_key: '無法驗證 API key，請確認供應商、金鑰與帳號權限。',
    no_supported_models: '這組金鑰沒有可加入的對話模型，請確認供應商帳號權限。',
    invalid_models: '模型選擇無效，請重新驗證並選擇模型。',
    model_id_conflict: '已有其他供應商使用相同模型 ID，請選擇其他模型。',
    model_limit: '已達到 32 個模型的設定上限，無法再新增。',
    save_cancelled: '儲存已取消，請重新驗證後再試。',
    credential_unavailable: '無法安全讀取金鑰，請重新輸入 API key。',
    credential_recovery_required:
      '設定未儲存且金鑰還原失敗，請重新驗證並儲存。',
    agent_unavailable: '無法確認工作狀態，請重新啟動 Meadow Desktop 後再儲存。',
    invalid_base_url: '請輸入有效的 API Base URL。',
    invalid_connection:
      '請確認連線名稱、類型、API 位址及 Token 上限，再重新驗證。',
    connection_not_found: '找不到這組連線，請重新開啟此頁再試。',
    stored_key_endpoint_changed:
      'API 位址或連線類型已變更，請重新輸入 API key。',
    credential_endpoint_changed:
      'API 位址或連線類型已變更，請重新輸入 API key。',
    invalid_core_invitation:
      'Core 配對邀請無效或已過期，請從 Core 重新產生邀請。',
    core_pairing_failed: '無法完成 Core 配對，請確認服務位址與邀請期限。',
    local_core_unavailable: '無法讀取這台電腦的 Core 連線，請改用配對邀請。',
    local_core_changed: 'Core 連線已變更，請重新偵測。',
    core_identity_mismatch: '無法確認 Core 身分與連線權限，請重新配對。',
    managed_api_unavailable: 'Meadow API 尚未開放，你仍可連接自己的 API。',
    managed_api_not_allowed:
      '目前會員方案尚未開放 Meadow API，請確認訂閱或 API 額度。',
  };
  if (typeof codeMessages[error?.code] === 'string')
    return codeMessages[error.code];
  if (error?.status === 409)
    return '目前仍有執行中或排隊的工作。完成或停止後，再儲存模型。';
  if (error?.status === 410 || error?.code === 'verification_expired')
    return '驗證已過期，請重新驗證供應商。';
  if (error?.status === 401 || error?.status === 403)
    return '無法驗證 API key，請確認供應商、金鑰與帳號權限。';
  if (error?.status === 429) return '供應商暫時限制請求次數，請稍後再試。';
  if (error?.status === 400)
    return '無法使用這組設定，請確認 API key 並重新驗證。';
  return action === 'save'
    ? '尚未確認模型儲存成功，請重新開啟此頁檢查後再試。'
    : '目前無法連線到供應商，請檢查網路後再試。';
}

// Returns explicit cleanup so changing settings tabs clears secrets just like
// closing the dialog. No detached form or pending response can revive them.
export function buildModelConnections(body) {
  const section = element('section', 'settings-model-connections');
  const header = element('div', 'settings-group');
  const copy = element('header', 'settings-group-copy');
  copy.append(
    element('strong', '', '模型供應商'),
    element('span', '', '連接 API 後，新增的模型會出現在對話的模型選單。'),
  );
  const add = button('設定模型', 'settingsAddModel');
  add.disabled = true;
  add.setAttribute('aria-expanded', 'false');
  add.setAttribute('aria-controls', 'settingsModelConnectionForm');
  header.append(copy, add);
  const summary = element(
    'p',
    'settings-model-connection-summary',
    '正在讀取模型設定…',
  );
  summary.setAttribute('aria-live', 'polite');
  const form = element('form', 'settings-model-connection-form');
  form.id = 'settingsModelConnectionForm';
  form.hidden = true;
  form.autocomplete = 'off';
  const steps = element('ol', 'model-setup-steps');
  steps.hidden = true;
  steps.setAttribute('aria-label', '模型設定步驟');
  for (const [index, title] of [
    '模型來源',
    '連接供應商',
    '選擇模型',
  ].entries()) {
    const item = element('li');
    item.dataset.step = String(index + 1);
    item.append(
      element('span', 'model-step-number', String(index + 1)),
      element('span', '', title),
    );
    steps.appendChild(item);
  }
  const sourceChooser = element('section', 'model-source-chooser');
  sourceChooser.hidden = true;
  sourceChooser.setAttribute('aria-label', '選擇模型來源');
  for (const group of ['雲端', '地端']) {
    const area = element('section', 'model-source-group');
    area.appendChild(element('h3', '', group));
    const grid = element('div', 'model-source-grid');
    for (const source of SOURCE_CHOICES.filter(
      (item) => item.group === group,
    )) {
      const tile = button('', `settingsSource-${source.id}`);
      tile.className = 'model-source-tile';
      tile.dataset.source = source.id;
      tile.setAttribute('aria-pressed', 'false');
      const text = element('span', 'model-source-copy');
      text.append(
        element('strong', '', source.title),
        element('small', '', source.description),
      );
      tile.append(connectionIcon(source.icon), text);
      grid.appendChild(tile);
    }
    area.appendChild(grid);
    sourceChooser.appendChild(area);
  }
  const sourceHeading = element('div', 'model-source-heading');
  const selectedSourceLabel = element('strong');
  const backToSources = button('更換來源', 'settingsChangeModelSource');
  sourceHeading.append(selectedSourceLabel, backToSources);
  const providerChoices = element('div', 'model-provider-grid');
  providerChoices.setAttribute('aria-label', '選擇供應商');
  const providerLabel = element('label', '', '模型供應商');
  providerLabel.htmlFor = 'settingsModelProvider';
  const provider = element('select');
  provider.id = 'settingsModelProvider';
  provider.hidden = true;
  providerLabel.hidden = true;
  provider.setAttribute('aria-hidden', 'true');
  const nameLabel = element('label', '', '連線名稱');
  nameLabel.htmlFor = 'settingsModelConnectionName';
  const connectionName = element('input');
  connectionName.id = 'settingsModelConnectionName';
  connectionName.type = 'text';
  connectionName.maxLength = 120;
  const urlLabel = element('label', '', 'API Base URL');
  urlLabel.htmlFor = 'settingsModelBaseURL';
  const baseUrl = element('input');
  baseUrl.id = 'settingsModelBaseURL';
  baseUrl.type = 'url';
  baseUrl.placeholder = 'http://127.0.0.1:8000/v1';
  baseUrl.autocomplete = 'off';
  baseUrl.spellcheck = false;
  const urlNote = element(
    'p',
    'settings-model-connection-note',
    '遠端服務請使用 HTTPS；本地 API 可使用 localhost 或內網 IP 的 HTTP。',
  );
  urlNote.id = 'settingsModelURLNote';
  baseUrl.setAttribute('aria-describedby', urlNote.id);
  const imageLabel = element('label', 'settings-model-stored-key');
  const supportsImages = element('input');
  supportsImages.id = 'settingsModelSupportsImages';
  supportsImages.type = 'checkbox';
  imageLabel.append(supportsImages, element('span', '', '此服務支援圖片輸入'));
  const contextLabel = element('label', '', '上下文 Token 上限（選填）');
  contextLabel.htmlFor = 'settingsModelContextTokens';
  const contextTokens = element('input');
  contextTokens.id = contextLabel.htmlFor;
  const outputLabel = element('label', '', '輸出 Token 上限（選填）');
  outputLabel.htmlFor = 'settingsModelMaxOutputTokens';
  const maxOutputTokens = element('input');
  maxOutputTokens.id = outputLabel.htmlFor;
  for (const input of [contextTokens, maxOutputTokens]) {
    input.type = 'number';
    input.min = '1';
    input.max = '8388608';
    input.step = '1';
  }
  const capabilityNote = element(
    'p',
    'settings-model-connection-note',
    '請依服務文件設定圖片能力與 Token 上限。編輯既有連線時，未調整的欄位會保留各模型原設定。',
  );
  const connectionFields = [
    nameLabel,
    connectionName,
    urlLabel,
    baseUrl,
    urlNote,
    imageLabel,
    contextLabel,
    contextTokens,
    outputLabel,
    maxOutputTokens,
    capabilityNote,
  ];
  for (const field of connectionFields) field.hidden = true;
  const basicFields = element('div', 'model-connection-fields');
  const advanced = element('details', 'model-connection-advanced');
  advanced.id = 'settingsModelAdvanced';
  advanced.appendChild(element('summary', '', '進階連線設定'));
  const advancedFields = element('div', 'model-connection-fields');
  advanced.appendChild(advancedFields);
  const protocolLabel = element('label', '', 'API 協定');
  protocolLabel.htmlFor = 'settingsModelProtocol';
  const protocol = element('select');
  protocol.id = protocolLabel.htmlFor;
  for (const [value, title] of [
    ['openai-compatible', 'OpenAI 相容 API'],
    ['anthropic', 'Anthropic API'],
  ]) {
    const option = element('option', '', title);
    option.value = value;
    protocol.appendChild(option);
  }
  advancedFields.append(protocolLabel, protocol, ...connectionFields);
  const coreImport = element('details', 'model-connection-advanced');
  coreImport.appendChild(element('summary', '', '連接另一台主機'));
  const coreImportBody = element('div', 'model-connection-fields');
  coreImport.appendChild(coreImportBody);
  const managed = element('fieldset', 'settings-model-managed-api');
  managed.hidden = true;
  const managedStatus = element(
    'p',
    'settings-model-connection-note',
    '正在讀取 Meadow API 狀態…',
  );
  managedStatus.id = 'settingsManagedAPIStatus';
  managedStatus.tabIndex = -1;
  managedStatus.setAttribute('role', 'status');
  const managedUsage = element('p', 'settings-model-connection-note');
  managedUsage.id = 'settingsManagedAPIUsage';
  const managedConnect = button('連接 Meadow API', 'settingsConnectManagedAPI');
  managed.append(
    element('legend', '', 'Meadow 會員 API'),
    managedStatus,
    managedUsage,
    managedConnect,
  );
  const pairing = element('fieldset', 'settings-model-core-pairing');
  pairing.hidden = true;
  pairing.style.minWidth = '0';
  pairing.append(element('legend', '', '連接 Meadow Core'));
  const invitationLabel = element('label', '', '貼上 Core 配對邀請');
  invitationLabel.htmlFor = 'settingsCoreInvitation';
  const invitation = element('textarea');
  invitation.id = invitationLabel.htmlFor;
  invitation.rows = 3;
  invitation.autocomplete = 'off';
  invitation.spellcheck = false;
  invitation.style.width = '100%';
  invitation.style.boxSizing = 'border-box';
  const pairActions = element('div', 'settings-model-connection-actions');
  const pair = button('配對並取得模型', 'settingsPairCore');
  const importInvitation = button(
    '匯入 .meadow-connection',
    'settingsImportCoreConnection',
  );
  const invitationFile = element('input');
  invitationFile.id = 'settingsCoreConnectionFile';
  invitationFile.type = 'file';
  invitationFile.accept = '.meadow-connection,application/json';
  invitationFile.hidden = true;
  const detect = button(
    '偵測這台電腦的 Meadow Core',
    'settingsDetectLocalCore',
  );
  pairActions.append(importInvitation, invitationFile, pair);
  const detectedCores = element('div');
  detectedCores.id = 'settingsDetectedCores';
  const discovery = element('section', 'settings-model-local-core');
  discovery.hidden = true;
  const discoveryStatus = element('p', 'settings-model-connection-note');
  discoveryStatus.id = 'settingsLocalCoreStatus';
  discoveryStatus.tabIndex = -1;
  discoveryStatus.setAttribute('role', 'status');
  discovery.append(
    element('strong', '', '此電腦的 Meadow Core'),
    discoveryStatus,
    detectedCores,
    detect,
  );
  coreImportBody.append(
    invitationLabel,
    invitation,
    pairActions,
    element(
      'p',
      'settings-model-connection-note',
      '請從要連接的 Meadow Core 產生配對邀請或連線檔。',
    ),
  );
  pairing.append(discovery, coreImport);
  const keyLabel = element('label', '', 'API key');
  keyLabel.htmlFor = 'settingsModelAPIKey';
  const key = element('input');
  key.id = 'settingsModelAPIKey';
  key.type = 'password';
  key.autocomplete = 'new-password';
  key.spellcheck = false;
  key.autocapitalize = 'none';
  key.setAttribute('aria-describedby', 'settingsModelKeyNote');
  const keyNote = element('p', 'settings-model-connection-note');
  keyNote.id = 'settingsModelKeyNote';
  const storedLabel = element('label', 'settings-model-stored-key');
  const stored = element('input');
  stored.type = 'checkbox';
  stored.id = 'settingsModelStoredKey';
  storedLabel.append(stored, element('span', '', '使用已儲存的 API key'));
  const actions = element('div', 'settings-model-connection-actions');
  const verify = button('驗證並取得模型', 'settingsVerifyModelProvider');
  verify.type = 'submit';
  const cancel = button('返回', 'settingsCancelModelProvider');
  actions.append(verify, cancel);
  const status = element('p', 'settings-model-connection-status');
  status.id = 'settingsModelConnectionStatus';
  status.setAttribute('role', 'status');
  status.setAttribute('aria-live', 'polite');
  const choices = element('fieldset', 'settings-model-choices');
  choices.hidden = true;
  const legend = element('legend', '', '勾選要使用的模型');
  const options = element('div', 'settings-model-choice-list');
  const save = button('儲存模型', 'settingsSaveModels');
  save.className = 'primary';
  const selectAll = button('全選', 'settingsSelectAllModels');
  save.disabled = true;
  choices.append(legend, selectAll, options, save);
  form.append(
    sourceHeading,
    providerChoices,
    providerLabel,
    provider,
    managed,
    pairing,
    basicFields,
    advanced,
    keyLabel,
    key,
    keyNote,
    storedLabel,
    actions,
    status,
    choices,
  );
  const savedConnections = element('div', 'settings-model-connection-list');
  savedConnections.id = 'settingsSavedModelConnections';
  savedConnections.hidden = true;
  section.append(header, summary, savedConnections, steps, sourceChooser, form);
  body.appendChild(section);

  let disposed = false;
  let generation = 0;
  let controller = null;
  let discoveryController = null;
  let discoveryRevision = 0;
  let discovering = false;
  let managedController = null;
  let managedRevision = 0;
  let managedAvailable = false;
  let managedConfigured = false;
  let managedAllowed = false;
  let providers = [];
  let connections = [];
  let connectionsMode = false;
  let selectedBrand = '';
  let wizardStage = 1;
  let wizardVisible = false;
  let enabled = false;
  let editingConnection = null;
  let replacementKeyRequired = false;
  let capabilityDirty = {};
  let verificationId = '';
  let busy = false;
  let saving = false;
  let expiryTimer = null;
  const active = (revision) =>
    !disposed && generation === revision && section.isConnected;
  const selectedModels = () =>
    [...options.querySelectorAll('input:checked')].map((input) => input.value);
  const declaredCapabilities = () => ({
    ...(capabilityDirty.supportsImages
      ? { supportsImages: supportsImages.checked }
      : {}),
    ...(capabilityDirty.contextTokens && contextTokens.value
      ? { contextTokens: Number(contextTokens.value) }
      : {}),
    ...(capabilityDirty.maxOutputTokens && maxOutputTokens.value
      ? { maxOutputTokens: Number(maxOutputTokens.value) }
      : {}),
  });
  const resetCapabilities = (connection = null) => {
    capabilityDirty = {};
    const models = Array.isArray(connection?.models) ? connection.models : [];
    const imageValues = new Set(
      models.map((model) => model.supportsImages === true),
    );
    supportsImages.checked = imageValues.size === 1 && imageValues.has(true);
    supportsImages.indeterminate = imageValues.size > 1;
    for (const [field, input] of [
      ['contextTokens', contextTokens],
      ['maxOutputTokens', maxOutputTokens],
    ]) {
      const values = new Set(models.map((model) => Number(model[field]) || 0));
      const value = values.size === 1 ? [...values][0] : 0;
      input.value = value > 0 ? String(value) : '';
    }
  };
  const showStep = (step) => {
    wizardStage = step;
    steps.hidden = !wizardVisible;
    sourceChooser.hidden = !wizardVisible || !connectionsMode || step !== 1;
    form.hidden = !wizardVisible || step === 1;
    section.dataset.setupStep = String(step);
    for (const item of steps.children) {
      const current = Number(item.dataset.step) === step;
      item.classList.toggle('is-current', current);
      item.classList.toggle('is-complete', Number(item.dataset.step) < step);
      if (current) item.setAttribute('aria-current', 'step');
      else item.removeAttribute('aria-current');
    }
  };
  const renderProviderChoices = () => {
    providerChoices.textContent = '';
    const rows = connectionsMode
      ? CLOUD_PROVIDERS
      : CLOUD_PROVIDERS.filter((item) =>
          providers.some((provider) => provider.id === item.id),
        );
    for (const row of rows) {
      const label = element('label', 'model-provider-tile');
      label.dataset.provider = row.id;
      const check = element('input');
      check.type = 'checkbox';
      check.id = `settingsCloudProvider-${row.id}`;
      check.setAttribute('aria-label', row.name);
      check.checked = selectedBrand === row.id;
      const text = element('span', 'model-provider-name', row.name);
      label.append(check, connectionIcon('', row.badge), text);
      check.addEventListener('change', () => {
        if (busy) {
          check.checked = selectedBrand === row.id;
          return;
        }
        const picked = check.checked;
        invalidate();
        key.value = '';
        invitation.value = '';
        invitationFile.value = '';
        stored.checked = false;
        replacementKeyRequired = editingConnection?.configured === true;
        selectedBrand = picked ? row.id : '';
        if (connectionsMode) {
          connectionName.value = picked
            ? row.id === 'other'
              ? '其他供應商 API'
              : row.name
            : '';
          baseUrl.value = picked ? row.baseUrl : '';
          protocol.value = row.protocol;
          resetCapabilities();
        } else provider.value = picked ? row.id : '';
        setStatus('');
        showStep(2);
        updateControls();
        if (picked) {
          if (row.id === 'other') baseUrl.focus();
          else key.focus();
        }
      });
      providerChoices.appendChild(label);
    }
  };
  const setStatus = (message, tone = '') => {
    status.textContent = message;
    status.dataset.tone = tone;
  };
  const clearVerification = () => {
    verificationId = '';
    clearTimeout(expiryTimer);
    expiryTimer = null;
    options.textContent = '';
    choices.hidden = true;
    if (wizardVisible && wizardStage === 3) showStep(2);
  };
  const invalidate = () => {
    generation += 1;
    controller?.abort();
    controller = null;
    busy = false;
    saving = false;
    clearVerification();
  };
  const updateControls = () => {
    const isManaged = connectionsMode && provider.value === 'managed-api';
    const isCore = connectionsMode && provider.value === 'meadow-core';
    const isCloudProvider =
      connectionsMode && provider.value === 'provider-api';
    const isCustom =
      connectionsMode &&
      (provider.value === 'local-api' ||
        (isCloudProvider && selectedBrand === 'other'));
    const providerPicked = !isCloudProvider || Boolean(selectedBrand);
    selectedSourceLabel.textContent = connectionsMode
      ? CONNECTION_NAMES[provider.value] || ''
      : '連接供應商';
    providerChoices.hidden =
      wizardStage === 3 || (!isCloudProvider && connectionsMode);
    sourceHeading.hidden = !connectionsMode;
    for (const input of providerChoices.querySelectorAll('input')) {
      input.checked =
        selectedBrand === input.closest('[data-provider]').dataset.provider;
      input.disabled = busy;
      input
        .closest('[data-provider]')
        .classList.toggle('is-selected', input.checked);
    }
    for (const tile of sourceChooser.querySelectorAll('[data-source]')) {
      tile.disabled =
        saving ||
        !enabled ||
        !providers.some((item) => item.id === tile.dataset.source);
      tile.setAttribute(
        'aria-pressed',
        String(wizardStage !== 1 && provider.value === tile.dataset.source),
      );
    }
    backToSources.disabled = saving;
    backToSources.textContent = editingConnection ? '取消編輯' : '更換來源';
    basicFields.hidden = !isCustom || wizardStage === 3;
    advanced.hidden =
      isManaged || isCore || wizardStage === 3 || !providerPicked;
    protocolLabel.hidden = !isCloudProvider;
    protocol.hidden = !isCloudProvider;
    const endpointFields = [
      nameLabel,
      connectionName,
      urlLabel,
      baseUrl,
      urlNote,
    ];
    const endpointContainer = isCustom ? basicFields : advancedFields;
    for (const field of endpointFields) {
      if (field.parentElement !== endpointContainer)
        endpointContainer.appendChild(field);
    }
    managed.hidden = !isManaged;
    managedConnect.disabled =
      busy || !managedAvailable || !managedConfigured || !managedAllowed;
    for (const field of connectionFields)
      field.hidden = !connectionsMode || isManaged || !providerPicked;
    for (const field of [keyLabel, key, keyNote])
      field.hidden =
        isManaged || isCore || !providerPicked || wizardStage === 3;
    verify.hidden =
      isManaged ||
      (isCore && !editingConnection) ||
      !providerPicked ||
      wizardStage === 3;
    const storedEndpointMatches =
      editingConnection &&
      editingConnection.kind === provider.value &&
      editingConnection.baseUrl === baseUrl.value.trim() &&
      (editingConnection.protocol || 'openai-compatible') === protocol.value;
    const configured = connectionsMode
      ? editingConnection?.configured === true && storedEndpointMatches
      : providers.find((item) => item.id === provider.value)?.configured ===
        true;
    storedLabel.hidden =
      !configured ||
      isManaged ||
      isCore ||
      !providerPicked ||
      wizardStage === 3;
    if (!configured) stored.checked = false;
    add.disabled = saving || !enabled || providers.length === 0;
    protocol.disabled = saving;
    provider.disabled = saving;
    connectionName.disabled = saving;
    baseUrl.disabled = saving;
    supportsImages.disabled = saving;
    contextTokens.disabled = saving;
    maxOutputTokens.disabled = saving;
    stored.disabled = saving;
    key.disabled = saving || stored.checked;
    pairing.hidden = !isCore || Boolean(editingConnection) || wizardStage === 3;
    discovery.hidden = !isCore;
    invitation.disabled = saving;
    invitationFile.disabled = busy;
    importInvitation.disabled = busy;
    pair.disabled = busy || !invitation.value.trim();
    detect.disabled = busy || discovering || !enabled;
    detect.hidden = discovering || detectedCores.querySelector('[data-core-id]') !== null;
    for (const control of detectedCores.querySelectorAll('button'))
      control.disabled = busy || discovering || !enabled;
    verify.disabled =
      busy ||
      (isCloudProvider &&
        selectedBrand !== 'other' &&
        !stored.checked &&
        !key.value.trim()) ||
      isManaged ||
      !providerPicked ||
      (connectionsMode
        ? !connectionName.value.trim() || !baseUrl.value.trim()
        : !stored.checked && !key.value.trim());
    verify.textContent = busy && !saving ? '正在驗證…' : '驗證並取得模型';
    save.disabled = busy || !verificationId || selectedModels().length === 0;
    save.textContent = saving ? '正在儲存…' : '完成設定';
    selectAll.disabled = busy;
    actions.hidden = wizardStage === 3;
    managed.hidden = !isManaged || wizardStage === 3;
    // Once a save is dispatched it may finish even if the browser request is
    // cancelled. Keep this form stable; closing Settings still clears memory.
    cancel.disabled = saving;
    for (const input of options.querySelectorAll('input'))
      input.disabled = saving || input.dataset.configured === 'true';
    for (const control of savedConnections.querySelectorAll('button, input'))
      control.disabled = busy || !enabled;
    keyLabel.textContent =
      isCloudProvider && selectedBrand !== 'other'
        ? 'API key'
        : connectionsMode
          ? 'API key（選填）'
          : 'API key';
    keyNote.textContent = stored.checked
      ? '驗證時使用此供應商已儲存的金鑰。'
      : isCloudProvider && selectedBrand !== 'other'
        ? '貼上供應商的 API key。驗證後由 Desktop 安全保存。'
        : connectionsMode
          ? replacementKeyRequired
            ? 'API 位址或連線類型已變更，原金鑰不會帶入。若服務需要授權，請重新輸入金鑰。'
            : 'API key 可留空；需要授權的服務請輸入金鑰。儲存後由 Desktop 安全保存。'
          : '金鑰只用於連接供應商，儲存模型後才保存到這台 Mac。';
  };
  const closeForm = () => {
    invalidate();
    key.value = '';
    invitation.value = '';
    invitationFile.value = '';
    stored.checked = false;
    editingConnection = null;
    replacementKeyRequired = false;
    wizardVisible = connectionsMode && connections.length === 0;
    showStep(1);
    add.setAttribute('aria-expanded', String(wizardVisible));
    selectedBrand = '';
    setStatus('');
    updateControls();
  };
  const selectProvider = () => {
    invalidate();
    key.value = '';
    invitation.value = '';
    invitationFile.value = '';
    if (connectionsMode) {
      stored.checked = false;
      replacementKeyRequired = editingConnection?.configured === true;
      resetCapabilities();
    } else {
      stored.checked =
        providers.find((item) => item.id === provider.value)?.configured ===
        true;
    }
    setStatus('');
    updateControls();
  };
  const renderCatalog = (catalog) => {
    connectionsMode = catalog?.mode === 'connections';
    enabled = catalog?.enabled !== false;
    discovery.hidden = true;
    providers = connectionsMode
      ? (Array.isArray(catalog?.kinds) ? catalog.kinds : []).filter(
          (item) => CONNECTION_NAMES[item?.id],
        )
      : (Array.isArray(catalog?.providers) ? catalog.providers : []).filter(
          (item) => PROVIDER_NAMES[item?.id],
        );
    for (const field of connectionFields) field.hidden = !connectionsMode;
    providerLabel.textContent = connectionsMode ? '連線類型' : '模型供應商';
    keyLabel.textContent = connectionsMode ? 'API key（選填）' : 'API key';
    copy.lastElementChild.textContent = connectionsMode
      ? '連接 Meadow 會員 API、其他供應商、本地 API 或另外安裝的 Meadow Core。'
      : '連接 API 後，新增的模型會出現在對話的模型選單。';
    add.textContent = connections.length ? '新增供應商' : '設定模型';
    provider.textContent = '';
    for (const item of providers) {
      const option = element(
        'option',
        '',
        connectionsMode ? CONNECTION_NAMES[item.id] : PROVIDER_NAMES[item.id],
      );
      option.value = item.id;
      provider.appendChild(option);
    }
    const models = (
      Array.isArray(catalog?.models) ? catalog.models : []
    ).filter((model) => PROVIDER_NAMES[model?.providerId]);
    if (connectionsMode) {
      connections = (
        Array.isArray(catalog.connections) ? catalog.connections : []
      ).filter(
        (item) => typeof item?.id === 'string' && CONNECTION_NAMES[item.kind],
      );
      savedConnections.textContent = '';
      savedConnections.hidden = false;
      for (const [index, connection] of connections.entries()) {
        const row = element('article', 'model-saved-connection');
        row.dataset.connectionId = connection.id;
        const selected =
          connection.selected === true ||
          catalog.selectedConnectionId === connection.id;
        row.append(
          connectionIcon(
            connection.kind === 'meadow-core' ||
              connection.kind === 'managed-api'
              ? 'meadow'
              : connection.kind === 'local-api'
                ? 'chip'
                : 'cloud',
          ),
        );
        row.append(
          element(
            'strong',
            '',
            `${connection.name || CONNECTION_NAMES[connection.kind]}${selected ? '（已選擇）' : ''}`,
          ),
        );
        row.append(
          element(
            'span',
            'settings-model-connection-note',
            `${Array.isArray(connection.models) ? connection.models.length : 0} 個模型 · ${CONNECTION_NAMES[connection.kind]}`,
          ),
        );
        const models = (
          Array.isArray(connection.models) ? connection.models : []
        ).filter((item) => typeof item?.id === 'string' && item.id);
        const modelList = element('div', 'settings-saved-model-list');
        modelList.setAttribute('role', 'group');
        modelList.setAttribute('aria-label', `${connection.name || '供應商'}模型`);
        for (const model of models) {
          const label = element('label', 'settings-model-choice');
          const check = element('input');
          check.type = 'checkbox';
          check.value = model.id;
          check.checked = model.enabled !== false;
          check.dataset.savedModel = model.id;
          const text = element('span');
          text.append(element('span', '', String(model.name || model.id)));
          if (selected && catalog.selectedModelId === model.id && check.checked)
            text.append(element('small', '', '目前模型'));
          label.append(check, text);
          check.addEventListener('change', () => {
            const ids = [...modelList.querySelectorAll('input:checked')].map((input) => input.value);
            void updateSavedModels(connection.id, ids, check.checked ? model.id : '');
          });
          modelList.appendChild(label);
        }
        const actions = element('div', 'settings-model-connection-actions');
        const edit = button('編輯連線');
        edit.dataset.connectionAction = 'edit';
        edit.addEventListener('click', () => {
          if (busy) return;
          invalidate();
          editingConnection = connection;
          replacementKeyRequired = false;
          provider.value = connection.kind;
          selectedBrand =
            connection.kind === 'provider-api'
              ? CLOUD_PROVIDERS.find(
                  (item) =>
                    item.id !== 'other' &&
                    item.baseUrl ===
                      String(connection.baseUrl || '').replace(/\/$/, ''),
                )?.id || 'other'
              : '';
          protocol.value =
            connection.protocol === 'anthropic'
              ? 'anthropic'
              : 'openai-compatible';
          connectionName.value = String(
            connection.name || CONNECTION_NAMES[connection.kind],
          );
          baseUrl.value = String(connection.baseUrl || '');
          resetCapabilities(connection);
          key.value = '';
          invitation.value = '';
          invitationFile.value = '';
          stored.checked = connection.configured === true;
          wizardVisible = true;
          renderProviderChoices();
          showStep(2);
          add.setAttribute('aria-expanded', 'true');
          setStatus('');
          updateControls();
          (connection.kind === 'meadow-core'
            ? verify
            : stored.checked
              ? verify
              : key
          ).focus();
        });
        actions.append(edit);
        row.append(modelList, actions);
        savedConnections.appendChild(row);
      }
      summary.textContent = !enabled
        ? '這個版本未開放連接模型。'
        : connections.length
          ? `已連接 ${connections.length} 個供應商，模型可從對話選單選擇。`
          : '先選擇模型來源，再連接供應商並選擇模型。';
    } else
      summary.textContent =
        catalog?.enabled === false
          ? '這個版本未開放自行連接模型供應商。'
          : models.length
            ? `已連接 ${models.length} 個 API 模型。新增模型不會切換正在使用的模型。`
            : '尚未連接 API 模型。你仍可使用已設定的本地模型。';
    if (!editingConnection) {
      selectedBrand = '';
      wizardVisible = connections.length === 0;
      showStep(connectionsMode ? 1 : 2);
    }
    renderProviderChoices();
    add.textContent = connections.length ? '新增供應商' : '設定模型';
    updateControls();
  };
  const updateSavedModels = async (connectionId, modelIds, preferredModelId) => {
    if (busy || !enabled) return;
    closeForm();
    const revision = ++generation;
    controller = new AbortController();
    busy = true;
    saving = true;
    summary.textContent = '正在儲存選擇的模型…';
    updateControls();
    try {
      const result = await providerRequest(
        `${PROVIDERS_PATH}/models`,
        { connectionId, modelIds, preferredModelId },
        controller.signal,
      );
      if (!active(revision)) return;
      if (
        result?.saved !== true ||
        !['applied', 'restart-required'].includes(result.activation)
      )
        throw new Error('selection_not_confirmed');
      const confirmation =
        result.activation === 'applied'
          ? '模型選擇已儲存。勾選的模型會顯示在對話中。'
          : RESTART_NOTICE;
      summary.textContent = confirmation;
      if (result.activation === 'applied')
        bus.emit('model_catalog_changed', {});
      try {
        const catalog = await providerRequest(
          PROVIDERS_PATH,
          undefined,
          controller.signal,
        );
        if (active(revision)) {
          renderCatalog(catalog);
          summary.textContent = confirmation;
        }
      } catch {
        // The mutation is confirmed even if the following read fails.
      }
    } catch (error) {
      if (active(revision) && error.name !== 'AbortError') {
        summary.textContent = safeError(error, 'save');
        const saved = connections.find((row) => row.id === connectionId);
        const row = [...savedConnections.children].find((node) => node.dataset.connectionId === connectionId);
        for (const input of row?.querySelectorAll('[data-saved-model]') || [])
          input.checked = saved?.models?.find((model) => model.id === input.value)?.enabled !== false;
      }
    } finally {
      if (active(revision)) {
        busy = false;
        saving = false;
        controller = null;
        updateControls();
      }
    }
  };
  const loadCatalog = async () => {
    const revision = ++generation;
    controller = new AbortController();
    try {
      const catalog = await providerRequest(
        PROVIDERS_PATH,
        undefined,
        controller.signal,
      );
      if (active(revision)) {
        renderCatalog(catalog);
        if (connectionsMode && enabled) {
          void discoverLocalCores();
          void loadManagedStatus();
        }
      }
    } catch (error) {
      if (active(revision) && error.name !== 'AbortError') {
        summary.textContent = '無法讀取模型供應商設定，請重新開啟此頁。';
        add.disabled = true;
      }
    }
  };
  const beginSource = (kind) => {
    if (busy || !enabled) return;
    invalidate();
    key.value = '';
    invitation.value = '';
    invitationFile.value = '';
    stored.checked = false;
    editingConnection = null;
    replacementKeyRequired = false;
    selectedBrand = '';
    provider.value = kind;
    connectionName.value = CONNECTION_NAMES[kind] || '';
    baseUrl.value = kind === 'local-api' ? 'http://127.0.0.1:8000/v1' : '';
    protocol.value = 'openai-compatible';
    resetCapabilities();
    advanced.open = false;
    coreImport.open = false;
    wizardVisible = true;
    renderProviderChoices();
    showStep(2);
    add.setAttribute('aria-expanded', 'true');
    setStatus('');
    updateControls();
    if (kind === 'meadow-core') {
      void discoverLocalCores();
      discoveryStatus.focus();
    } else if (kind === 'managed-api') {
      void loadManagedStatus();
      managedStatus.focus();
    } else if (kind === 'local-api') baseUrl.focus();
    else providerChoices.querySelector('input:not([disabled])')?.focus();
  };
  for (const tile of sourceChooser.querySelectorAll('[data-source]'))
    tile.addEventListener('click', () => beginSource(tile.dataset.source));
  const returnToSources = () => {
    if (saving) return;
    closeForm();
    wizardVisible = true;
    showStep(connectionsMode ? 1 : 2);
    renderProviderChoices();
    updateControls();
    (connectionsMode
      ? sourceChooser.querySelector('button:not([disabled])')
      : providerChoices.querySelector('input:not([disabled])')
    )?.focus();
  };
  backToSources.addEventListener('click', returnToSources);
  add.addEventListener('click', returnToSources);
  cancel.addEventListener('click', () => {
    if (saving) return;
    returnToSources();
    sourceChooser.querySelector('button')?.focus();
  });
  provider.addEventListener('change', selectProvider);
  connectionName.addEventListener('input', () => {
    invalidate();
    setStatus('');
    updateControls();
  });
  for (const [field, input] of [
    ['supportsImages', supportsImages],
    ['contextTokens', contextTokens],
    ['maxOutputTokens', maxOutputTokens],
  ]) {
    input.addEventListener(
      input === supportsImages ? 'change' : 'input',
      () => {
        invalidate();
        capabilityDirty[field] = true;
        setStatus('');
        updateControls();
      },
    );
  }
  protocol.addEventListener('change', () => {
    invalidate();
    key.value = '';
    stored.checked = false;
    replacementKeyRequired = editingConnection?.configured === true;
    setStatus('');
    updateControls();
  });
  baseUrl.addEventListener('input', () => {
    invalidate();
    key.value = '';
    stored.checked = false;
    replacementKeyRequired = editingConnection?.configured === true;
    resetCapabilities();
    setStatus('');
    updateControls();
  });
  stored.addEventListener('change', () => {
    invalidate();
    key.value = '';
    setStatus('');
    updateControls();
    if (!stored.checked) key.focus();
  });
  key.addEventListener('input', () => {
    invalidate();
    setStatus('');
    updateControls();
  });
  options.addEventListener('change', updateControls);
  selectAll.addEventListener('click', () => {
    if (busy) return;
    for (const input of options.querySelectorAll('input:not([disabled])'))
      input.checked = true;
    updateControls();
  });
  const presentVerification = (result) => {
    if (!result?.verificationId || !Array.isArray(result.models))
      throw new Error('invalid_verification');
    verificationId = result.verificationId;
    for (const model of result.models) {
      if (typeof model?.id !== 'string' || !model.id) continue;
      const label = element('label', 'settings-model-choice');
      const check = element('input');
      check.type = 'checkbox';
      check.value = model.id;
      check.dataset.configured = String(
        !connectionsMode && model.configured === true,
      );
      check.disabled = !connectionsMode && model.configured === true;
      check.checked =
        connectionsMode &&
        editingConnection?.models?.some((item) => item.id === model.id && item.enabled !== false) ===
          true;
      const name = element('span', '', String(model.name || model.id));
      const detail = element(
        'small',
        '',
        !connectionsMode && model.configured ? '已加入' : model.id,
      );
      const text = element('span');
      text.append(name, detail);
      label.append(check, text);
      options.appendChild(label);
    }
    choices.hidden = false;
    showStep(3);
    updateControls();
    options.querySelector('input:not([disabled])')?.focus();
    const available = options.querySelectorAll('input:not([disabled])').length;
    setStatus(
      available
        ? '驗證成功，請選擇要加入的模型。'
        : '驗證成功，這個供應商的可用模型都已加入。',
      'success',
    );
    const seconds = Number(result.expiresInSeconds) || 600;
    const verifiedToken = verificationId;
    expiryTimer = setTimeout(
      () => {
        if (
          disposed ||
          !section.isConnected ||
          verificationId !== verifiedToken
        )
          return;
        clearVerification();
        setStatus('驗證已過期，請重新驗證供應商。');
        updateControls();
      },
      Math.min(Math.max(seconds, 1), 600) * 1000,
    );
  };
  const verifyCore = async (path, payload) => {
    if (busy || !connectionsMode || provider.value !== 'meadow-core') return;
    for (const input of [contextTokens, maxOutputTokens]) {
      if (!input.reportValidity()) return;
    }
    Object.assign(payload, declaredCapabilities());
    invalidate();
    invitation.value = '';
    invitationFile.value = '';
    key.value = '';
    stored.checked = false;
    const revision = ++generation;
    controller = new AbortController();
    busy = true;
    setStatus('正在連接 Meadow Core 並取得可用模型…');
    updateControls();
    try {
      const result = await providerRequest(path, payload, controller.signal);
      if (!active(revision)) return;
      if (
        result?.connection?.kind !== 'meadow-core' ||
        typeof result.connection.name !== 'string' ||
        typeof result.connection.baseUrl !== 'string'
      )
        throw new Error('invalid_core_connection');
      editingConnection = null;
      replacementKeyRequired = false;
      connectionName.value = result.connection.name;
      baseUrl.value = result.connection.baseUrl;
      resetCapabilities({ models: result.models });
      if (Object.hasOwn(payload, 'supportsImages')) {
        supportsImages.checked = payload.supportsImages;
        supportsImages.indeterminate = false;
      }
      if (Object.hasOwn(payload, 'contextTokens'))
        contextTokens.value = String(payload.contextTokens);
      if (Object.hasOwn(payload, 'maxOutputTokens'))
        maxOutputTokens.value = String(payload.maxOutputTokens);
      presentVerification(result);
    } catch (error) {
      if (active(revision) && error.name !== 'AbortError')
        setStatus(safeError(error, 'verify'), 'error');
    } finally {
      delete payload.invitation;
      if (active(revision)) {
        busy = false;
        controller = null;
        updateControls();
      }
    }
  };
  invitation.addEventListener('input', () => {
    invalidate();
    invitationFile.value = '';
    setStatus('');
    updateControls();
  });
  pair.addEventListener('click', () => {
    if (pair.disabled) return;
    void verifyCore(`${PROVIDERS_PATH}/pair`, {
      invitation: invitation.value.trim(),
    });
  });
  importInvitation.addEventListener('click', () => {
    if (!busy) invitationFile.click();
  });
  invitationFile.addEventListener('change', async () => {
    const file = invitationFile.files?.[0];
    invitationFile.value = '';
    if (!file || busy) return;
    invalidate();
    invitation.value = '';
    key.value = '';
    stored.checked = false;
    const revision = ++generation;
    busy = true;
    setStatus('正在讀取 Core 連線檔…');
    updateControls();
    try {
      if (file.size > 32 * 1024) throw new Error('connection_file_too_large');
      const content = JSON.parse(await file.text());
      if (!content || typeof content !== 'object' || Array.isArray(content))
        throw new Error('connection_file_invalid');
      if (!active(revision)) return;
      invitation.value = JSON.stringify(content);
      setStatus('連線檔已載入，請按「配對並取得模型」繼續。');
    } catch {
      if (active(revision))
        setStatus(
          '無法讀取連線檔，請選擇有效且不超過 32 KiB 的 .meadow-connection JSON 檔案。',
          'error',
        );
    } finally {
      if (active(revision)) {
        busy = false;
        updateControls();
      }
    }
  });
  const discoverLocalCores = async () => {
    if (disposed || !connectionsMode || !enabled || discovering) return;
    discoveryController?.abort();
    const revision = ++discoveryRevision;
    discoveryController = new AbortController();
    discovering = true;
    discoveryStatus.textContent = '正在偵測這台電腦的 Meadow Core…';
    updateControls();
    const current = () =>
      !disposed && section.isConnected && discoveryRevision === revision;
    try {
      const result = await providerRequest(
        `${PROVIDERS_PATH}/local-core`,
        undefined,
        discoveryController.signal,
      );
      if (!current()) return;
      detectedCores.textContent = '';
      const cores = (Array.isArray(result?.cores) ? result.cores : []).filter(
        (core) => typeof core?.coreId === 'string' && core.coreId,
      );
      for (const core of cores) {
        const existing = connections.find((item) => item.kind === 'meadow-core' && sameCoreEndpoint(item.baseUrl, core.baseUrl));
        if (existing) {
          const linked = element('p', 'settings-model-connection-note', 'Meadow Core 已連接，模型已列在上方。');
          linked.dataset.coreId = core.coreId;
          detectedCores.appendChild(linked);
          continue;
        }
        const connect = button(`連接 ${String(core.name || 'Meadow Core')}`);
        connect.dataset.coreId = core.coreId;
        connect.addEventListener('click', () => {
          if (busy || !enabled) return;
          if (form.hidden || provider.value !== 'meadow-core') {
            beginSource('meadow-core');
            provider.value = 'meadow-core';
            connectionName.value = '';
            baseUrl.value = '';
            resetCapabilities();
            wizardVisible = true;
            showStep(2);
            add.setAttribute('aria-expanded', 'true');
          }
          void verifyCore(`${PROVIDERS_PATH}/local-core/verify`, {
            coreId: core.coreId,
          });
        });
        detectedCores.append(
          connect,
          element(
            'p',
            'settings-model-connection-note',
            String(core.baseUrl || ''),
          ),
        );
      }
      const detection = result?.detection;
      // Inventory and public health do not establish a paired Core identity.
      // Use closed local copy, never paths or server-supplied diagnostic text.
      const detectionMessages = {
        'legacy-runtime':
          '找到舊版 Core 啟動設定。可啟動原服務後，選擇「其他本地 API」連接。',
        'running-unverified':
          '偵測到運作中的 Core 服務，尚未確認配對。請匯入連線檔，或使用「其他本地 API」。',
        'installed-offline':
          '已找到 Core 安裝記錄，目前尚未連線。啟動 Core 後再按「重新偵測」。',
        'paired-offline': '已找到 Core 配對記錄；請驗證連線後取得模型。',
        'paired-ready': '已確認本機 Core 身分，點選連接即可取得模型。',
        'not-detected':
          '尚未找到可配對的 Core。可啟動 Core 後重新偵測，或連接既有的本地 API。',
        unavailable: '目前無法讀取本機 Core 安裝狀態。仍可匯入 Core 連線檔。',
      };
      const knownDetection =
        typeof detection?.state === 'string' &&
        Object.hasOwn(detectionMessages, detection.state)
          ? detectionMessages[detection.state]
          : '';
      discoveryStatus.textContent =
        (cores.some((core) => connections.some((item) => item.kind === 'meadow-core' && sameCoreEndpoint(item.baseUrl, core.baseUrl))) ? '已連接這台電腦的 Meadow Core。直接勾選上方模型即可使用。' : '') || knownDetection ||
        (cores.length
          ? '找到 Core 配對連線，請驗證並取得可用模型。'
          : detection?.installed === true
            ? detection.running === true
              ? 'Meadow Core 已安裝並執行，但尚未配對。請匯入 Core 連線檔。'
              : 'Meadow Core 已安裝，目前尚未執行。啟動後再按「重新偵測」。'
            : '尚未找到可連接的 Meadow Core。可啟動 Core 後重新偵測，或匯入連線檔。');
      detect.textContent = '重新偵測';
    } catch (error) {
      if (current() && error.name !== 'AbortError')
        discoveryStatus.textContent = safeError(error, 'verify');
    } finally {
      if (current()) {
        discovering = false;
        discoveryController = null;
        updateControls();
      }
    }
  };
  detect.addEventListener('click', () => void discoverLocalCores());
  const loadManagedStatus = async () => {
    managedController?.abort();
    const revision = ++managedRevision;
    managedController = new AbortController();
    const current = () =>
      !disposed && section.isConnected && managedRevision === revision;
    try {
      const result = await providerRequest(
        `${PROVIDERS_PATH}/managed-status`,
        undefined,
        managedController.signal,
      );
      if (!current()) return;
      managedAvailable = result?.available === true;
      managedConfigured = result?.status?.configured === true;
      managedAllowed = result?.status?.allowed === true;
      managedStatus.textContent = !managedConfigured
        ? 'Meadow API 尚未設定，你仍可連接自己的 API。'
        : !managedAvailable
          ? 'Meadow API 尚未開放，你仍可連接自己的 API。'
          : !managedAllowed
            ? '目前會員方案尚未開放 Meadow API，請確認訂閱或 API 額度。'
            : 'Meadow API 已開放，可連接會員模型。';
      const amounts = [];
      const usd = new Intl.NumberFormat('en-US', {
        style: 'currency',
        currency: 'USD',
        maximumFractionDigits: 4,
      });
      const usage = result?.usage?.currency === 'USD' ? result.usage : null;
      const budget = result?.status?.budget;
      const maximum =
        typeof usage?.maxBudget === 'number'
          ? usage.maxBudget
          : budget?.currency === 'USD' &&
              budget.unit === 'money' &&
              typeof budget.maxBudgetUsdMicros === 'number'
            ? budget.maxBudgetUsdMicros / 1_000_000
            : null;
      if (typeof usage?.spend === 'number' && Number.isFinite(usage.spend))
        amounts.push(`已使用 ${usd.format(usage.spend)}`);
      if (typeof maximum === 'number' && Number.isFinite(maximum))
        amounts.push(`預算 ${usd.format(maximum)}`);
      if (
        typeof usage?.remaining === 'number' &&
        Number.isFinite(usage.remaining)
      )
        amounts.push(`剩餘 ${usd.format(usage.remaining)}`);
      managedUsage.textContent = amounts.length
        ? `${amounts.join(' · ')}（USD）`
        : '用量資料尚未提供。';
    } catch (error) {
      if (current() && error.name !== 'AbortError') {
        managedAvailable = false;
        managedConfigured = false;
        managedAllowed = false;
        managedStatus.textContent =
          '無法取得 Meadow API 狀態，請稍後重新開啟設定。你仍可連接自己的 API。';
        managedUsage.textContent = '';
      }
    } finally {
      if (current()) {
        managedController = null;
        updateControls();
      }
    }
  };
  managedConnect.addEventListener('click', async () => {
    if (
      managedConnect.disabled ||
      !connectionsMode ||
      provider.value !== 'managed-api'
    )
      return;
    invalidate();
    key.value = '';
    invitation.value = '';
    invitationFile.value = '';
    stored.checked = false;
    const revision = ++generation;
    controller = new AbortController();
    busy = true;
    setStatus('正在取得 Meadow API 模型…');
    updateControls();
    try {
      const result = await providerRequest(
        `${PROVIDERS_PATH}/managed-connect`,
        {},
        controller.signal,
      );
      if (!active(revision)) return;
      if (
        result?.connection?.kind !== 'managed-api' ||
        typeof result.connection.name !== 'string' ||
        typeof result.connection.baseUrl !== 'string'
      )
        throw new Error('invalid_managed_connection');
      editingConnection = null;
      replacementKeyRequired = false;
      connectionName.value = result.connection.name;
      baseUrl.value = result.connection.baseUrl;
      resetCapabilities({ models: result.models });
      presentVerification(result);
    } catch (error) {
      if (active(revision) && error.name !== 'AbortError')
        setStatus(safeError(error, 'verify'), 'error');
    } finally {
      if (active(revision)) {
        busy = false;
        controller = null;
        updateControls();
      }
    }
  });
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (busy || verify.disabled) return;
    clearVerification();
    const revision = ++generation;
    controller = new AbortController();
    const payload = connectionsMode
      ? {
          ...(editingConnection ? { connectionId: editingConnection.id } : {}),
          kind: provider.value,
          name: connectionName.value.trim(),
          baseUrl: baseUrl.value.trim(),
          ...(provider.value === 'provider-api'
            ? { protocol: protocol.value }
            : {}),
          useStoredKey: stored.checked,
          ...declaredCapabilities(),
        }
      : { provider: provider.value, useStoredKey: stored.checked };
    if (!stored.checked && (!connectionsMode || key.value.trim()))
      payload.apiKey = key.value.trim();
    key.value = '';
    invitation.value = '';
    invitationFile.value = '';
    busy = true;
    setStatus('正在連接供應商並取得可用模型…');
    updateControls();
    try {
      const result = await providerRequest(
        `${PROVIDERS_PATH}/verify`,
        payload,
        controller.signal,
      );
      if (!active(revision)) return;
      key.value = '';
      presentVerification(result);
    } catch (error) {
      if (active(revision) && error.name !== 'AbortError')
        setStatus(safeError(error, 'verify'), 'error');
    } finally {
      delete payload.apiKey;
      if (active(revision)) {
        busy = false;
        controller = null;
        updateControls();
      }
    }
  });
  save.addEventListener('click', async () => {
    if (busy || save.disabled) return;
    const revision = ++generation;
    controller = new AbortController();
    busy = true;
    saving = true;
    let completed = false;
    setStatus('正在儲存模型…');
    updateControls();
    try {
      const result = await providerRequest(
        `${PROVIDERS_PATH}/save`,
        { verificationId, modelIds: selectedModels() },
        controller.signal,
      );
      if (!active(revision)) return;
      if (result?.saved !== true) throw new Error('save_not_confirmed');
      if (
        connectionsMode &&
        !['applied', 'restart-required'].includes(result.activation)
      )
        throw new Error('activation_not_confirmed');
      key.value = '';
      clearVerification();
      wizardVisible = false;
      showStep(1);
      add.setAttribute('aria-expanded', 'false');
      const confirmation =
        connectionsMode && result.activation === 'restart-required'
          ? RESTART_NOTICE
          : '設定完成，模型已加入對話選單。';
      summary.textContent = confirmation;
      completed = true;
      if (!connectionsMode || result.activation === 'applied')
        bus.emit('model_catalog_changed', {});
      // Refresh configured flags for a subsequent addition, keeping the
      // explicit save confirmation visible until the user opens the form.
      try {
        const catalog = await providerRequest(
          PROVIDERS_PATH,
          undefined,
          controller.signal,
        );
        if (active(revision)) {
          if (connectionsMode) {
            renderCatalog(catalog);
            summary.textContent = confirmation;
            editingConnection = null;
            replacementKeyRequired = false;
            stored.checked = false;
            wizardVisible = false;
            showStep(1);
          } else
            providers = (
              Array.isArray(catalog?.providers) ? catalog.providers : []
            ).filter((item) => PROVIDER_NAMES[item?.id]);
        }
      } catch {
        // A failed catalog refresh does not undo a confirmed save.
      }
    } catch (error) {
      if (active(revision) && error.name !== 'AbortError')
        setStatus(safeError(error, 'save'), 'error');
    } finally {
      if (active(revision)) {
        busy = false;
        saving = false;
        controller = null;
        updateControls();
        if (completed) add.focus();
      }
    }
  });
  void loadCatalog();
  return () => {
    disposed = true;
    invalidate();
    discoveryRevision += 1;
    discoveryController?.abort();
    discoveryController = null;
    managedRevision += 1;
    managedController?.abort();
    managedController = null;
    key.value = '';
    invitation.value = '';
    invitationFile.value = '';
    providers = [];
    connections = [];
    editingConnection = null;
  };
}
