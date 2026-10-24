// Trusted embedded runner. Source is data: never evaluate it or load its project.
const ts = require(process.argv[1]);
const fs = require('node:fs');
const request = JSON.parse(fs.readFileSync(0, 'utf8'));
const diagnostics = [];
let truncated = false;
let suppressed = false;
for (const unit of request.units) {
  const name = '/source' + unit.extension;
  const options = {allowJs: true, checkJs: true, noLib: true, noResolve: true,
    noEmit: true, target: ts.ScriptTarget.Latest, jsx: ts.JsxEmit.Preserve};
  const source = ts.createSourceFile(name, unit.text, options.target, true);
  // Use the pinned parser's directives, not a standalone scanner: regex/template
  // rescanning needs parser context. These fields are regression-tested below.
  if (source.checkJsDirective?.enabled === false || source.commentDirectives?.length) suppressed = true;
  const host = {
    getSourceFile: file => file === name ? source : undefined,
    getDefaultLibFileName: () => '', writeFile: () => {},
    getCurrentDirectory: () => '/', getDirectories: () => [],
    fileExists: file => file === name, readFile: file => file === name ? unit.text : undefined,
    getCanonicalFileName: file => file, useCaseSensitiveFileNames: () => true,
    getNewLine: () => '\n',
  };
  const program = ts.createProgram([name], options, host);
  const errors = [...program.getSyntacticDiagnostics(source),
    ...program.getSemanticDiagnostics(source).filter(d => d.code === 2588)];
  for (const error of errors) {
    if (diagnostics.length >= 20) { truncated = true; break; }
    const position = source.getLineAndCharacterOfPosition(error.start || 0);
    diagnostics.push({code: 'TS' + error.code,
      line: unit.line + position.line,
      column: position.character + 1 + (position.line === 0 ? unit.column - 1 : 0),
      message: ts.flattenDiagnosticMessageText(error.messageText, '\n').slice(0, 500)});
  }
  if (truncated) break;
}
process.stdout.write(JSON.stringify({status: suppressed ? 'checked_with_suppressions' : 'checked', parser: 'TypeScript ' + ts.version, diagnostics, truncated}));
