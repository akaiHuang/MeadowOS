"""Small, safe Markdown presentation for native GTK chat labels.

Only markup emitted here is interpreted by Pango. The original model message
must remain in the caller's history/copy state; this is a display transform.
Links are readable text, never executable actions. Artifact opening belongs to
the separate, trusted artifact UI, not model-supplied markup.
"""
import html
import re


# Very long messages still display in full. Past these limits, text is escaped
# without Markdown parsing so streamed/untrusted input has a bounded parse cost.
_FORMAT_LIMIT = 64 * 1024
_INLINE_LIMIT = 8192
_INVALID_XML = re.compile(r"[\x00-\x08\x0b\x0c\x0e-\x1f\ud800-\udfff\ufffe\uffff]")
_FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})([^\r\n]*)$")
_HEADING = re.compile(r"^ {0,3}#{1,6}[ \t]+(.+?)(?:[ \t]+#+[ \t]*)?$")
_BULLET = re.compile(r"^([ \t]*)([-+*])[ \t]+(.+)$")
_PANGO_TAGS = ("<b>", "</b>", "<tt>", "</tt>")
_INLINE = re.compile(
    r"(?P<escape>\\[\\`*_\[\]()#>\-])"
    r"|(?P<code>`[^`\r\n]+`)"
    r"|(?P<bold>\*\*[^*\r\n]+\*\*|__[^_\r\n]+__)"
    r"|(?P<link>\[(?P<label>[^\[\]\r\n]{1,512})\]"
    r"\((?P<target>(?:https?://|mailto:)[^\s<>()]{1,2048})\))"
)


def _escape(text):
    # Pango cannot accept XML-invalid controls/surrogates even after escaping.
    # Replacing only their presentation keeps an unexpected byte from blanking
    # the entire label; the caller retains the untouched original message.
    return html.escape(_INVALID_XML.sub("\ufffd", text), quote=True)


def _inline(text, nested=False):
    if len(text) > _INLINE_LIMIT:
        return _escape(text)
    result = []
    end = 0
    for token in _INLINE.finditer(text):
        result.append(_escape(text[end:token.start()]))
        value = token.group(0)
        if token.group("escape"):
            result.append(_escape(value[1:]))
        elif token.group("code"):
            result.append("<tt>" + _escape(value[1:-1]) + "</tt>")
        elif token.group("bold"):
            # At most one nested pass, for inline code inside a bold phrase.
            content = _escape(value[2:-2]) if nested else _inline(value[2:-2], True)
            result.append("<b>" + content + "</b>")
        else:
            # A target is shown verbatim and cannot launch a command or file.
            title = _escape(token.group("label")) if nested else _inline(token.group("label"), True)
            target = _escape(token.group("target"))
            result.append(title if token.group("label") == token.group("target") else title + " (" + target + ")")
        end = token.end()
    result.append(_escape(text[end:]))
    return "".join(result)


def _trim_trailing_presentation(markup):
    position = len(markup)
    tags = []
    while position:
        if markup[position - 1].isspace():
            position -= 1
            continue
        tag = next((tag for tag in _PANGO_TAGS
                    if markup.endswith(tag, 0, position)), None)
        if tag is None:
            break
        tags.append(tag)
        position -= len(tag)
    return markup[:position] + "".join(reversed(tags))


def markdown_to_pango(text, *, trim_trailing=False):
    """Render common chat Markdown with balanced, escaped Pango markup.

    Supports bold, inline/fenced code, headings, bullets and readable web links.
    Unsupported or unfinished inline syntax remains literal while streaming.
    An unfinished code fence displays its received content as code. No HTML,
    images, styles, links or external resources supplied in text are executed.
    Runtime is bounded per formatted line; oversized content falls back to
    escaped text without dropping the message or changing stored model output.
    ``trim_trailing`` removes only outer trailing presentation whitespace for
    assistant bubbles. Leading whitespace, paragraph gaps and code indentation
    remain intact; callers continue to retain the original model text.
    """
    if trim_trailing:
        text = text.rstrip()
    result = []
    formatted = 0
    fence = None
    for line in text.splitlines(keepends=True):
        content = line.rstrip("\r\n")
        newline = line[len(content):]
        marker = _FENCE.fullmatch(content)
        if fence:
            if marker and marker[1][0] == fence[0] and len(marker[1]) >= len(fence) and not marker[2].strip():
                result.append("</tt>" + newline)
                fence = None
            else:
                result.append(_escape(line))
            continue
        if formatted + len(line) > _FORMAT_LIMIT or len(content) > _INLINE_LIMIT:
            result.append(_escape(line))
            continue
        formatted += len(line)
        if marker and not (marker[1][0] == "`" and "`" in marker[2]):
            fence = marker[1]
            result.append("<tt>")
            continue
        heading = _HEADING.fullmatch(content)
        bullet = _BULLET.fullmatch(content)
        if heading:
            result.append("<b>" + _inline(heading[1]) + "</b>" + newline)
        elif bullet:
            result.append(_escape(bullet[1]) + "\u2022 " + _inline(bullet[3]) + newline)
        else:
            result.append(_inline(content) + newline)
    if fence:
        result.append("</tt>")
    markup = "".join(result)
    if trim_trailing:
        # A final fenced block has a newline before its closing marker. Trim
        # that visible suffix too, while preserving every balanced Pango tag.
        # Only tags emitted by this formatter are recognized here.
        markup = _trim_trailing_presentation(markup)
    return markup
