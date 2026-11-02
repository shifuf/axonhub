import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import ts from 'typescript';

const source = readFileSync(join(import.meta.dirname, 'compatible-quota-display.ts'), 'utf8');
const transpiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 },
}).outputText;
const moduleUrl = `data:text/javascript;base64,${Buffer.from(transpiled).toString('base64')}`;
const { parseCompatibleQuotaDisplay } = await import(moduleUrl);
const quotaBadges = readFileSync(join(import.meta.dirname, '../../..', 'components/quota-badges.tsx'), 'utf8');
const quotaData = readFileSync(join(import.meta.dirname, 'quotas.ts'), 'utf8');

test('parses a live TestQuota payload with nested raw data and limits', () => {
  const result = parseCompatibleQuotaDisplay({
    provider_type: 'compatible',
    raw_data: {
      source: 'new_api',
      currency: 'USD',
      total_granted_usd: 10,
      total_used_usd: 2.5,
      total_available_usd: 7.5,
    },
    limits: [{ type: 'token', window: 'credits', usageRatio: 0.25 }],
  });

  assert.equal(result.source, 'new_api');
  assert.equal(result.available, 7.5);
  assert.equal(result.total, 10);
  assert.equal(result.used, 2.5);
  assert.equal(result.usedRatio, 0.25);
});

test('parses persisted DeepSeek balances in multiple currencies', () => {
  const result = parseCompatibleQuotaDisplay({
    source: 'deepseek',
    balances: [
      { amount: 88.5, currency: 'CNY', granted_amount: 10, topped_up_amount: 78.5 },
      { amount: 12, currency: 'USD' },
    ],
  });

  assert.equal(result.source, 'deepseek');
  assert.equal(result.currency, 'CNY');
  assert.equal(result.available, 88.5);
  assert.deepEqual(result.balances[0], {
    amount: 88.5,
    currency: 'CNY',
    grantedAmount: 10,
    toppedUpAmount: 78.5,
  });
  assert.equal(result.balances[1].currency, 'USD');
});

test('derives missing usage values from the provider-reported total', () => {
  const result = parseCompatibleQuotaDisplay({
    source: 'openai_billing',
    total_granted_usd: 100,
    balance_usd: 75,
  });

  assert.equal(result.used, 25);
  assert.equal(result.usedRatio, 0.25);
});

test('rejects unrelated provider payloads', () => {
  assert.equal(parseCompatibleQuotaDisplay({ windows: { '5h': { utilization: 0.5 } } }), null);
});

test('channel quota UI renders and measures compatible balances', () => {
  assert.match(quotaBadges, /channel\.providerType === 'compatible'[\s\S]*parseCompatibleQuotaDisplay/);
  assert.match(quotaBadges, /<CompatibleQuotaSummary value=\{channel\.quotaStatus\.quotaData\}/);
  assert.match(quotaBadges, /channel\.providerType !== 'compatible'[\s\S]*PeriodQuotaEstimate/);
  assert.match(quotaBadges, /isOpenaiType\(channel\.type\) && channel\.providerType && channel\.providerType !== 'compatible'/);
  assert.match(quotaData, /providerType: 'compatible'[\s\S]*ProviderCompatibleQuotaData/);
  assert.match(quotaData, /type: 'deepseek' \| 'deepseek_anthropic'/);
});
