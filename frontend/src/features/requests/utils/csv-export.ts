import type { Request } from '../data/schema';

export type RequestsExportScope = 'page' | 'all';

interface RequestsCsvLabels {
  id: string;
  status: string;
  source: string;
  model: string;
  apiFormat: string;
  channel: string;
  key: string;
  clientIP: string;
  userAgent: string;
  tokens: string;
  totalTokens: string;
  input: string;
  output: string;
  reasoning: string;
  readCache: string;
  writeCache: string;
  cost: string;
  duration: string;
  firstToken: string;
  total: string;
  tokensPerSecond: string;
  caller: string;
  createdAt: string;
}

interface RequestsCsvColumn {
  // Table column id this CSV column belongs to; '' marks always-on columns.
  tableColumnId: string;
  header: string;
  value: (request: Request) => string | number | null | undefined;
}

function usage(request: Request) {
  return request.usageLogs?.edges?.[0]?.node;
}

function callerName(request: Request): string {
  const first = request.apiKey?.user?.firstName ?? '';
  const last = request.apiKey?.user?.lastName ?? '';
  return `${first}${last}`.trim();
}

/**
 * CSV columns mirror the requests table. `tableColumnId` ties each CSV column
 * to a table column so hidden columns can be skipped; token/duration cells fan
 * out into several plain CSV fields for spreadsheet friendliness.
 */
export function buildRequestsCsvColumns(labels: RequestsCsvLabels): RequestsCsvColumn[] {
  return [
    { tableColumnId: '', header: labels.id, value: (r) => r.id },
    { tableColumnId: 'status', header: labels.status, value: (r) => r.status },
    { tableColumnId: 'source', header: labels.source, value: (r) => r.source },
    { tableColumnId: 'modelID', header: labels.model, value: (r) => r.modelID },
    { tableColumnId: 'apiFormat', header: labels.apiFormat, value: (r) => r.format ?? '' },
    { tableColumnId: 'channel', header: labels.channel, value: (r) => r.channel?.name ?? '' },
    { tableColumnId: 'channelAPIKeyIndex', header: labels.key, value: (r) => r.executions?.edges?.[0]?.node?.channelAPIKeyIndex ?? '' },
    { tableColumnId: 'tokens', header: `${labels.tokens}/${labels.totalTokens}`, value: (r) => {
      const u = usage(r);
      return u ? (u.promptTokens || 0) + (u.completionTokens || 0) : '';
    } },
    { tableColumnId: 'tokens', header: `${labels.tokens}/${labels.input}`, value: (r) => usage(r)?.promptTokens ?? '' },
    { tableColumnId: 'tokens', header: `${labels.tokens}/${labels.output}`, value: (r) => usage(r)?.completionTokens ?? '' },
    { tableColumnId: 'tokens', header: `${labels.tokens}/${labels.reasoning}`, value: (r) => usage(r)?.completionReasoningTokens ?? '' },
    { tableColumnId: 'readCache', header: labels.readCache, value: (r) => usage(r)?.promptCachedTokens ?? '' },
    { tableColumnId: 'writeCache', header: labels.writeCache, value: (r) => usage(r)?.promptWriteCachedTokens ?? '' },
    { tableColumnId: 'cost', header: labels.cost, value: (r) => usage(r)?.totalCost ?? '' },
    { tableColumnId: 'duration', header: `${labels.duration}/${labels.firstToken}`, value: (r) => r.metricsFirstTokenLatencyMs ?? '' },
    { tableColumnId: 'duration', header: `${labels.duration}/${labels.total}`, value: (r) => r.metricsLatencyMs ?? '' },
    { tableColumnId: 'tokensPerSecond', header: labels.tokensPerSecond, value: (r) => {
      const u = usage(r);
      const latency = r.metricsLatencyMs;
      if (!u || !latency) return '';
      const completion = u.completionTokens || 0;
      return completion > 0 ? Number(((completion / latency) * 1000).toFixed(2)) : '';
    } },
    { tableColumnId: 'caller', header: labels.caller, value: (r) => r.source === 'api' ? `${r.apiKey?.name ?? ''}${callerName(r) ? ` (${callerName(r)})` : ''}` : r.source },
    { tableColumnId: 'clientIP', header: labels.clientIP, value: (r) => r.clientIP ?? '' },
    { tableColumnId: 'userAgent', header: labels.userAgent, value: (r) => r.userAgent ?? '' },
    { tableColumnId: '', header: labels.createdAt, value: (r) => formatDate(r.createdAt) },
  ];
}

function formatDate(value: Date | string): string {
  const date = typeof value === 'string' ? new Date(value) : value;
  const pad = (n: number) => n.toString().padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

function escapeCsvValue(value: unknown): string {
  if (value === null || value === undefined) return '';
  const text = String(value);
  if (/[",\r\n]/.test(text)) {
    return `"${text.replace(/"/g, '""')}"`;
  }
  return text;
}

export function buildRequestsCsv(
  requests: Request[],
  options: {
    labels: RequestsCsvLabels;
    /** Visible table column ids; hidden table columns are skipped in the CSV. */
    visibleColumnIds?: Set<string>;
  }
): string {
  const visible = options.visibleColumnIds;
  const columns = buildRequestsCsvColumns(options.labels).filter(
    (column) => !column.tableColumnId || !visible || visible.has(column.tableColumnId)
  );

  const lines = [columns.map((c) => escapeCsvValue(c.header)).join(',')];
  for (const request of requests) {
    lines.push(columns.map((c) => escapeCsvValue(c.value(request))).join(','));
  }
  // BOM keeps Excel happy with UTF-8 Chinese text.
  return `\uFEFF${lines.join('\r\n')}`;
}

export function downloadCsv(filename: string, csv: string) {
  const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}
