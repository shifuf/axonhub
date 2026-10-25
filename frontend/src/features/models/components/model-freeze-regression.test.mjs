import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18next from 'i18next';
import { graphqlDocuments, loadCallback, loadFunctions, loadTS, readSource } from '../../../../tests/unit-loader.mjs';

const i18n = i18next.createInstance();
await i18n.init({ showSupportNotice: false, lng: 'en', keySeparator: false, resources: { en: { translation: {
  ...JSON.parse(readSource('locales/en/channels.json')), ...JSON.parse(readSource('locales/en/models.json')),
} } } });
const translation = { 'react-i18next': { useTranslation: () => ({ t: i18n.t.bind(i18n), i18n }) } };
const render = (Component, props) => renderToStaticMarkup(React.createElement(Component, props));
const path = 'features/models/components/models-association-dialog.tsx';
const channel = { id: 'gid://axonhub/Channel/7', name: 'Frozen channel', type: 'openai', status: 'enabled' };
const permanent = { model: 'request-model', reason: 'Manual operator freeze', createdAt: '2026-10-20T00:00:00Z', expiresAt: null };

test('association freeze/unfreeze address the channel and retain sibling permanent freezes in parallel updates', async () => {
  let connections = [{ channel: { ...channel, modelCooldowns: [permanent] }, models: [] }];
  const calls = [];
  const apply = loadCallback(path, 'applyChannelModelCooldowns', { useCallback: (fn) => fn, connectionsFetchSequenceRef: { current: 0 }, setConnections: (update) => { connections = update(connections); } });
  const bindings = {
    useCallback: (fn) => fn, isDeveloperMode: false, currentRow: { id: 'gid://axonhub/Model/99' },
    isActiveCooldown: loadTS('features/channels/components/cooldown-details.tsx').isActiveCooldown,
    applyChannelModelCooldowns: apply,
    freezeChannelModel: { mutateAsync: async (input) => { calls.push(input); return true; } },
    clearChannelModelCooldown: { mutateAsync: async (input) => { calls.push(input); return true; } },
    t: (key) => key, toast: { success() {} },
  };
  const freeze = loadCallback(path, 'handleFreezeModel', bindings);
  const original = connections[0];
  await Promise.all([freeze(original, 'model-b', 30), freeze(original, 'model-c', 0)]);
  assert.equal(calls[0].channelID, channel.id);
  assert.deepEqual(connections[0].channel.modelCooldowns.map((entry) => entry.model).sort(), ['model-b', 'model-c', 'request-model']);
  assert.equal(connections[0].channel.modelCooldowns.find((entry) => entry.model === 'model-c').expiresAt, null);
  const unfreeze = loadCallback(path, 'handleUnfreezeModel', bindings);
  await unfreeze(connections[0], 'model-b');
  assert.equal(calls.at(-1).channelID, channel.id);
  assert.deepEqual(connections[0].channel.modelCooldowns.map((entry) => entry.model).sort(), ['model-c', 'request-model']);
});

test('a false freeze/unfreeze response never creates optimistic state or success toast', async () => {
  for (const name of ['handleFreezeModel', 'handleUnfreezeModel']) {
    let updates = 0;
    let successes = 0;
    const callback = loadCallback(path, name, {
      useCallback: (fn) => fn, isDeveloperMode: false, currentRow: { id: 'model-guid' },
      applyChannelModelCooldowns: () => updates++, freezeChannelModel: { mutateAsync: async () => false },
      clearChannelModelCooldown: { mutateAsync: async () => false }, t: (key) => key, toast: { success: () => successes++ },
    });
    await callback({ channel, models: [] }, 'request-model', 0);
    assert.equal(updates, 0);
    assert.equal(successes, 0);
  }
});


test('association manual freeze uses the canonical model alias while keeping the channel id', async () => {
  const calls = [];
  const callback = loadCallback(path, 'handleFreezeModel', {
    useCallback: (fn) => fn,
    isDeveloperMode: false,
    currentRow: { id: 'gid://axonhub/Model/99', modelID: 'deepseek-flash' },
    isActiveCooldown: loadTS('features/channels/components/cooldown-details.tsx').isActiveCooldown,
    applyChannelModelCooldowns: () => {},
    freezeChannelModel: { mutateAsync: async (input) => { calls.push(input); return true; } },
    t: (key) => key,
    toast: { success() {} },
  });

  await callback(
    { channel, models: [{ requestModel: 'deepseek-v4.1-flash' }] },
    'deepseek-v4.1-flash',
    30,
  );

  assert.deepEqual(calls, [{ channelID: channel.id, model: 'deepseek-flash', durationMinutes: 30 }]);
});

test('association unfreeze clears the canonical alias and applicable legacy upstream cooldown', async () => {
  const canonical = { model: 'deepseek-flash', reason: 'Canonical manual freeze', createdAt: '2026-10-20T00:00:00Z', expiresAt: null };
  const legacy = { model: 'deepseek-v4.1-flash', reason: 'Legacy manual freeze', createdAt: '2026-10-20T00:00:00Z', expiresAt: null };
  let connections = [{ channel: { ...channel, modelCooldowns: [canonical, legacy] }, models: [{ requestModel: legacy.model }] }];
  const calls = [];
  const callback = loadCallback(path, 'handleUnfreezeModel', {
    useCallback: (fn) => fn,
    isDeveloperMode: false,
    currentRow: { id: 'gid://axonhub/Model/99', modelID: canonical.model },
    applyChannelModelCooldowns: (channelID, update) => {
      connections = connections.map((connection) =>
        connection.channel.id === channelID
          ? { ...connection, channel: { ...connection.channel, modelCooldowns: update(connection.channel.modelCooldowns) } }
          : connection,
      );
    },
    clearChannelModelCooldown: { mutateAsync: async (input) => { calls.push(input); return true; } },
    t: (key) => key,
    toast: { success() {} },
  });

  await callback(connections[0], legacy.model);

  assert.deepEqual(calls, [
    { channelID: channel.id, model: canonical.model },
    { channelID: channel.id, model: legacy.model },
  ]);
  assert.deepEqual(connections[0].channel.modelCooldowns, []);
});

test('model cards map a canonical cooldown to displayed upstream aliases without leaking unrelated freezes', () => {
  const { ChannelModelsCard } = loadTS('features/models/components/channel-models-list.tsx', translation);
  const html = render(ChannelModelsCard, {
    originalRequestModel: 'deepseek-flash',
    connection: {
      channel: {
        ...channel,
        modelCooldowns: [
          { model: 'deepseek-flash', reason: 'Canonical routing failure', createdAt: '2026-10-20T00:00:00Z', expiresAt: null },
          { model: 'other-model', reason: 'Unrelated model freeze', createdAt: '2026-10-20T00:00:00Z', expiresAt: null },
        ],
      },
      models: [{ requestModel: 'deepseek-v4.1-flash' }],
    },
  });

  assert.match(html, /deepseek-v4\.1-flash/);
  assert.match(html, /Canonical routing failure/);
  assert.doesNotMatch(html, /Unrelated model freeze/);
});

test('model cards display permanent and timed reasons without depending on a frozenModels prop', () => {
  const { ChannelModelsCard } = loadTS('features/models/components/channel-models-list.tsx', translation);
  const html = render(ChannelModelsCard, { connection: { channel: { ...channel, modelCooldowns: [permanent] }, models: [{ requestModel: permanent.model }] } });
  assert.match(html, /Manual operator freeze/);
  assert.match(html, /Permanent|manual recovery|manually/i);
  const timed = { ...permanent, expiresAt: new Date(Date.now() + 47 * 60000).toISOString(), reason: 'Rolling error rule' };
  const timedHtml = render(ChannelModelsCard, { connection: { channel: { ...channel, modelCooldowns: [timed] }, models: [{ requestModel: timed.model }] } });
  assert.match(timedHtml, /Rolling error rule/);
  assert.match(timedHtml, /Until|until/);
  assert.match(timedHtml, /47|about 1 hour/);
});

test('expanded model freezes include permanent entries, reasons and expiry without expired rows', () => {
  const { ModelFrozenChannelsSection } = loadTS('features/models/components/model-frozen-channels-section.tsx', {
    ...translation,
    '../data/models': { useFrozenModelChannels: () => ({ data: [{ channel, cooldowns: [permanent, { ...permanent, model: 'expired-model', expiresAt: '2020-01-01T00:00:00Z' }] }], isLoading: false }) },
  });
  const html = render(ModelFrozenChannelsSection, { modelID: permanent.model });
  assert.match(html, /Frozen channel/);
  assert.match(html, /Manual operator freeze/);
  assert.match(html, /Permanent|manual recovery|manually/i);
  assert.doesNotMatch(html, /expired-model/);
});

test('freeze cache invalidation reaches the expanded-model query and request errors reject', async () => {
  for (const name of ['useFreezeChannelModel', 'useClearChannelModelCooldown']) {
    const invalidated = [];
    const bindings = {
      useQueryClient: () => ({ invalidateQueries: ({ queryKey }) => invalidated.push(queryKey) }),
      useErrorHandler: () => ({ handleError() {} }), useTranslation: () => ({ t: (key) => key }), useMutation: (options) => options,
      ...graphqlDocuments('features/channels/data/channels.ts'),
      graphqlRequest: async () => { throw new Error('offline mutation failed'); },
    };
    const { [name]: useHook } = loadFunctions('features/channels/data/channels.ts', [name], bindings);
    const hook = useHook();
    hook.onSuccess();
    assert.ok(invalidated.some((key) => key[0] === 'frozenModelChannels'));
    await assert.rejects(hook.mutationFn({ channelID: channel.id, model: permanent.model }), /offline mutation failed/);
  }
});


test('a frozen sibling request model does not mark an unrelated preview card frozen', () => {
  const { ChannelModelsCard } = loadTS('features/models/components/channel-models-list.tsx', translation);
  const html = render(ChannelModelsCard, { connection: { channel: { ...channel, modelCooldowns: [permanent] }, models: [{ requestModel: 'unrelated-model' }] } });
  assert.doesNotMatch(html, />Frozen</);
  assert.doesNotMatch(html, /Manual operator freeze/);
});

test('closing the association dialog invalidates an in-flight preview before reopening', () => {
  const previewSequence = { current: 7 };
  const events = [];
  const handleClose = loadCallback(path, 'handleClose', {
    useCallback: (fn) => fn,
    setOpen: (value) => events.push(['open', value]),
    setCurrentDeveloper: (value) => events.push(['developer', value]),
    form: { reset: () => events.push(['reset']) },
    setConnections: (value) => events.push(['connections', value]),
    setChannelFilter: (value) => events.push(['filter', value]),
    connectionsFetchSequenceRef: previewSequence,
  });

  handleClose();

  assert.equal(previewSequence.current, 8);
  assert.equal(7 === previewSequence.current, false);
  assert.deepEqual(events.slice(0, 2), [['open', null], ['developer', null]]);
});
