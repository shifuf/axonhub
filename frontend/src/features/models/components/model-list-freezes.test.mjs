import assert from 'node:assert/strict';
import test from 'node:test';
import { createRequire } from 'node:module';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18next from 'i18next';
import ts from 'typescript';
import { loadTS, readSource } from '../../../../tests/unit-loader.mjs';

const require = createRequire(import.meta.url);
// The shared callback loader handles plain TS; table cell callbacks contain JSX.
function loadCellCallback(path, name, bindings) {
  const ast = ts.createSourceFile(path, readSource(path), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  let initializer;
  function visit(node) {
    if (ts.isVariableDeclaration(node) && node.name.getText(ast) === name) initializer = node.initializer.getText(ast);
    ts.forEachChild(node, visit);
  }
  visit(ast);
  assert.ok(initializer, `${name} exists`);
  const { outputText } = ts.transpileModule(`const callback = ${initializer};`, {
    fileName: 'cell.tsx', compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023, jsx: ts.JsxEmit.ReactJSX },
  });
  return new Function(...Object.keys(bindings), `const exports = {}; ${outputText}; return callback;`)(...Object.values(bindings));
}
const now = Date.parse('2026-10-29T04:00:00Z');
const cooldowns = [
  { model: 'alias-model', reason: 'Five cancellations after upstream headers', createdAt: '2026-10-29T03:00:00Z', expiresAt: '2026-10-29T04:47:00Z' },
  { model: 'permanent-model', reason: 'Manual operator freeze', createdAt: '2026-10-29T03:00:00Z', expiresAt: null },
  { model: 'expired-model', reason: 'Old freeze', createdAt: '2026-10-29T03:00:00Z', expiresAt: '2026-10-29T04:00:00Z' },
];
const render = (component, props) => renderToStaticMarkup(React.createElement(component, props));
const Null = () => null;
const Wrapper = ({ children }) => React.createElement('div', null, children);
const tooltip = { Tooltip: Wrapper, TooltipTrigger: Wrapper, TooltipContent: ({ children }) => React.createElement('aside', { 'data-details': true }, children) };

for (const language of ['en', 'zh-CN']) {
  const i18n = i18next.createInstance();
  await i18n.init({ showSupportNotice: false, lng: language, keySeparator: false, resources: { [language]: { translation: {
    ...JSON.parse(readSource(`locales/${language}/channels.json`)), ...JSON.parse(readSource(`locales/${language}/models.json`)),
  } } } });
  const translation = { 'react-i18next': { useTranslation: () => ({ t: i18n.t.bind(i18n), i18n }) } };

  test(`direct freeze badges show model/channel names, reason and expiry in ${language}`, () => {
    const { ModelFreezeBadges } = loadTS('features/channels/components/model-freeze-badges.tsx', { ...translation, '@/components/ui/tooltip': tooltip });
    const channel = render(ModelFreezeBadges, { cooldowns, now });
    assert.match(channel, language === 'en' ? /alias-model model frozen/ : /alias-model 模型已冷冻/);
    assert.match(channel, /permanent-model/);
    assert.match(channel, /Five cancellations after upstream headers/);
    assert.match(channel, /47/);
    assert.match(channel, /datetime="2026-10-29T04:47:00Z"/i);
    assert.match(channel, language === 'en' ? /Permanent.*manual/ : /永久.*手动/);
    assert.doesNotMatch(channel, /expired-model|Old freeze/);
    const model = render(ModelFreezeBadges, { cooldowns, channelName: 'Channel X', now });
    assert.match(model, language === 'en' ? /model frozen on channel Channel X/ : /模型在渠道 Channel X 已冷冻/);
    assert.equal(render(ModelFreezeBadges, { cooldowns: [cooldowns[0]], now: now + 48 * 60_000 }), '');
  });

  test(`cancellation policy explains upstream headers or data, not a first-token requirement in ${language}`, () => {
    const copy = JSON.parse(readSource(`locales/${language}/system.json`))['system.retry.clientCancellation.description'];
    assert.match(copy, language === 'en' ? /response headers or data/i : /响应头或数据/);
    assert.match(copy, language === 'en' ? /without.*response.*not counted/i : /未收到上游响应.*不计/);
    assert.match(copy, language === 'en' ? /first token.*not required/i : /不要求.*首.*[Tt]oken/);
    assert.match(copy, language === 'en' ? /local.*heartbeat.*not.*evidence/i : /本地.*心跳.*不.*证据/);
  });
}

// Exercise the actual table cells (not only the shared badge component).
test('channel NameCell exposes cooldowns without expansion for readers and hides them without read permission', () => {
  const { ModelFreezeBadges } = loadTS('features/channels/components/model-freeze-badges.tsx', {
    'react-i18next': { useTranslation: () => ({ t: (key, values) => key === 'channels.cooldown.modelFrozen' ? `${values.model} model frozen` : key, i18n: { language: 'en' } }) },
    '@/components/ui/tooltip': tooltip,
  });
  for (const canRead of [true, false]) {
    const NameCell = loadCellCallback('features/channels/components/channels-columns.tsx', 'NameCell', {
      require, memo: (fn) => fn, useTranslation: () => ({ t: (key) => key }),
      usePermissions: () => ({ channelPermissions: { canRead: true, canWrite: false }, hasSystemScope: () => canRead }),
      useCooldownNow: () => now, getCooldownRemainingMinutes: () => 0,
      CLIENT_CANCELLATION_COOLDOWN_REASON_PREFIX: 'client cancellation',
      getChannelWebsiteURL: () => null, getChannelQuotaRoutingIndicator: () => null,
      cn: (...args) => args.filter(Boolean).join(' '), ModelFreezeBadges,
      ...tooltip,
    });
    const html = render(NameCell, { row: { original: { status: 'enabled', modelCooldowns: cooldowns }, getValue: () => 'Channel X' } });
    if (canRead) {
      assert.match(html, /alias-model model frozen/);
      assert.match(html, /Five cancellations after upstream headers/);
    } else assert.doesNotMatch(html, /alias-model/);
    assert.doesNotMatch(html, /expired-model|Old freeze/);
  }
});

test('model name column displays backend-resolved channel freezes without expansion and expires them', () => {
  const { ModelFreezeBadges } = loadTS('features/channels/components/model-freeze-badges.tsx', {
    'react-i18next': { useTranslation: () => ({ t: (key, values) => key === 'models.cooldown.frozenOnChannel' ? `model frozen on channel ${values.channel}` : key, i18n: { language: 'en' } }) },
    '@/components/ui/tooltip': tooltip,
  });
  const createColumns = loadCellCallback('features/models/components/models-columns.tsx', 'createColumns', {
    require, Button: Null, Checkbox: Null, DeveloperCell: Null, StatusSwitchCell: Null,
    AssociationRulesCell: Null, DataTableRowActions: Null,
    ModelFreezeBadges,
  });
  const unloadedColumns = createColumns((key) => key, false);
  for (const modelID of ['__proto__', 'constructor']) {
    assert.doesNotThrow(() => render(unloadedColumns.find((column) => column.accessorKey === 'name').cell, { row: { original: { name: 'Literal ID', modelID } } }));
  }
  const frozen = { 'alias-model': [{ channel: { id: '7', name: 'Channel X' }, cooldowns }] };
  const columns = createColumns((key) => key, false, frozen, now);
  const html = render(columns.find((column) => column.accessorKey === 'name').cell, {
    row: { original: { name: 'Friendly alias', modelID: 'alias-model' } },
  });
  assert.match(html, /Friendly alias/);
  assert.match(html, /model frozen on channel Channel X/);
  assert.match(html, /Five cancellations after upstream headers/);
  assert.doesNotMatch(html, /expired-model|Old freeze/);
  const expiredColumns = createColumns((key) => key, false, { 'alias-model': [{ channel: { id: '7', name: 'Channel X' }, cooldowns: [cooldowns[0]] }] }, now + 48 * 60_000);
  assert.doesNotMatch(render(expiredColumns.find((column) => column.accessorKey === 'name').cell, { row: { original: { name: 'Friendly alias', modelID: 'alias-model' } } }), /Channel X/);
  assert.doesNotMatch(render(columns.find((column) => column.accessorKey === 'name').cell, {
    row: { original: { name: 'Other model', modelID: 'not-associated' } },
  }), /Channel X/);
});
