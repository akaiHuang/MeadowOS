"""Bounded native-composer edit history; GTK behavior has an isolated fixture."""
import importlib.util
from pathlib import Path
import sys
import unittest

SOURCE = Path(__file__).resolve().parents[1] / "text_editing.py"
spec = importlib.util.spec_from_file_location("text_editing", SOURCE)
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
EditHistory, TextSnapshot = module.EditHistory, module.TextSnapshot


def snapshot(text, insert=None, bound=None):
    insert = len(text) if insert is None else insert
    return TextSnapshot(text, insert, insert if bound is None else bound)


class EditingHistoryTests(unittest.TestCase):
    def test_undo_and_redo_preserve_unicode_and_selection_direction(self):
        history = EditHistory()
        before = snapshot("我想編輯這段文字", 7, 3)
        after = snapshot("我想編重新輸入字", 7)
        history.record(before, after)
        self.assertEqual(history.undo(), before)
        self.assertIsNone(history.undo())
        self.assertEqual(history.redo(), after)
        self.assertIsNone(history.redo())

    def test_new_edit_after_undo_drops_redo_branch(self):
        history = EditHistory()
        history.record(snapshot(""), snapshot("a"))
        history.record(snapshot("a"), snapshot("ab"))
        history.undo()
        history.record(snapshot("a"), snapshot("ac"))
        self.assertIsNone(history.redo())
        self.assertEqual(history.undo().text, "a")
        self.assertEqual(history.undo().text, "")
        self.assertEqual(history.bytes_used, 4)

    def test_same_text_or_selection_change_is_not_an_edit(self):
        history = EditHistory()
        history.record(snapshot("a"), snapshot("ab"))
        history.undo()
        history.record(snapshot("a", 0), snapshot("a", 1))
        self.assertEqual(history.redo().text, "ab")

    def test_send_boundary_keeps_current_text_outside_history(self):
        history = EditHistory()
        history.record(snapshot("sent message"), snapshot("new draft"))
        history.clear()
        self.assertIsNone(history.undo())
        self.assertIsNone(history.redo())
        history.record(snapshot("new draft"), snapshot("new draft!"))
        self.assertEqual(history.undo().text, "new draft")
        self.assertIsNone(history.undo())

    def test_oldest_complete_edits_are_evicted_by_step_limit(self):
        history = EditHistory(max_steps=2)
        for before, after in (("", "a"), ("a", "ab"), ("ab", "abc")):
            history.record(snapshot(before), snapshot(after))
        self.assertEqual(history.undo().text, "ab")
        self.assertEqual(history.undo().text, "a")
        self.assertIsNone(history.undo())

    def test_byte_budget_counts_unicode_and_both_undo_redo(self):
        history = EditHistory(max_bytes=12)
        history.record(snapshot(""), snapshot("中"))
        history.record(snapshot("中"), snapshot("中文"))
        self.assertEqual(history.bytes_used, 12)
        history.undo()
        self.assertEqual(history.bytes_used, 12)
        history.record(snapshot("中"), snapshot("中a"))
        self.assertEqual(history.bytes_used, 10)
        self.assertIsNone(history.redo())

    def test_oversized_edit_clears_old_undo_instead_of_erasing_new_text(self):
        history = EditHistory(max_bytes=12)
        history.record(snapshot(""), snapshot("a"))
        history.record(snapshot("a"), snapshot("中文測試過長"))
        self.assertEqual(history.bytes_used, 0)
        self.assertIsNone(history.undo())
        self.assertIsNone(history.redo())


if __name__ == "__main__":
    unittest.main()
