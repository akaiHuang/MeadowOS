// Trusted bundled parser. Source bytes are data; never execute or import them.
const ts = require(process.argv[1]);
const fs = require('node:fs');
const request = JSON.parse(fs.readFileSync(0, 'utf8'));
const window = request.window;
const symbols = [], diagnostics = [];
let matched = 0, truncated = false, nextOffset;
let hadErrors = false;
const bounded = (text, maxBytes) => {
  text = String(text);
  if (Buffer.byteLength(text) <= maxBytes) return text;
  let out = '', bytes = 0;
  for (const char of text) {
    const width = Buffer.byteLength(char);
    if (bytes + width > maxBytes - 3) break;
    out += char; bytes += width;
  }
  return out + '…';
};
for (const unit of request.units) {
  const source = ts.createSourceFile('/source' + unit.extension, unit.text,
    ts.ScriptTarget.Latest, true);
  const position = offset => {
    const p = source.getLineAndCharacterOfPosition(offset);
    return {line: unit.line + p.line,
      column: p.character + 1 + (p.line === 0 ? unit.column - 1 : 0)};
  };
  for (const error of source.parseDiagnostics) {
    hadErrors = true;
    if (diagnostics.length >= 20) { truncated = true; break; }
    const p = position(error.start || 0);
    diagnostics.push({code: 'TS' + error.code, ...p,
      message: bounded(ts.flattenDiagnosticMessageText(error.messageText, '\n'), 300)});
  }
  const nameOf = node => node.name ? bounded(node.name.getText(source), 100) : '';
  const childScope = (parent, name) => bounded(parent ? parent + '.' + name : name, 160);
  const emit = (name, kind, parent, node) => {
    if (!name) return;
    const start = position(node.getStart(source)), end = position(node.end);
    if (start.line > window.end_line || end.line < window.start_line) return;
    matched++;
    if (matched <= window.offset) return;
    if (symbols.length >= window.limit) {
      truncated = true; nextOffset = window.offset + symbols.length; return;
    }
    symbols.push({name: bounded(name, 100), kind, ...(parent ? {parent: bounded(parent, 160)} : {}),
      line: start.line, column: start.column, end_line: end.line, end_column: end.column});
  };
  const bindingNames = (node, names = []) => {
    if (ts.isIdentifier(node)) names.push(node.text);
    else if (ts.isObjectBindingPattern(node) || ts.isArrayBindingPattern(node))
      for (const element of node.elements) if (ts.isBindingElement(element)) bindingNames(element.name, names);
    return names;
  };
  const walk = (node, parent = '', functionDepth = 0) => {
    if (ts.isVariableDeclaration(node)) {
      const names = bindingNames(node.name);
      const initializer = node.initializer;
      const callable = initializer && (ts.isArrowFunction(initializer) || ts.isFunctionExpression(initializer));
      const classValue = initializer && ts.isClassExpression(initializer);
      if (functionDepth === 0 || callable || classValue)
        for (const name of names) emit(name, callable ? 'function' : classValue ? 'class' : 'variable', parent, node);
      if (initializer) walk(initializer, names.length === 1 ? childScope(parent, names[0]) : parent, functionDepth);
      return;
    }
    if (ts.isFunctionDeclaration(node)) {
      const name = nameOf(node);
      emit(name, 'function', parent, node);
      const scope = name ? childScope(parent, name) : parent;
      ts.forEachChild(node, child => walk(child, scope, functionDepth + 1));
      return;
    }
    if (ts.isFunctionExpression(node) || ts.isArrowFunction(node)) {
      // Named function expressions have their own lexical self name.
      const name = nameOf(node);
      if (name) emit(name, 'function_expression', parent, node);
      const scope = name ? childScope(parent, name) : parent;
      ts.forEachChild(node, child => walk(child, scope, functionDepth + 1));
      return;
    }
    if (ts.isClassDeclaration(node) || ts.isClassExpression(node) || ts.isInterfaceDeclaration(node) ||
        ts.isEnumDeclaration(node) || ts.isModuleDeclaration(node)) {
      const name = nameOf(node);
      const kind = ts.isInterfaceDeclaration(node) ? 'interface' : ts.isEnumDeclaration(node) ? 'enum' :
        ts.isModuleDeclaration(node) ? 'namespace' : 'class';
      if (!ts.isClassExpression(node) || name) emit(name, kind, parent, node);
      const scope = name ? childScope(parent, name) : parent;
      ts.forEachChild(node, child => walk(child, scope, functionDepth));
      return;
    }
    if (ts.isTypeAliasDeclaration(node)) {
      emit(nameOf(node), 'type_alias', parent, node); return;
    }
    if (ts.isEnumMember(node)) {
      emit(nameOf(node), 'enum_member', parent, node); return;
    }
    if (ts.isMethodDeclaration(node) || ts.isMethodSignature(node) || ts.isGetAccessorDeclaration(node) ||
        ts.isSetAccessorDeclaration(node) || ts.isConstructorDeclaration(node)) {
      const name = ts.isConstructorDeclaration(node) ? 'constructor' : nameOf(node);
      const kind = ts.isMethodSignature(node) ? 'method_signature' : ts.isGetAccessorDeclaration(node) ? 'getter' :
        ts.isSetAccessorDeclaration(node) ? 'setter' : ts.isConstructorDeclaration(node) ? 'constructor' : 'method';
      emit(name, kind, parent, node);
      ts.forEachChild(node, child => walk(child, childScope(parent, name), functionDepth + 1));
      return;
    }
    if (ts.isPropertyDeclaration(node) || ts.isPropertySignature(node)) {
      const name = nameOf(node);
      emit(name, 'field', parent, node);
      if (node.initializer) walk(node.initializer, childScope(parent, name), functionDepth);
      return;
    }
    if (ts.isPropertyAssignment(node) && node.initializer &&
        (ts.isArrowFunction(node.initializer) || ts.isFunctionExpression(node.initializer))) {
      const name = nameOf(node);
      emit(name, 'method', parent, node);
      walk(node.initializer, childScope(parent, name), functionDepth); return;
    }
    if (ts.isPropertyAssignment(node) && ts.isObjectLiteralExpression(node.initializer)) {
      walk(node.initializer, childScope(parent, nameOf(node)), functionDepth); return;
    }
    ts.forEachChild(node, child => walk(child, parent, functionDepth));
  };
  walk(source);
}
process.stdout.write(JSON.stringify({status: hadErrors ? 'parsed_with_errors' : 'parsed', symbols,
  diagnostics, truncated, ...(nextOffset !== undefined ? {next_offset: nextOffset} : {})}));
