// Offline TypeScript/TSX loader for Node's existing unit-test runner.
import { createRequire } from 'node:module';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import ts from 'typescript';

const root = resolve(import.meta.dirname, '../src');
const require = createRequire(import.meta.url);
export const readSource = (path) => readFileSync(resolve(root, path), 'utf8');

export function loadTS(path, stubs = {}) {
  const cache = new Map();
  function load(file) {
    if (cache.has(file)) return cache.get(file).exports;
    const module = { exports: {} };
    cache.set(file, module);
    const { outputText } = ts.transpileModule(readFileSync(file, 'utf8'), {
      fileName: file,
      compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023, jsx: ts.JsxEmit.ReactJSX, esModuleInterop: true },
    });
    const localRequire = (id) => {
      if (Object.hasOwn(stubs, id)) return stubs[id];
      if (!id.startsWith('.') && !id.startsWith('@/')) return require(id);
      const base = id.startsWith('@/') ? resolve(root, id.slice(2)) : resolve(dirname(file), id);
      const target = [base, `${base}.ts`, `${base}.tsx`, `${base}/index.ts`, `${base}/index.tsx`].find((name) => existsSync(name));
      if (!target) throw new Error(`Cannot load ${id} from ${file}`);
      return load(target);
    };
    new Function('require', 'module', 'exports', outputText)(localRequire, module, module.exports);
    return module.exports;
  }
  return load(resolve(root, path));
}

// Execute the production callback, replacing only its external effects.
export function loadCallback(path, name, bindings) {
  const source = readSource(path);
  const ast = ts.createSourceFile(path, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  let initializer;
  function visit(node) {
    if (ts.isVariableDeclaration(node) && node.name.getText(ast) === name) initializer = node.initializer.getText(ast);
    ts.forEachChild(node, visit);
  }
  visit(ast);
  if (!initializer) throw new Error(`${name} not found`);
  const { outputText } = ts.transpileModule(`const callback = ${initializer};`, { compilerOptions: { target: ts.ScriptTarget.ES2023 } });
  return new Function(...Object.keys(bindings), `${outputText}; return callback;`)(...Object.values(bindings));
}

export function graphqlDocuments(path) {
  const ast = ts.createSourceFile(path, readSource(path), ts.ScriptTarget.Latest, true);
  const constants = {};
  for (const statement of ast.statements) {
    if (!ts.isVariableStatement(statement)) continue;
    for (const declaration of statement.declarationList.declarations) {
      const init = declaration.initializer;
      if (!init || !(ts.isNoSubstitutionTemplateLiteral(init) || ts.isTemplateExpression(init))) continue;
      try { constants[declaration.name.getText(ast)] = new Function(...Object.keys(constants), `return ${init.getText(ast)};`)(...Object.values(constants)); } catch { /* unrelated templates may need runtime bindings */ }
    }
  }
  return constants;
}

export function loadFunctions(path, names, bindings = {}) {
  const ast = ts.createSourceFile(path, readSource(path), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const nodes = ast.statements.filter((node) => ts.isFunctionDeclaration(node) && names.includes(node.name?.text));
  if (nodes.length !== names.length) throw new Error(`Missing functions: ${names.join(', ')}`);
  const source = nodes.map((node) => node.getText(ast).replace(/^export\s+/, '')).join('\n');
  const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2023, jsx: ts.JsxEmit.ReactJSX } });
  return new Function(...Object.keys(bindings), `${outputText}; return { ${names.join(', ')} };`)(...Object.values(bindings));
}
