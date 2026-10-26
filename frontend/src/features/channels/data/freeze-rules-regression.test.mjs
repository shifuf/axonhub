import assert from 'node:assert/strict';
import test from 'node:test';
import { graphqlDocuments, loadCallback, loadTS } from '../../../../tests/unit-loader.mjs';

const schema = loadTS('features/channels/data/schema.ts');
const helpers = loadTS('features/channels/data/auto-disable.ts');
const base = { statusCodes: [429], times: 3, action: 'temporary_disable', disableDurationMinutes: 47 };

const canonical = { ...base, windowMinutes: 10, freezeModel: true };

test('automatic forms require a positive integer window, model-only target and temporary action', () => {
  for (const windowMinutes of [1, 10, 10080]) {
    assert.equal(schema.apiKeyAutoDisableRuleFormSchema.safeParse({ ...canonical, windowMinutes }).success, true);
  }
  for (const windowMinutes of [undefined, null, 0, -1, 0.5, 10081, NaN, Infinity]) {
    assert.equal(schema.apiKeyAutoDisableRuleFormSchema.safeParse({ ...canonical, windowMinutes }).success, false, `reject window ${windowMinutes}`);
  }
  for (const action of ['', 'disable_until_cron', 'permanent_disable', 'permanent_disable_delete']) {
    assert.equal(schema.apiKeyAutoDisableRuleFormSchema.safeParse({ ...canonical, action, disableUntilCron: '0 0 * * *' }).success, false, `reject action ${action}`);
  }
  for (const freezeModel of [undefined, null, false]) {
    assert.equal(schema.apiKeyAutoDisableRuleFormSchema.safeParse({ ...canonical, freezeModel }).success, false, `reject target ${freezeModel}`);
  }
});

test('automatic thresholds have no upper clamp and duration is always a positive integer', () => {
  for (const times of [1, 5, 6, 99, 1000000]) {
    assert.equal(schema.apiKeyAutoDisableRuleFormSchema.safeParse({ ...canonical, times }).success, true);
    assert.equal(helpers.serializeApiKeyAutoDisableRules([{ ...canonical, times }])[0].times, times);
  }
  for (const field of ['times', 'disableDurationMinutes']) {
    for (const value of [undefined, null, 0, -1, 0.5, NaN, Infinity]) {
      assert.equal(schema.apiKeyAutoDisableRuleFormSchema.safeParse({ ...canonical, [field]: value }).success, false, `reject ${field} ${value}`);
    }
  }
  for (const disableDurationMinutes of [1, 47, 10081]) {
    assert.equal(schema.apiKeyAutoDisableRuleFormSchema.safeParse({ ...canonical, disableDurationMinutes }).success, true);
  }
  for (const rule of [
    { ...canonical, statusCodes: [], keywordPatterns: [] },
    { ...canonical, statusCodes: [], keywordPatterns: ['  '] },
    { ...canonical, statusCodes: [99] },
    { ...canonical, statusCodes: [600] },
    { ...canonical, statusCodes: [429.5] },
  ]) assert.equal(schema.areApiKeyAutoDisableRulesValid([rule]), false);
  assert.equal(schema.areApiKeyAutoDisableRulesValid([{ ...canonical, statusCodes: [], keywordPatterns: ['quota.*exceeded'] }]), true);
});

test('legacy wire actions including empty or unset decode and reopen as safe canonical rules', () => {
  for (const action of [undefined, null, '', 'temporary_disable', 'disable_until_cron', 'permanent_disable', 'permanent_disable_delete']) {
    for (const windowMinutes of [undefined, null, 0]) {
      const persisted = schema.apiKeyAutoDisableRuleSchema.parse({ statusCodes: [401], times: 8, action, windowMinutes, freezeModel: false });
      const echoed = helpers.toApiKeyAutoDisableRuleFormValues([persisted])[0];
      assert.equal(echoed.action, 'temporary_disable', `migrate ${action}`);
      assert.equal(echoed.freezeModel, true);
      assert.equal(echoed.windowMinutes, 10);
      assert.equal(echoed.disableDurationMinutes, 30);
      assert.equal(echoed.disableUntilCron, null);
      assert.equal(echoed.disableUntilTimezone, null);
      assert.equal(echoed.times, 8);
      assert.equal(schema.areApiKeyAutoDisableRulesValid([echoed]), true);
      assert.deepEqual(helpers.serializeApiKeyAutoDisableRules([echoed])[0], echoed);
    }
  }
  for (const action of ['temporary_disable', 'disable_until_cron', 'permanent_disable', 'permanent_disable_delete']) {
    assert.equal(schema.apiKeyAutoDisableActionSchema.safeParse(action).success, true, 'wire enums remain available');
  }
  assert.equal(schema.apiKeyAutoDisableRuleSchema.safeParse({ ...canonical, action: 'unknown' }).success, false);
});

test('canonical serialization preserves custom positive timing and patterns but never old actions, targets or schedules', () => {
  const rules = ['temporary_disable', 'disable_until_cron', 'permanent_disable', 'permanent_disable_delete', ''].map((action) => ({
    ...base, action, times: 12, windowMinutes: 10080, freezeModel: false,
    keywordPatterns: [' quota.*exceeded ', ''], disableUntilCron: '17 4 * * 2', disableUntilTimezone: 'Pacific/Chatham',
  }));
  for (const normalized of [helpers.toApiKeyAutoDisableRuleFormValues(rules), helpers.serializeApiKeyAutoDisableRules(rules)]) {
    assert.ok(normalized.every((rule) => rule.action === 'temporary_disable' && rule.freezeModel === true));
    assert.ok(normalized.every((rule) => rule.windowMinutes === 10080 && rule.disableDurationMinutes === 47 && rule.times === 12));
    assert.ok(normalized.every((rule) => rule.disableUntilCron === null && rule.disableUntilTimezone === null));
  }
  assert.deepEqual(helpers.serializeApiKeyAutoDisableRules(rules)[0].keywordPatterns, ['quota.*exceeded']);
  const zeroDuration = schema.apiKeyAutoDisableRuleSchema.parse({ ...canonical, disableDurationMinutes: 0 });
  assert.equal(helpers.toApiKeyAutoDisableRuleFormValues([zeroDuration])[0].disableDurationMinutes, 30);
});

test('legacy normalization does not hide invalid nonlegacy numeric values', () => {
  for (const field of ['windowMinutes', 'times', 'disableDurationMinutes']) {
    for (const value of [-1, 0.5, NaN, Infinity, ...(field === 'windowMinutes' ? [10081] : [])]) {
      const echoed = helpers.toApiKeyAutoDisableRuleFormValues([{ ...canonical, [field]: value }])[0];
      assert.ok(Object.is(echoed[field], value), `${field} ${value} must remain visible`);
      assert.equal(schema.areApiKeyAutoDisableRulesValid([echoed]), false);
    }
  }
});

test('every channel rules read/write response and global edit query selects the roundtrip fields', () => {
  const docs = graphqlDocuments('features/channels/data/channels.ts');
  const selections = Object.values(docs).filter((text) => text.includes('apiKeyAutoDisableRules'));
  assert.ok(selections.length >= 7);
  for (const doc of selections) {
    for (const selection of doc.matchAll(/apiKeyAutoDisableRules\s*\{([^}]+)\}/g)) {
      assert.match(selection[1], /\bwindowMinutes\b/, doc.slice(0, 80));
      assert.match(selection[1], /\bfreezeModel\b/, doc.slice(0, 80));
    }
  }
  const system = graphqlDocuments('features/system/data/system.ts');
  assert.match(system.RETRY_POLICY_QUERY, /rules\s*\{[^}]*windowMinutes[^}]*freezeModel/);
});


const submitPaths = [
  ['features/channels/components/channels-availability-dialog.tsx', 'onSubmit'],
  ['features/channels/components/channels-bulk-auto-disable-dialog.tsx', 'handleSubmit'],
  ['features/system/components/retry-settings.tsx', 'handleSubmit'],
];

function submitBindings(rules, mutate) {
  return {
    useCallback: (fn) => fn, areApiKeyAutoDisableRulesValid: schema.areApiKeyAutoDisableRulesValid,
    mode: 'custom', rules, currentPolicies: { stream: 'forbid' }, currentRow: { id: 'channel-guid' },
    selectedChannels: [{ id: 'channel-guid' }], action: 'write_rules',
    buildBulkAutoDisableInput: helpers.buildBulkAutoDisableInput, availabilityPoliciesPayload: helpers.availabilityPoliciesPayload,
    updateChannel: { mutateAsync: mutate }, mutation: { mutateAsync: mutate }, updateRetryPolicy: { mutateAsync: mutate },
    formData: { autoDisableChannel: { enabled: true, rules, permanentFailureThreshold: 6, permanentFailureWindowMinutes: 24 * 60 } },
    DEFAULT_PERMANENT_FAILURE_WINDOW_MINUTES: 24 * 60,
    toApiKeyAutoDisableRuleFormValues: helpers.toApiKeyAutoDisableRuleFormValues,
    serializeApiKeyAutoDisableRules: helpers.serializeApiKeyAutoDisableRules,
    modelOnlyClientCancellationPolicy: (policy) => policy,
    toast: { error() {}, success() {} }, t: (key) => key, onOpenChange() {}, resetRowSelection() {}, handleClose() {},
  };
}

test('single-channel, bulk and global submit reject invalid raw rules before normalization or mutation', async () => {
  for (const invalid of [
    { ...canonical, windowMinutes: 0 }, { ...canonical, windowMinutes: null }, { ...canonical, windowMinutes: 10081 },
    { ...canonical, windowMinutes: 0.5 }, { ...canonical, windowMinutes: NaN },
    { ...canonical, times: 0 }, { ...canonical, times: 1.5 },
    { ...canonical, disableDurationMinutes: null }, { ...canonical, disableDurationMinutes: 0 }, { ...canonical, disableDurationMinutes: -1 },
    { ...canonical, freezeModel: false }, { ...canonical, action: 'permanent_disable' },
    { ...canonical, action: 'disable_until_cron', disableUntilCron: '0 0 * * *' },
    { ...canonical, action: 'permanent_disable_delete', freezeModel: false },
    { ...canonical, statusCodes: [], keywordPatterns: [' '] },
  ]) {
    for (const [path, name] of submitPaths) {
      let mutations = 0;
      await loadCallback(path, name, submitBindings([invalid], async () => mutations++))({ preventDefault() {} });
      assert.equal(mutations, 0, `${path} should not submit ${JSON.stringify(invalid)}`);
    }
  }
});

test('single-channel, bulk and global submit canonical loaded legacy rules without changing stream or thresholds', async () => {
  const rules = helpers.toApiKeyAutoDisableRuleFormValues([{ statusCodes: [401], times: 9, action: 'permanent_disable_delete', windowMinutes: 0 }]);
  for (const [path, name] of submitPaths) {
    const payloads = [];
    await loadCallback(path, name, submitBindings(rules, async (input) => payloads.push(input)))({ preventDefault() {} });
    assert.equal(payloads.length, 1, path);
    const payload = payloads[0];
    const rule = (payload.input?.policies?.apiKeyAutoDisableRules ?? payload.rules ?? payload.autoDisableChannel?.rules)[0];
    assert.equal(rule.action, 'temporary_disable', path);
    assert.equal(rule.freezeModel, true, path);
    assert.equal(rule.windowMinutes, 10, path);
    assert.equal(rule.disableDurationMinutes, 30, path);
    assert.equal(rule.times, 9, path);
    if (payload.input) assert.equal(payload.input.policies.stream, 'forbid');
  }
});

test('bulk copy migrates old rules before editing without mutating the source', () => {
  const sourceRules = [{ statusCodes: [401], times: 3, action: 'permanent_disable_delete', windowMinutes: 0, freezeModel: false }];
  const snapshot = structuredClone(sourceRules);
  let copied;
  loadCallback('features/channels/components/channels-bulk-auto-disable-dialog.tsx', 'handleCopyFrom', {
    copySources: [{ id: 'source', policies: { apiKeyAutoDisableRules: sourceRules } }],
    toApiKeyAutoDisableRuleFormValues: helpers.toApiKeyAutoDisableRuleFormValues,
    setRules: (value) => { copied = value; }, setCopySearchOpen() {}, toast: { error() {} }, t: (key) => key,
  })('source');
  assert.equal(copied[0].action, 'temporary_disable');
  assert.equal(copied[0].freezeModel, true);
  assert.equal(copied[0].windowMinutes, 10);
  assert.equal(copied[0].disableDurationMinutes, 30);
  assert.deepEqual(sourceRules, snapshot);
});

test('canonical serialization refuses invalid nonlegacy timing or thresholds instead of emitting unsafe rules', () => {
  for (const invalid of [
    { ...canonical, windowMinutes: -1 }, { ...canonical, windowMinutes: 0.5 }, { ...canonical, windowMinutes: 10081 },
    { ...canonical, times: 0 }, { ...canonical, times: 1.5 },
    { ...canonical, disableDurationMinutes: -1 }, { ...canonical, disableDurationMinutes: 0.5 },
    { ...canonical, statusCodes: [], keywordPatterns: [] },
  ]) {
    assert.throws(() => helpers.serializeApiKeyAutoDisableRules([invalid]), undefined, JSON.stringify(invalid));
  }
});

test('availability off mode cannot bypass validation by saving an invalid draft as inactive rules', async () => {
  for (const invalid of [{ ...canonical, windowMinutes: -1 }, { ...canonical, disableDurationMinutes: null }]) {
    let mutations = 0;
    await loadCallback(submitPaths[0][0], 'onSubmit', { ...submitBindings([invalid], async () => mutations++), mode: 'off' })({ preventDefault() {} });
    assert.equal(mutations, 0, 'off mode persists rules, so it must validate the draft before mutation');
  }
});

test('availability inherit discards an invalid draft and keeps the stream policy', async () => {
  const payloads = [];
  await loadCallback(submitPaths[0][0], 'onSubmit', {
    ...submitBindings([{ ...canonical, windowMinutes: -1 }], async (input) => payloads.push(input)), mode: 'inherit',
  })({ preventDefault() {} });
  assert.equal(payloads.length, 1);
  assert.deepEqual(payloads[0].input.policies, { stream: 'forbid', apiKeyAutoDisableMode: 'inherit', apiKeyAutoDisableRules: null });
});
