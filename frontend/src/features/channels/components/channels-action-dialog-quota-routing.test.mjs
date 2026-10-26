import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';

// Run the dialog's pure quota-routing-mode mapping helpers and the REAL
// mergeChannelSettingsForUpdate without loading React or its providers.
const dialogPath = new URL('./channels-action-dialog.tsx', import.meta.url);
const source = readFileSync(dialogPath, 'utf8');
const ast = ts.createSourceFile('channels-action-dialog.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);

const helperNames = [
  'recallQuotaRoutingMode',
  'quotaRoutingModeSettingsPatch',
  'reconcileGroupedSupportedModels',
  'getGroupModelCandidates',
  'reconcileFetchedGroupSelection',
  'reconcileUnifiedFetchedGroupModels',
];
const helperNodes = ast.statements.filter((node) => ts.isFunctionDeclaration(node) && helperNames.includes(node.name?.text));
assert.equal(helperNodes.length, 6, 'dialog behavior helpers must stay in channels-action-dialog.tsx');
const helpers = helperNodes.map((node) => `export ${node.getText(ast).replace(/^export\s+/, '')}`).join('\n');

function loadTsModule(sourceText) {
  const { outputText } = ts.transpileModule(sourceText, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 },
  });
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
}

const { recallQuotaRoutingMode, quotaRoutingModeSettingsPatch, reconcileGroupedSupportedModels, getGroupModelCandidates, reconcileFetchedGroupSelection, reconcileUnifiedFetchedGroupModels } =
  await loadTsModule(helpers);

// Type-only imports are erased, so utils/merge.ts loads standalone.
const mergeSource = readFileSync(new URL('../utils/merge.ts', import.meta.url), 'utf8');
const { mergeChannelSettingsForUpdate } = await loadTsModule(mergeSource);

// Wire payload = JSON round-trip: keys with undefined values drop.
const wire = (settings) => JSON.parse(JSON.stringify(settings));

test('recall: absent settings display as INHERIT', () => {
  assert.equal(recallQuotaRoutingMode(undefined), 'INHERIT');
  assert.equal(recallQuotaRoutingMode(null), 'INHERIT');
  assert.equal(recallQuotaRoutingMode({}), 'INHERIT');
});

test('recall: edit and duplicate show the stored mode', () => {
  assert.equal(recallQuotaRoutingMode({ quotaRoutingMode: 'BACKPRESSURE' }), 'BACKPRESSURE');
  assert.equal(recallQuotaRoutingMode({ quotaRoutingMode: 'REMOVE_ON_EXHAUSTED' }), 'REMOVE_ON_EXHAUSTED');
});

test('payload mapping: INHERIT selection sends explicit INHERIT (clears server-side)', () => {
  const patch = quotaRoutingModeSettingsPatch('INHERIT');
  assert.deepEqual(patch, { quotaRoutingMode: 'INHERIT' });
  // Create path (existing=null): pick() passes the explicit value through, so
  // the payload carries INHERIT; the backend maps INHERIT to empty storage.
  const merged = wire(mergeChannelSettingsForUpdate(undefined, patch));
  assert.equal(merged.quotaRoutingMode, 'INHERIT');
});

test('payload mapping: BACKPRESSURE survives into the payload', () => {
  const patch = quotaRoutingModeSettingsPatch('BACKPRESSURE');
  assert.deepEqual(patch, { quotaRoutingMode: 'BACKPRESSURE' });
  const merged = wire(mergeChannelSettingsForUpdate(undefined, patch));
  assert.equal(merged.quotaRoutingMode, 'BACKPRESSURE');
});

test('clobber protection: unrelated edit keeps the stored mode, INHERIT selection clears it', () => {
  const existing = { extraModelPrefix: 'prefix-', quotaRoutingMode: 'REMOVE_ON_EXHAUSTED' };

  // Unrelated fields only: the patch omits quotaRoutingMode, so the merge
  // whitelist falls back to the stored mode.
  const unrelatedPatch = { passThroughUserAgent: true, passThroughBody: false };
  const merged = wire(mergeChannelSettingsForUpdate(existing, unrelatedPatch));
  assert.equal(merged.quotaRoutingMode, 'REMOVE_ON_EXHAUSTED');
  assert.equal(merged.extraModelPrefix, 'prefix-');

  // Same edit while the select sits on INHERIT: the explicit value overrides
  // the whitelist fallback and reaches the payload, so the stored override is
  // cleared server-side (backend maps INHERIT to empty storage).
  const patchWithInheritSelected = { ...unrelatedPatch, ...quotaRoutingModeSettingsPatch('INHERIT') };
  assert.equal(patchWithInheritSelected.quotaRoutingMode, 'INHERIT');
  const mergedAgain = wire(mergeChannelSettingsForUpdate(existing, patchWithInheritSelected));
  assert.equal(mergedAgain.quotaRoutingMode, 'INHERIT');
});

test('mapping helpers stay wired into the dialog payload boundaries', () => {
  const spread = source.split('...quotaRoutingModeSettingsPatch(quotaRoutingMode)').length - 1;
  const recall = source.split('recallQuotaRoutingMode(initialRow?.settings)').length - 1;
  assert.equal(spread, 2, 'payload mapping must feed the edit settingsPatch and the create merge');
  assert.equal(recall, 2, 'recall mapping must feed the state init and the reopen reset');
});

test('inherit option includes the current global mode when available', () => {
  assert.match(source, /quotaRoutingMode\.options\.INHERIT_WITH_MODE/);
  assert.match(source, /quotaRoutingSettings\.defaultMode/);
});


test('group selection replaces stale grouped models while retaining manually added models', () => {
  const result = reconcileGroupedSupportedModels(
    [{ models: ['gpt-6-luna'] }, { models: ['gpt-5.6-luna'] }],
    ['manual-model'],
  );
  assert.deepEqual(result, ['manual-model', 'gpt-6-luna', 'gpt-5.6-luna']);
});

test('saving uses the reconciled key-group model list for channel model options', () => {
  assert.match(source, /supportedModels: resolvedSupportedModels/);
  assert.match(source, /reconcileGroupedSupportedModels\(submittedKeyGroups, manualModels\)/);
});


test('a fresh group model fetch excludes stale removed models but retains explicitly manual models', () => {
  assert.deepEqual(
    getGroupModelCandidates(
      ['old-model-1', 'old-model-2', 'manual-model'],
      ['current-model-1', 'current-model-2'],
      ['manual-model'],
      true,
    ),
    ['current-model-1', 'current-model-2', 'manual-model'],
  );
});

test('a fresh fetch prunes unsupported saved selections but keeps an explicit manual model', () => {
  const candidates = getGroupModelCandidates(
    ['removed-model', 'current-model', 'manual-model'],
    ['current-model'],
    ['manual-model'],
    true,
  );
  assert.deepEqual(reconcileFetchedGroupSelection(['removed-model', 'current-model', 'manual-model'], candidates), [
    'current-model',
    'manual-model',
  ]);
});

test('before fetching, a group picker still shows its saved selection', () => {
  assert.deepEqual(getGroupModelCandidates(['saved-model'], [], [], false), ['saved-model']);
});

test('group and unified-fetch model flows reconcile using fresh candidates rather than retaining stale ids', () => {
  assert.match(source, /getGroupModelCandidates\(own, fetchedForGroup, manualModels, hasFetchedForGroup\)/);
  assert.match(source, /reconcileUnifiedFetchedGroupModels\(/);
  assert.match(source, /if \(hasFetchedForGroup\) \{/);
  assert.match(source, /setGroupModelSelection\(hasFetched \? reconcileFetchedGroupSelection\(own, candidates\)/);
  assert.match(source, /const fetchedCandidates = new Set\(\[\.\.\.collected\.map\(\(entry\) => entry\.model\), \.\.\.manualModels\]\)/);
});


test('unified fetch replaces fetched groups, preserves explicitly manual models, and leaves failed groups untouched', () => {
  assert.deepEqual(
    reconcileUnifiedFetchedGroupModels(
      ['removed-model', 'manual-model'],
      ['current-model', 'manual-model'],
      ['current-model', 'unselected-model'],
      ['manual-model'],
      true,
    ),
    ['manual-model', 'current-model'],
  );
  assert.deepEqual(
    reconcileUnifiedFetchedGroupModels(['manual-model'], [], ['current-model'], ['manual-model'], true),
    [],
    'deselecting a manually configured model must remove it from the group allowlist',
  );
  assert.deepEqual(reconcileUnifiedFetchedGroupModels(['saved-model'], [], [], [], false), ['saved-model']);
});
