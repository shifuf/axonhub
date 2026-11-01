import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import ts from 'typescript';
import { pathToFileURL } from 'node:url';

const require = createRequire(import.meta.url);

const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8');

test('dedicated quota token is submitted by channel forms and has an unsaved test operation', () => {
  const schema = read('./schema.ts');
  const channels = read('./channels.ts');
  const dialog = read('../components/channels-action-dialog.tsx');

  assert.match(schema, /channelCredentialsSchema\s*=\s*z\.object\(\{[\s\S]*?quotaApiKey:\s*z\.string\(\)\.optional\(\)\.nullable\(\)/);
  assert.match(schema, /credentials:\s*z\.object\(\{[\s\S]*?quotaApiKey:\s*z\.string\(\)\.optional\(\)/);
  assert.match(schema, /credentials:\s*z\s*\.object\(\{[\s\S]*?quotaApiKey:\s*z\.string\(\)\.optional\(\)/);
  assert.match(channels, /credentials\s*\{[\s\S]*?quotaApiKey/);
  assert.match(channels, /mutation TestChannelQuota\([\s\S]*?quotaApiKey: String!/);
  assert.match(dialog, /name='credentials\.quotaApiKey'/);
  assert.match(dialog, /testChannelQuota\.mutateAsync\(\{[\s\S]*?form\.getValues\('credentials\.quotaApiKey'\)/);
  assert.match(dialog, /hasQuotaApiKey[\s\S]*?delete updateInput\.credentials/);
});

test('OpenAI-compatible quota-token controls are limited to known quota-provider hosts', () => {
  const dialog = read('../components/channels-action-dialog.tsx');

  assert.match(dialog, /api\\\.\(\?:synthetic\\\.new\|neuralwatt\\\.com\|apertis\\\.ai\)/);
  assert.match(dialog, /\|hyper\\\.charm\\\.land\)\$/);
});

test('quota-token test result schema accepts the existing provider quota payload', async () => {
  const source = read('./schema.ts');
  const start = source.indexOf('export const testChannelQuotaPayloadSchema');
  const end = source.indexOf('export type TestChannelQuotaPayload', start);
  assert.notEqual(start, -1);
  assert.notEqual(end, -1);

  const moduleSource = `import { z } from 'zod';\n${source.slice(start, end)}\nexport { testChannelQuotaPayloadSchema };`;
  const { outputText } = ts.transpileModule(moduleSource, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 },
  });
  const tmpDir = await mkdtemp(join(tmpdir(), 'axonhub-quota-schema-'));
  const modulePath = join(tmpDir, 'schema.mjs');
  await writeFile(modulePath, outputText.replace("from 'zod'", `from ${JSON.stringify(pathToFileURL(require.resolve('zod')).href)}`));
  const schemaModule = await import(pathToFileURL(modulePath).href);
  const testChannelQuotaPayloadSchema = schemaModule.testChannelQuotaPayloadSchema;

  const payload = {
    providerType: 'nanogpt',
    status: 'available',
    ready: true,
    quotaData: { limits: [{ type: 'tokens', usageRatio: 0.25 }] },
  };
  assert.deepEqual(testChannelQuotaPayloadSchema.parse(payload), payload);
});
