import assert from 'node:assert/strict';
import test from 'node:test';
import ts from 'typescript';
import { loadCallback, readSource } from '../../../../tests/unit-loader.mjs';

const path = 'features/models/components/models-association-dialog.tsx';
function previewEffect(bindings) {
  const source = readSource(path);
  const ast = ts.createSourceFile(path, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  let callback;
  function visit(node) {
    if (ts.isCallExpression(node) && node.expression.getText(ast) === 'useEffect' && node.arguments[0]?.getText(ast).includes('const fetchConnections = async')) {
      callback = node.arguments[0].getText(ast);
    }
    ts.forEachChild(node, visit);
  }
  visit(ast);
  assert.ok(callback, 'production preview effect exists');
  const { outputText } = ts.transpileModule(`const effect = ${callback};`, { compilerOptions: { target: ts.ScriptTarget.ES2023 } });
  return new Function(...Object.keys(bindings), `${outputText}; return effect;`)(...Object.values(bindings));
}
const frozen = [{ channel: { id: 'gid://axonhub/Channel/6', modelCooldowns: [{ model: 'upstream-model', reason: 'manual freeze', expiresAt: null }] }, models: [{ requestModel: 'upstream-model' }] }];
const thawed = [{ ...frozen[0], channel: { ...frozen[0].channel, modelCooldowns: [] } }];
const flush = () => new Promise((resolve) => setImmediate(resolve));
function harness(initial = frozen) {
  let connections = structuredClone(initial);
  const pending = [];
  const errors = [];
  const sequence = { current: 0 };
  const bindings = {
    useCallback: (fn) => fn,
    connectionsFetchSequenceRef: sequence,
    setConnections: (update) => { connections = typeof update === 'function' ? update(connections) : update; },
    debouncedAssociations: { rows: [{ type: 'channel_model' }], hasInvalidPriority: false },
    channelOptions: [], inheritedAssociations: [], currentRow: { id: 'model-record', modelID: 'public-model' },
    isOpen: true, isDeveloperMode: false,
    sortAssociationsByPriority: (rows) => rows, formAssociationToInput: (row) => row, modelAssociationToInput: (row) => row,
    queryConnections: () => new Promise((resolve, reject) => { pending.push({ resolve, reject }); }),
    toast: { error: (error) => errors.push(error), success() {} }, t: (key) => key,
    setOpen() {}, setCurrentDeveloper() {}, setChannelFilter() {}, form: { reset() {} },
  };
  return { bindings, pending, errors, sequence, read: () => connections, run: (options = {}) => previewEffect({ ...bindings, ...options })() };
}

for (const action of ['unfreeze', 'freeze']) {
  test(`a preview started before manual ${action} cannot overwrite the successful action`, async () => {
    const state = harness(action === 'unfreeze' ? frozen : thawed);
    state.run();
    const apply = loadCallback(path, 'applyChannelModelCooldowns', state.bindings);
    const callback = loadCallback(path, action === 'unfreeze' ? 'handleUnfreezeModel' : 'handleFreezeModel', {
      ...state.bindings, applyChannelModelCooldowns: apply,
      isActiveCooldown: (entry) => !entry.expiresAt || new Date(entry.expiresAt).getTime() > Date.now(),
      clearChannelModelCooldown: { mutateAsync: async () => true }, freezeChannelModel: { mutateAsync: async () => true },
    });
    await callback(state.read()[0], 'upstream-model', 0);
    state.pending[0].resolve(structuredClone(action === 'unfreeze' ? frozen : thawed));
    await flush();
    assert.equal(state.read()[0].channel.modelCooldowns.length, action === 'unfreeze' ? 0 : 1);
  });
}

test('closing a dialog prevents its late preview from restoring a frozen card', async () => {
  const state = harness();
  state.run();
  loadCallback(path, 'handleClose', state.bindings)();
  assert.deepEqual(state.read(), []);
  state.pending[0].resolve(structuredClone(frozen));
  await flush();
  assert.deepEqual(state.read(), [], 'a closed dialog cannot receive old preview state');
});

test('preview effect cleanup also invalidates a late result when dialog unmounts', async () => {
  const state = harness(thawed);
  const cleanup = state.run();
  assert.equal(typeof cleanup, 'function');
  cleanup();
  state.pending[0].resolve(structuredClone(frozen));
  await flush();
  assert.deepEqual(state.read(), thawed);
});

test('a reopened dialog accepts fresh server state, but not a late response or error from its previous opening', async () => {
  for (const rejectOld of [false, true]) {
    const state = harness();
    state.run();
    loadCallback(path, 'handleClose', state.bindings)();
    state.run();
    state.pending[1].resolve(structuredClone(thawed));
    await flush();
    if (rejectOld) state.pending[0].reject(new Error('old request failed'));
    else state.pending[0].resolve(structuredClone(frozen));
    await flush();
    assert.deepEqual(state.read(), thawed);
    assert.deepEqual(state.errors, []);
  }
});
