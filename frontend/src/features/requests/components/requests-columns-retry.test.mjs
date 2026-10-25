import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18next from 'i18next';
import { z } from 'zod';
import { loadTS, readSource } from '../../../../tests/unit-loader.mjs';

const permissions = { canViewApiKeys: true, canViewChannels: true, canViewCallerUser: true };
const schemaDependencies = {
  '@/features/apikeys/data/schema': { apiKeySchema: z.object({ id: z.string(), name: z.string() }) },
  '@/features/channels/data': { channelSchema: z.object({ id: z.string(), name: z.string() }) },
};
const { requestConnectionSchema } = loadTS('features/requests/data/schema.ts', schemaDependencies);
const translations = new Map();
for (const language of ['en', 'zh-CN']) {
  const i18n = i18next.createInstance();
  await i18n.init({
    showSupportNotice: false,
    lng: language,
    keySeparator: false,
    resources: { [language]: { translation: {
      ...JSON.parse(readSource(`locales/${language}/base.json`)),
      ...JSON.parse(readSource(`locales/${language}/requests.json`)),
    } } },
  });
  translations.set(language, { t: i18n.t.bind(i18n), i18n });
}

function columns(language = 'en', access = permissions) {
  const { useRequestsColumns } = loadTS('features/requests/components/requests-columns.tsx', {
    'react-i18next': { useTranslation: () => translations.get(language) },
    '../../../hooks/useRequestPermissions': { useRequestPermissions: () => access },
    '@/hooks/usePermissions': { usePermissions: () => ({ hasSystemScope: () => false }) },
    '@/hooks/use-pagination-search': { usePaginationSearch: () => ({ navigateWithSearch() {} }) },
    '@/features/system/data/system': {
      useGeneralSettings: () => ({ data: {} }),
      useSecuritySettings: () => ({ data: {} }),
      useUpdateSecuritySettings: () => ({}),
    },
    '@/components/data-table-column-header': { DataTableColumnHeader: () => null },
    // Expose the portal content offline; the actual column callback and badges render normally.
    '@/components/ui/tooltip': {
      Tooltip: ({ children }) => React.createElement(React.Fragment, null, children),
      TooltipTrigger: ({ children }) => children,
      TooltipContent: ({ children, className }) => React.createElement('div', { role: 'tooltip', className }, children),
    },
  });
  return useRequestsColumns();
}
const channelColumns = new Map(['en', 'zh-CN'].map((language) => [language, columns(language).find((column) => column.id === 'channel')]));

const attempt = (id, status, responseStatusCode, errorMessage, channelName = 'Shared channel') => ({
  id: String(id), status, responseStatusCode, errorMessage,
  createdAt: `2026-10-25T04:00:0${id}Z`,
  modelID: 'test-model',
  channel: { id: `channel-${channelName}`, name: channelName },
});
function connection(executions) {
  return {
    edges: [{ cursor: 'request-cursor', node: {
      id: 'request-1', createdAt: '2026-10-25T04:00:00Z', updatedAt: '2026-10-25T04:00:05Z',
      source: 'api', modelID: 'test-model', stream: false, status: 'completed',
      executions: { edges: executions.map((node) => ({ node, cursor: `execution-${node.id}` })) },
    } }],
    pageInfo: { hasNextPage: false, hasPreviousPage: false, startCursor: 'request-cursor', endCursor: 'request-cursor' },
  };
}
function renderRetry(executions, language = 'en') {
  const row = requestConnectionSchema.parse(connection(executions)).edges[0];
  return renderToStaticMarkup(channelColumns.get(language).cell({ row: { original: row.node } }));
}

// Protect the existing shared list/detail schema: unknown keys would otherwise be stripped by Zod.
test('list execution Zod parsing preserves error/status fields and accepts nullable or missing summaries', () => {
  const executions = [attempt(1, 'failed', 429, 'Rate limited'), attempt(2, 'failed', null, null), attempt(3, 'failed')];
  const parsed = requestConnectionSchema.parse(connection(executions)).edges[0].node.executions.edges;
  assert.equal(parsed[0].node.errorMessage, 'Rate limited');
  assert.equal(parsed[0].node.responseStatusCode, 429);
  assert.equal(parsed[1].node.errorMessage, null);
  assert.equal(parsed[1].node.responseStatusCode, null);
  assert.equal(parsed[2].node.errorMessage, undefined);
  assert.equal(parsed[2].node.responseStatusCode, undefined);
});

for (const language of ['en', 'zh-CN']) {
  test(`retry tooltip renders each failed attempt's own HTTP status and persisted error in ${language}`, () => {
    const html = renderRetry([
      attempt(1, 'failed', 429, 'Rate limit exceeded'),
      attempt(3, 'completed', 200, null, 'Final channel'),
      attempt(2, 'failed', 403, 'Model access denied'),
    ], language);
    assert.match(html, /HTTP 429/);
    assert.match(html, /Rate limit exceeded/);
    assert.match(html, /HTTP 403/);
    assert.match(html, /Model access denied/);
    assert.doesNotMatch(html, /HTTP 200/);
    assert.equal((html.match(/role="tooltip"/g) ?? []).length, 1);
    assert.ok(html.indexOf('Model access denied') < html.indexOf('Rate limit exceeded'), 'latest attempt remains first');
    assert.match(html, new RegExp(translations.get(language).t('requests.columns.retryProcess')));
  });

  for (const [label, message, code] of [['missing', undefined, undefined], ['null', null, null], ['empty', '', 503], ['whitespace', ' \n  ', 502]]) {
    test(`failed retry with ${label} error uses a localized fallback in ${language}`, () => {
      const html = renderRetry([attempt(1, 'failed', code, message), attempt(2, 'failed', 429, 'Known failure')], language);
      assert.match(html, new RegExp(translations.get(language).t('common.errors.unknownError')));
      assert.match(html, /Known failure/);
      if (code != null) assert.match(html, new RegExp(`HTTP ${code}`));
      assert.doesNotMatch(html, /HTTP (undefined|null|0)\b/);
    });
  }

  test(`long retry errors wrap in a width/height-bounded scrollable tooltip in ${language}`, () => {
    const message = `First error line\n${'unbroken-provider-error-'.repeat(300)}<script>not executable</script>`;
    const html = renderRetry([
      { ...attempt(1, 'failed', 500, message), responseBody: { secret: 'RAW_BODY_API_SECRET' }, responseHeaders: { authorization: 'RAW_HEADER_API_SECRET' } },
      attempt(2, 'completed', 200, null),
    ], language);
    assert.match(html, /First error line\n/);
    assert.match(html, /&lt;script&gt;not executable&lt;\/script&gt;/);
    assert.doesNotMatch(html, /<script>|RAW_BODY_API_SECRET|RAW_HEADER_API_SECRET/);
    const tooltipClass = html.match(/role="tooltip" class="([^"]+)"/)?.[1] ?? '';
    assert.match(tooltipClass, /\bmax-w-\[/);
    assert.match(tooltipClass, /\bmax-h-\[/);
    assert.match(tooltipClass, /overflow-y-auto/);
    assert.match(html, /whitespace-pre-wrap/);
    assert.match(html, /break-words|break-all|overflow-wrap:anywhere/);
  });

  test(`nonfailed attempts do not receive failure fallbacks or HTTP error badges in ${language}`, () => {
    const html = renderRetry(['completed', 'processing', 'pending', 'canceled'].map((status, index) => attempt(index + 1, status, 200, null)), language);
    assert.doesNotMatch(html, /HTTP 200/);
    assert.ok(!html.includes(translations.get(language).t('common.errors.unknownError')));
  });
}

test('a single execution retains the plain channel display', () => {
  const html = renderRetry([attempt(1, 'failed', 503, 'Single failure')]);
  assert.match(html, /Shared channel/);
  assert.doesNotMatch(html, /role="tooltip"/);
});

test('users without channel permissions still do not receive a channel/retry column', () => {
  assert.equal(columns('en', { ...permissions, canViewChannels: false }).some((column) => column.id === 'channel'), false);
});

// A list endpoint fixture projects the persisted error fields only when the actual
// generated operation selects them, just as GraphQL would. No live server calls.
function requestData(access) {
  const calls = [];
  const data = loadTS('features/requests/data/requests.ts', {
    ...schemaDependencies,
    react: { useCallback: (callback) => callback },
    '@tanstack/react-query': { useQuery: (options) => options },
    'react-i18next': { useTranslation: () => translations.get('en') },
    '../../../hooks/useRequestPermissions': { useRequestPermissions: () => access },
    '@/stores/projectStore': { useSelectedProjectId: () => 'project-7' },
    '@/hooks/use-error-handler': { useErrorHandler: () => ({ handleError() {} }) },
    '@/gql/graphql': { graphqlRequest: async (query, variables, headers) => {
      calls.push({ query, variables, headers });
      const selection = query.match(/executions\([^)]*\)\s*\{\s*edges\s*\{\s*node\s*\{([\s\S]*?)\}/)?.[1];
      assert.ok(selection, 'the operation must request execution summaries');
      assert.doesNotMatch(selection, /\b(requestHeaders|responseHeaders|requestBody|responseBody|responseChunks|channelAPIKeySuffix)\b/);
      const execution = attempt(1, 'failed', 429, 'Persisted retry error');
      for (const field of ['errorMessage', 'responseStatusCode']) {
        if (!new RegExp(`\\b${field}\\b`).test(selection)) delete execution[field];
      }
      if (!access.canViewChannels) delete execution.channel;
      return { requests: connection([execution]) };
    } },
  });
  return { ...data, calls };
}

for (const canViewChannels of [true, false]) {
  for (const flow of ['project list', 'admin list', 'older page', 'newer page', 'export']) {
    test(`${flow} query and Zod contract retain retry errors (channel permission: ${canViewChannels})`, async () => {
      const access = { canViewApiKeys: false, canViewChannels, canViewCallerUser: false };
      const api = requestData(access);
      let requests;
      if (flow.endsWith('list')) {
        const result = await api.useRequests({ first: 20 }, { scopeToSelectedProject: flow === 'project list', projectId: flow === 'project list' ? 'project-7' : null }).queryFn();
        requests = result.edges.map((edge) => edge.node);
      } else if (flow === 'export') {
        requests = await api.useExportAllRequests()();
      } else {
        requests = (await api.fetchAdjacentRequestPage({ cursor: 'request-cursor', direction: flow === 'older page' ? 'older' : 'newer', pageSize: 20, permissions: access, projectId: 'project-7' })).requests;
      }
      const execution = requests[0].executions.edges[0].node;
      assert.equal(execution.errorMessage, 'Persisted retry error');
      assert.equal(execution.responseStatusCode, 429);
      assert.equal(api.calls.length, 1);
      assert.deepEqual(api.calls[0].headers, flow === 'admin list' ? undefined : { 'X-Project-ID': 'project-7' });
      if (!canViewChannels) assert.doesNotMatch(api.calls[0].query, /\bchannel\s*\{/);
    });
  }
}
