import assert from 'node:assert/strict';
import test from 'node:test';
import { loadTS } from '../../../../tests/unit-loader.mjs';

const path = 'features/models/data/frozen-model-channels.ts';
const cooldown = { model: 'upstream-name', reason: 'Backend association match', createdAt: '2026-10-29T00:00:00Z', expiresAt: null };
const entry = { channel: { id: '7', name: 'Channel X', type: 'openai', status: 'enabled' }, cooldowns: [cooldown] };
function load(request, canRead = true, data, { systemRead = canRead, userID = 'admin-1' } = {}) {
  return loadTS(path, {
    '@/gql/graphql': { graphqlRequest: request },
    '@/hooks/usePermissions': { usePermissions: () => ({ channelPermissions: { canRead }, hasSystemScope: () => systemRead, user: { id: userID } }) },
    '@tanstack/react-query': { useQuery: (options) => ({ ...options, data }) },
  });
}

test('model list fetches exact backend associations with bounded sequential aliases and no project header', async () => {
  const calls = [];
  let inFlight = 0;
  let peak = 0;
  const { fetchFrozenModelChannelsBatch } = load(async (query, variables, headers, init) => {
    calls.push({ query, variables, headers, init });
    peak = Math.max(peak, ++inFlight);
    await new Promise((resolve) => setImmediate(resolve));
    inFlight--;
    assert.ok(Object.keys(variables).length <= 20);
    assert.equal(headers?.['X-Project-ID'], undefined);
    return Object.fromEntries(Object.keys(variables).map((key, index) => [`m${index}`, variables[key] === 'alias-model' ? [entry] : []]));
  });
  const signal = new AbortController().signal;
  const ids = ['alias-model', ...Array.from({ length: 44 }, (_, i) => `model-${i}`), 'alias-model', ''];
  const data = await fetchFrozenModelChannelsBatch(ids, signal);
  assert.equal(calls.length, 3);
  assert.equal(peak, 1);
  assert.deepEqual(data['alias-model'], [entry]);
  assert.deepEqual(data['model-0'], []);
  assert.equal(Object.keys(data).length, 45);
  for (const call of calls) {
    assert.match(call.query, /m0:\s*queryFrozenModelChannels\(modelID:\s*\$model0\)/);
    assert.doesNotMatch(call.query, /alias-model/);
    assert.equal(call.init.signal, signal);
    for (const field of ['reason', 'createdAt', 'expiresAt', 'status']) assert.ok(call.query.includes(field));
  }
});

test('empty or aborted batches do not issue requests and malformed responses are rejected', async () => {
  let calls = 0;
  const { fetchFrozenModelChannelsBatch } = load(async () => { calls++; return { m0: [{ ...entry, cooldowns: [{ model: 'missing-createdAt' }] }] }; });
  assert.deepEqual(await fetchFrozenModelChannelsBatch([]), {});
  assert.equal(calls, 0);
  const abort = new AbortController();
  abort.abort();
  await assert.rejects(fetchFrozenModelChannelsBatch(['alias-model'], abort.signal), { name: 'AbortError' });
  assert.equal(calls, 0);
  await assert.rejects(fetchFrozenModelChannelsBatch(['alias-model']), /createdAt/);
});

test('list query shares freeze invalidation prefix, polls, and hides cached data without read permission', () => {
  for (const canRead of [true, false]) {
    const { useFrozenModelChannelsBatch } = load(() => assert.fail('hooks must not eagerly fetch'), canRead, { 'alias-model': [entry] });
    const query = useFrozenModelChannelsBatch(['b', 'alias-model', 'b']);
    assert.deepEqual(query.queryKey, ['frozenModelChannels', 'list', 'admin-1', ['alias-model', 'b']]);
    assert.equal(query.enabled, canRead);
    assert.equal(query.refetchInterval, 30_000);
    if (!canRead) assert.equal(query.data, undefined);
    assert.equal(useFrozenModelChannelsBatch([]).enabled, false);
  }
});

test('project-only readers cannot fetch or expose admin-wide freeze data', () => {
  const { useFrozenModelChannelsBatch } = load(() => assert.fail('unexpected fetch'), true, { 'alias-model': [entry] }, { systemRead: false });
  const query = useFrozenModelChannelsBatch(['alias-model']);
  assert.equal(query.enabled, false);
  assert.equal(query.data, undefined);
});

test('freeze cache entries are separated when the signed-in admin changes', () => {
  const first = load(() => {}, true, undefined, { userID: 'first-admin' }).useFrozenModelChannelsBatch(['alias-model']);
  const second = load(() => {}, true, undefined, { userID: 'second-admin' }).useFrozenModelChannelsBatch(['alias-model']);
  assert.notDeepEqual(first.queryKey, second.queryKey);
});

test('visible freeze lookup IDs exclude collapsed developer groups, deduplicate and remain stable on sorting', () => {
  const { getVisibleFrozenModelIDs } = load(() => {});
  const groups = new Map([
    ['Hidden', [{ original: { modelID: 'hidden-model' } }]],
    ['Visible', [{ original: { modelID: 'b' } }, { original: { modelID: 'alias-model' } }, { original: { modelID: 'b' } }]],
  ]);
  assert.deepEqual(getVisibleFrozenModelIDs(groups, new Set(['Hidden'])), ['alias-model', 'b']);
  assert.deepEqual(getVisibleFrozenModelIDs(new Map([...groups].reverse()), new Set(['Hidden'])), ['alias-model', 'b']);
  assert.deepEqual(getVisibleFrozenModelIDs(groups, new Set(['Hidden', 'Visible'])), []);
});

test('single-model detail also requires system channel read permission and suppresses cached data', () => {
  for (const systemRead of [true, false]) {
    const { useFrozenModelChannels } = loadTS('features/models/data/models.ts', {
      '@/gql/graphql': { graphqlRequest: () => assert.fail('unexpected fetch') },
      '@/hooks/usePermissions': { usePermissions: () => ({ hasSystemScope: () => systemRead, user: { id: 'admin-1' } }) },
      '@/hooks/use-error-handler': { useErrorHandler: () => ({ handleError() {} }) },
      '@tanstack/react-query': { useQuery: (options) => ({ ...options, data: [entry] }) },
    });
    const query = useFrozenModelChannels('alias-model');
    assert.equal(query.enabled, systemRead);
    assert.deepEqual(query.queryKey, ['frozenModelChannels', 'detail', 'admin-1', 'alias-model']);
    assert.equal(query.refetchIntervalInBackground, false);
    if (!systemRead) assert.equal(query.data, undefined);
    assert.equal(useFrozenModelChannels('alias-model', { enabled: false }).enabled, false);
    assert.equal(useFrozenModelChannels(undefined).enabled, false);
  }
});

test('literal model IDs cannot mutate the result map prototype', async () => {
  const { fetchFrozenModelChannelsBatch } = load(async (_query, variables) =>
    Object.fromEntries(Object.keys(variables).map((_, index) => [`m${index}`, [entry]])),
  );
  const data = await fetchFrozenModelChannelsBatch(['__proto__', 'constructor']);
  assert.equal(Object.hasOwn(data, '__proto__'), true);
  assert.deepEqual(data['__proto__'], [entry]);
  assert.equal(Object.getPrototypeOf(data), Object.prototype);
  assert.deepEqual(data.constructor, [entry]);
});
