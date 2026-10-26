import assert from 'node:assert/strict';
import test from 'node:test';
import { loadCallback, loadFunctions, loadTS, readSource } from '../../../../tests/unit-loader.mjs';

const path = 'features/system/components/retry-settings.tsx';
const helpers = loadTS('features/channels/data/auto-disable.ts');
const schema = loadTS('features/channels/data/schema.ts');
const source = readSource(path);

function cancellationNormalizer() {
  assert.match(source, /function modelOnlyClientCancellationPolicy\(/, 'cancellation policies need a shared model-only read/save normalizer');
  return loadFunctions(path, ['modelOnlyClientCancellationPolicy']).modelOnlyClientCancellationPolicy;
}

test('cancellation UI only offers a model cooldown, while scope and project override controls remain', () => {
  assert.doesNotMatch(source, /SelectItem value='(?:switch_and_cooldown|cooldown_only)'/);
  assert.doesNotMatch(source, /action:.*'switch_and_cooldown'/);
  assert.match(source, /data-testid='client-cancellation-model-action'/);
  assert.match(source, /client-cancellation-scope/);
  assert.match(source, /client-cancellation-project-enabled/);
  assert.match(source, /activeProjectOverride/);
});

test('legacy cancellation actions normalize globally and per project without losing gates, scope or evidence', () => {
  const normalize = cancellationNormalizer();
  const policy = {
    enabled: false, threshold: 4, windowMinutes: 19, cooldownMinutes: 47, action: 'cooldown_only', scope: 'project_first',
    upstreamEvidenceGate: { requireBeforeHeaders: true, minElapsedSeconds: 30 },
    projectOverrides: [
      { projectId: 42, enabled: true, threshold: 7, windowMinutes: 13, cooldownMinutes: 11, action: 'switch_and_cooldown', evidenceGate: 'required' },
      { projectId: 43, enabled: false, threshold: 8, windowMinutes: 9, cooldownMinutes: 12, action: 'cooldown_only' },
    ],
  };
  const snapshot = structuredClone(policy);
  const result = normalize(policy);
  assert.equal(result.action, 'cooldown_model');
  assert.deepEqual(result, { ...policy, action: 'cooldown_model', projectOverrides: policy.projectOverrides.map((override) => ({ ...override, action: 'cooldown_model' })) });
  assert.deepEqual(policy, snapshot, 'read normalization must not mutate cached policy');
  assert.equal(normalize(undefined).action, 'cooldown_model');
});

test('retry submit canonicalizes cancellation actions and preserves non-cancellation settings and gates', async () => {
  const normalize = cancellationNormalizer();
  const payloads = [];
  const formData = {
    enabled: true, retryDelayMs: 1234, emptyResponseDetection: true,
    autoDisableChannel: { enabled: true, permanentFailureWindowMinutes: 360, rules: helpers.toApiKeyAutoDisableRuleFormValues([{ statusCodes: [401], times: 8, action: 'permanent_disable_delete', windowMinutes: 0 }]) },
    clientCancellationPolicy: {
      enabled: true, threshold: 4, windowMinutes: 17, cooldownMinutes: 47, scope: 'project_first', action: 'cooldown_only',
      evidenceGate: { enabled: true },
      projectOverrides: [{ projectId: 42, enabled: false, threshold: 6, windowMinutes: 9, cooldownMinutes: 11, action: 'switch_and_cooldown' }],
    },
  };
  await loadCallback(path, 'handleSubmit', {
    useCallback: (fn) => fn, formData, updateRetryPolicy: { mutateAsync: async (input) => payloads.push(input) },
    areApiKeyAutoDisableRulesValid: schema.areApiKeyAutoDisableRulesValid,
    serializeApiKeyAutoDisableRules: helpers.serializeApiKeyAutoDisableRules,
    modelOnlyClientCancellationPolicy: normalize, toast: { error() {} }, t: (key) => key,
  })({ preventDefault() {} });
  assert.equal(payloads.length, 1);
  assert.deepEqual(payloads[0], { ...formData, autoDisableChannel: { enabled: true, rules: helpers.serializeApiKeyAutoDisableRules(formData.autoDisableChannel.rules), permanentFailureThreshold: 6, permanentFailureWindowMinutes: 360 }, clientCancellationPolicy: normalize(formData.clientCancellationPolicy) });
});

test('new project overrides stay model-only and preserve inherited timing', () => {
  let state = { clientCancellationPolicy: { enabled: true, threshold: 4, windowMinutes: 17, cooldownMinutes: 47, action: 'cooldown_only', scope: 'project_first', projectOverrides: [] } };
  loadCallback(path, 'handleProjectCancellationOverrideToggle', {
    useCallback: (fn) => fn, cancellationProjectID: 42, setFormData: (update) => { state = update(state); },
  })(true);
  assert.deepEqual(state.clientCancellationPolicy.projectOverrides, [{ projectId: 42, enabled: true, threshold: 4, windowMinutes: 17, cooldownMinutes: 47, action: 'cooldown_model' }]);
});

for (const language of ['en', 'zh-CN']) {
  test(`cancellation copy is model-only, evidence-qualified and distinct from the failure safeguard in ${language}`, () => {
    const locale = JSON.parse(readSource(`locales/${language}/system.json`));
    const description = locale['system.retry.clientCancellation.description'];
    const action = locale['system.retry.clientCancellation.action.description'];
    const cooldown = locale['system.retry.clientCancellation.cooldown.description'];
    assert.match(description, language === 'en' ? /evidence/i : /证据/);
    assert.match(description, language === 'en' ? /(?:not|never).*safeguard/i : /不.*保护/);
    assert.match(action, language === 'en' ? /original request model/i : /原始请求模型/);
    assert.doesNotMatch(action + cooldown, language === 'en' ? /disable.*(?:credential|entire channel)|channel or credential/i : /只禁用|禁用整个渠道|渠道或凭据/);
  });
}

test('permanent freeze window is exposed in the GraphQL request, settings UI, and both locales', () => {
  const dataSource = readSource('features/system/data/system.ts');
  assert.match(dataSource, /permanentFailureWindowMinutes/);
  assert.match(source, /DEFAULT_PERMANENT_FAILURE_WINDOW_MINUTES = 24 \* 60/);
  assert.match(source, /auto-disable-permanent-window/);
  for (const language of ['en', 'zh-CN']) {
    const locale = JSON.parse(readSource(`locales/${language}/system.json`));
    assert.ok(locale['system.retry.autoDisableChannel.permanentWindow.label']);
    assert.ok(locale['system.retry.autoDisableChannel.permanentWindow.description']);
  }
});
