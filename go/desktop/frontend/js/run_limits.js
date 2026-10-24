// New prompts/queue items snapshot time preferences and the confirmed selected
// effort. Thinking Off changes inference, not the selected task token allowance.
const KEY = 'meadow.runLimits.v1'; // gitleaks:allow -- Fixed localStorage preference namespace, not a credential.
const EVO_TOKEN_KEY = 'meadow.evoTokenBudget.v1';
const EVO_TOKEN_PRESETS = Object.freeze({
  effort: { label: '沿用 Effort', tokens: 0 },
  extended: { label: '200 萬 tokens', tokens: 2000000 },
  generous: { label: '300 萬 tokens', tokens: 3000000 },
});
let confirmedExecutionMode = '';
export const RUN_LIMIT_PRESETS = Object.freeze({
  brief: { label: '5 分鐘', runTimeoutMs: 300000 },
  standard: { label: '10 分鐘', runTimeoutMs: 600000 },
  extended: { label: '20 分鐘', runTimeoutMs: 1200000 },
  evoExtended: { label: '60 分鐘（Evo）', runTimeoutMs: 3600000, evoOnly: true },
});
let evoRunLimitAvailable = false;

function availablePreset(key) {
  return Object.hasOwn(RUN_LIMIT_PRESETS, key) &&
    (!RUN_LIMIT_PRESETS[key].evoOnly || evoRunLimitAvailable);
}

function fillRunLimitControl(select) {
  select.replaceChildren();
  for (const [key, preset] of Object.entries(RUN_LIMIT_PRESETS)) {
    if (!availablePreset(key)) continue;
    const option = document.createElement('option');
    option.value = key; option.textContent = preset.label; select.append(option);
  }
  select.value = selectedPreset();
}

// Availability comes from the current session's confirmed native capabilities.
// A late read can update an already open settings panel; it never saves a new
// preference or mutates the options snapshotted by an admitted task/queue item.
export function setEvoRunLimitAvailability(available) {
  const next = available === true;
  if (evoRunLimitAvailable === next) return;
  evoRunLimitAvailable = next;
  const select = typeof document === 'undefined' ? null : document.getElementById('runBudgetPreset');
  if (select) fillRunLimitControl(select);
}

// A host advertising Evo is insufficient: the current session must actually
// select it. This state affects new snapshots and the settings UI only.
export function setRunLimitExecutionMode(mode) {
  confirmedExecutionMode = mode || '';
  const select = typeof document === 'undefined' ? null : document.getElementById('evoTokenBudgetPreset');
  if (select) select.disabled = confirmedExecutionMode !== 'evo';
}
export const EFFORT_TOKEN_BUDGETS = Object.freeze({
  minimal: 150000,
  low: 250000,
  medium: 500000,
  high: 750000,
  max: 1000000,
});
export function runTokenBudget(effort = 'medium') {
  return Object.hasOwn(EFFORT_TOKEN_BUDGETS, effort)
    ? EFFORT_TOKEN_BUDGETS[effort]
    : EFFORT_TOKEN_BUDGETS.medium;
}
export function runTokenBudgetLabel(effort) {
  return `${runTokenBudget(effort) / 10000} 萬 tokens`;
}
function selectedPreset() {
  try {
    const value = localStorage.getItem(KEY);
    return availablePreset(value) ? value : 'standard';
  } catch { return 'standard'; }
}
function selectedEvoTokenPreset() {
  try {
    const value = localStorage.getItem(EVO_TOKEN_KEY);
    return Object.hasOwn(EVO_TOKEN_PRESETS, value) ? value : 'effort';
  } catch { return 'effort'; }
}
export function snapshotRunLimitPreferences() {
  return Object.freeze({
    runTimeoutMs: RUN_LIMIT_PRESETS[selectedPreset()].runTimeoutMs,
    evoTokenPreset: selectedEvoTokenPreset(),
  });
}
export function runLimitOptions(effort = 'medium', preferences = snapshotRunLimitPreferences(), effectiveMode = confirmedExecutionMode) {
  const evoTokens = effectiveMode === 'evo'
    ? EVO_TOKEN_PRESETS[preferences.evoTokenPreset]?.tokens : 0;
  return { runTimeoutMs: preferences.runTimeoutMs,
    runTokenLimit: evoTokens || runTokenBudget(effort) };
}
export function buildRunLimitControl() {
  const select = document.createElement('select');
  select.id = 'runBudgetPreset';
  select.setAttribute('aria-label', '每次任務的時間上限');
  fillRunLimitControl(select);
  select.addEventListener('change', () => {
    if (!availablePreset(select.value)) { select.value = selectedPreset(); return; }
    try { localStorage.setItem(KEY, select.value); }
    catch { select.value = selectedPreset(); }
  });
  return select;
}

export function buildEvoTokenBudgetControl() {
  const select = document.createElement('select');
  select.id = 'evoTokenBudgetPreset';
  select.setAttribute('aria-label', 'Evo 每次任務的 Token 上限');
  for (const [key, preset] of Object.entries(EVO_TOKEN_PRESETS)) {
    const option = document.createElement('option');
    option.value = key; option.textContent = preset.label; select.append(option);
  }
  select.value = selectedEvoTokenPreset();
  select.disabled = confirmedExecutionMode !== 'evo';
  select.addEventListener('change', () => {
    if (select.disabled || !Object.hasOwn(EVO_TOKEN_PRESETS, select.value)) {
      select.value = selectedEvoTokenPreset(); return;
    }
    try { localStorage.setItem(EVO_TOKEN_KEY, select.value); }
    catch { select.value = selectedEvoTokenPreset(); }
  });
  return select;
}
