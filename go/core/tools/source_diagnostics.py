# Trusted embedded runner. Source is JSON data, never imported or executed.
import ast
import base64
import json
import sys

source = base64.b64decode(json.load(sys.stdin)["source_base64"], validate=True)
diagnostics = []
try:
    ast.parse(source, filename="<saved-source>", mode="exec")
except SyntaxError as error:
    source_line = (error.text or "").rstrip("\r\n")
    if len(source_line) > 300:
        source_line = source_line[:300] + "…"
    diagnostics.append({
        "code": "PY_SYNTAX",
        "line": error.lineno or 1,
        "column": error.offset or 1,
        "message": (error.msg or "invalid Python syntax")[:500],
        "source_line": source_line,
    })
except (ValueError, MemoryError, RecursionError) as error:
    # Parser resource/failure limits are not proof that user source is invalid.
    sys.exit("Python parser could not finish: " + type(error).__name__)

sys.stdout.write(json.dumps({
    "status": "checked",
    "parser": "CPython " + sys.version.split()[0] + " ast.parse",
    "diagnostics": diagnostics,
}))
