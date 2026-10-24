"""Presentation-only Markdown handling; original model text stays untouched."""
from pathlib import Path
import sys
import unittest
import xml.etree.ElementTree as ET

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from chat_markup import markdown_to_pango


def visible(markup):
    return "".join(ET.fromstring("<markup>" + markup + "</markup>").itertext())


class ChatMarkupTests(unittest.TestCase):
    def test_screenshot_reply_is_readable_without_literal_markers(self):
        source = "檔案已建立：\n\n**路徑**：`/workspace/snake_game.html`\n\n**功能**\n- Canvas 貪食蛇，網格 20×20\n- 方向鍵 / WASD 控制"
        result = markdown_to_pango(source)
        self.assertIn("<b>路徑</b>：<tt>/workspace/snake_game.html</tt>", result)
        self.assertIn("• 方向鍵 / WASD 控制", visible(result))
        self.assertNotIn("**", visible(result))
        self.assertIn("**路徑**", source)

    def test_model_html_is_escaped_and_cannot_set_style_or_launch(self):
        source = '<a href="file:///secret">open</a> & <span size="999999">big</span>'
        result = markdown_to_pango(source)
        self.assertEqual(visible(result), source)
        self.assertNotIn("<a ", result)
        self.assertNotIn("<span ", result)

    def test_code_protects_markup_and_markdown(self):
        source = '`<span>**not bold** & data</span>`'
        result = markdown_to_pango(source)
        self.assertTrue(result.startswith("<tt>"))
        self.assertEqual(visible(result), "<span>**not bold** & data</span>")
        self.assertNotIn("<b>", result)

    def test_fenced_code_does_not_parse_as_markup(self):
        source = '```html\n<b>**not bold**</b>\n```\nDone'
        result = markdown_to_pango(source)
        self.assertEqual(visible(result), '<b>**not bold**</b>\n\nDone')
        self.assertIn("<tt>", result)
        self.assertNotIn("<b>", result)

    def test_every_streaming_prefix_has_valid_markup(self):
        source = '# 回覆\n**檔案** `[name].html`\n[網站](https://example.test/?x=1&y=2)\n```js\nif (x < 2) { run(); }\n```'
        for length in range(len(source) + 1):
            with self.subTest(length=length):
                visible(markdown_to_pango(source[:length]))
        self.assertEqual(visible(markdown_to_pango("**未完成")), "**未完成")
        self.assertEqual(visible(markdown_to_pango("`未完成")), "`未完成")

    def test_links_are_readable_text_and_unsafe_targets_do_not_become_actions(self):
        source = '[網站](https://example.test/?a=1&b=2) [危險](javascript:alert(1)) [檔案](/workspace/a.html)'
        result = markdown_to_pango(source)
        self.assertEqual(visible(result), '網站 (https://example.test/?a=1&b=2) [危險](javascript:alert(1)) [檔案](/workspace/a.html)')
        self.assertNotIn("href=", result)

    def test_headings_bullets_ordered_lists_and_indents(self):
        source = '# 標題\n  - **項目**\n1. 第一步\n2. 第二步\n\n一般文字'
        result = markdown_to_pango(source)
        self.assertEqual(visible(result), '標題\n  • 項目\n1. 第一步\n2. 第二步\n\n一般文字')
        self.assertIn('<b>標題</b>', result)

    def test_escaped_markers_and_inline_code_inside_bold(self):
        result = markdown_to_pango(r'\*\*literal\*\* **run `node --check`**')
        self.assertEqual(visible(result), '**literal** run node --check')
        self.assertIn('<b>run <tt>node --check</tt></b>', result)

    def test_unclosed_fences_show_content_without_invalid_markup(self):
        self.assertEqual(visible(markdown_to_pango('```python\nprint("<ok>")')), 'print("<ok>")')
        self.assertEqual(visible(markdown_to_pango('~~~~\n```\n~~\n')), '```\n~~\n')

    def test_long_adversarial_input_remains_complete(self):
        source = '[**`' * 20000 + '<last>'
        self.assertEqual(visible(markdown_to_pango(source)), source)
        large_message = ('plain\n' * 12000) + '**at end**'
        self.assertEqual(visible(markdown_to_pango(large_message)), large_message)

    def test_invalid_xml_controls_do_not_blank_entire_message(self):
        self.assertEqual(visible(markdown_to_pango('before\x00\ud800after')), 'before��after')

    def test_empty_plain_unicode_and_crlf(self):
        for source in ('', 'Meadow，你好 👋', 'first\r\nsecond', 'a & b < c'):
            with self.subTest(source=source):
                # XML parsers normalize CRLF, while Pango receives the original.
                self.assertEqual(visible(markdown_to_pango(source)), source.replace('\r\n', '\n'))

    def test_assistant_trailing_space_is_presentation_only(self):
        source = '  第一段\n\n第二段  \n\n\t\u3000'
        self.assertEqual(visible(markdown_to_pango(source, trim_trailing=True)),
                         '  第一段\n\n第二段')
        self.assertEqual(visible(markdown_to_pango(source)), source)
        self.assertTrue(source.endswith('\n\n\t\u3000'))

    def test_final_fenced_block_has_no_extra_presentation_line(self):
        source = '```python\n  print("保持縮排")\n\n```\n\n'
        result = markdown_to_pango(source, trim_trailing=True)
        self.assertEqual(visible(result), '  print("保持縮排")')
        self.assertEqual(result, '<tt>  print(&quot;保持縮排&quot;)</tt>')
        self.assertEqual(source, '```python\n  print("保持縮排")\n\n```\n\n')

    def test_trailing_trim_keeps_interior_blank_lines_and_balanced_markup(self):
        source = '**第一段**\n\n```text\n  中間\n\n  最後\n```\n\n**結尾 `ok`**\n\n'
        result = markdown_to_pango(source, trim_trailing=True)
        self.assertEqual(visible(result), '第一段\n\n  中間\n\n  最後\n\n\n結尾 ok')
        self.assertTrue(result.endswith('<b>結尾 <tt>ok</tt></b>'))
        for length in range(len(source) + 1):
            with self.subTest(length=length):
                visible(markdown_to_pango(source[:length], trim_trailing=True))

    def test_whitespace_only_markup_and_large_interior_space(self):
        self.assertEqual(visible(markdown_to_pango('hello\n\n** **', trim_trailing=True)), 'hello')
        source = 'left' + ' ' * 200000 + 'right\n\n'
        self.assertEqual(visible(markdown_to_pango(source, trim_trailing=True)), source.rstrip())


if __name__ == '__main__':
    unittest.main()
