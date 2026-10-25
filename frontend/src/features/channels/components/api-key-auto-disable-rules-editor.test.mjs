import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18next from 'i18next';
import { loadTS, readSource } from '../../../../tests/unit-loader.mjs';

const i18n = i18next.createInstance();
await i18n.init({ showSupportNotice: false, lng: 'en', keySeparator: false, resources: { en: { translation: JSON.parse(readSource('locales/en/channels.json')) } } });
const { ApiKeyAutoDisableRulesEditor, ApiKeyAutoDisableRulesPreview } = loadTS('features/channels/components/api-key-auto-disable-rules-editor.tsx', {
  'react-i18next': { useTranslation: () => ({ t: i18n.t.bind(i18n), i18n }) },
});
const render = (Component, props) => renderToStaticMarkup(React.createElement(Component, props));

const helpers = loadTS('features/channels/data/auto-disable.ts');

test('editor exposes a 1–10080 window, uncapped threshold and fixed model cooldown without credential/action toggles', () => {
  const html = render(ApiKeyAutoDisableRulesEditor, { rules: helpers.toApiKeyAutoDisableRuleFormValues([{ statusCodes: [429], times: 9, action: 'temporary_disable', disableDurationMinutes: 47, windowMinutes: 0, freezeModel: false }]), onChange() {} });
  assert.match(html, /min="1"/);
  assert.match(html, /max="10080"/);
  assert.match(html, /step="1"/);
  assert.match(html, /data-testid="auto-disable-window-0"/);
  assert.match(html, /data-testid="auto-disable-model-target"/);
  assert.doesNotMatch(html, /role="checkbox"|auto-disable-freeze-model/);
  assert.match(html, /value="10"/);
  assert.match(html, /value="47"/);
  assert.match(html, /value="9"/);
  const threshold = html.match(/<input[^>]*data-testid="auto-disable-threshold-0"[^>]*>/)?.[0];
  assert.ok(threshold, 'threshold input has a stable label');
  assert.doesNotMatch(threshold, /max=/, 'thresholds above 5 must never be clamped');
  const source = readSource('features/channels/components/api-key-auto-disable-rules-editor.tsx');
  assert.doesNotMatch(source, /SelectItem value='(?:permanent_disable|disable_until_cron|permanent_disable_delete)'/);
  assert.doesNotMatch(source, /name=\{`rules\.\$\{index\}\.(?:action|freezeModel)`\}/);
  assert.doesNotMatch(source, /allowDelete/);
  assert.match(html, /permanently.*freez/i);
  assert.doesNotMatch(html, /whole channel/i);
});

test('legacy rule previews always show a positive rolling window, original-request-model target and temporary recovery', () => {
  const html = render(ApiKeyAutoDisableRulesPreview, { rules: [
    { statusCodes: [429], times: 3, action: 'temporary_disable', disableDurationMinutes: 47, windowMinutes: 12, freezeModel: true },
    { statusCodes: [503], times: 2, action: 'disable_until_cron', disableUntilCron: '17 4 * * 2', disableUntilTimezone: 'Pacific/Chatham', windowMinutes: 0 },
    { statusCodes: [401], times: 1, action: 'permanent_disable' },
    { statusCodes: [403], times: 1, action: 'permanent_disable_delete', disableDurationMinutes: 11 },
  ] });
  assert.match(html, /12/);
  assert.match(html, /47/);
  assert.match(html, /10 minutes/);
  assert.match(html, /30 minutes/);
  assert.match(html, /11 minutes/);
  assert.match(html, /channel.*original request model/i);
  assert.doesNotMatch(html, /consecutive|17 4 \* \* 2|Pacific\/Chatham|failed credential|Permanent.*manual/i);
  assert.match(html, /concrete.*errors/i);
});

test('invalid nonlegacy window values stay visible and are not clamped on reopen', () => {
  const html = render(ApiKeyAutoDisableRulesEditor, { rules: helpers.toApiKeyAutoDisableRuleFormValues([{ statusCodes: [429], times: 3, action: 'permanent_disable_delete', windowMinutes: 10081, freezeModel: false }]), onChange() {} });
  assert.match(html, /whole number from 1 to 10080/);
  assert.doesNotMatch(html, /cannot delete credentials|turn off model-only/);
  assert.match(html, /value="10081"/);
});

test('new rules start with explicit model-only temporary 30-minute cooldown and 10-minute window', () => {
  const source = readSource('features/channels/components/api-key-auto-disable-rules-editor.tsx');
  assert.match(source, /appendRule\(\{[^}]*action: 'temporary_disable'[^}]*disableDurationMinutes: 30[^}]*windowMinutes: 10[^}]*freezeModel: true/s);
});

for (const language of ['en', 'zh-CN']) {
  test(`automatic rule copy explains non-consuming rolling failures and the permanent model freeze in ${language}`, () => {
    const channels = JSON.parse(readSource(`locales/${language}/channels.json`));
    const system = JSON.parse(readSource(`locales/${language}/system.json`));
    for (const key of ['channels.dialogs.apiKeyRules.policySummary', 'channels.dialogs.apiKeyRules.safetyHint', 'channels.dialogs.apiKeyRules.retentionHint']) assert.ok(channels[key], key);
    const copy = [channels['channels.dialogs.availability.scope'], channels['channels.dialogs.apiKeyRules.safetyHint'], channels['channels.dialogs.apiKeyRules.retentionHint'], system['system.retry.autoDisableChannel.description'], system['system.retry.autoDisableChannel.noDeleteCredential']].join(' ');
    assert.match(copy, language === 'en' ? /permanent-failure threshold|permanently freezes/i : /永久冻结/);
    assert.doesNotMatch(copy, language === 'en' ? /whole channel/i : /整个渠道/);
    assert.match(copy, language === 'en' ? /cooldown.*(?:do not|never).*clear/i : /冷却.*不.*清[零除]/);
    assert.match(copy, language === 'en' ? /never.*(?:disable|delete).*API keys/i : /不会.*(?:禁用|删除).*(?:API Key|密钥)/);
    assert.doesNotMatch(system['system.onboarding.steps.autoDisableToggle.description'], /401[^.。]*(?:permanent|永久)/i);
  });
}
