"""Native desktop changes wait for EWMH readback without closing applications."""
import sys
from pathlib import Path
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from workspace_lifecycle import WorkspaceLifecycle


class Loop:
    def __init__(self):
        self.now = 0
        self.sources = {}
        self.next_id = 1

    def timeout_add(self, _interval, callback):
        source = self.next_id
        self.next_id += 1
        self.sources[source] = callback
        return source

    def source_remove(self, source):
        self.sources.pop(source, None)

    def tick(self, elapsed=.04):
        self.now += elapsed
        for source, callback in list(self.sources.items()):
            if not callback():
                self.sources.pop(source, None)


class Workspace:
    def __init__(self, screen, number):
        self.screen, self.number = screen, number

    def get_number(self):
        return self.number

    def activate(self, timestamp):
        def apply():
            self.screen.active = self
        self.screen.request("activate", (self.number, timestamp), apply)


class Window:
    def __init__(self, screen, xid, workspace, *, pinned=False, sticky=False):
        self.screen, self.xid = screen, xid
        self.workspace, self.pinned, self.sticky = workspace, pinned, sticky

    def get_xid(self):
        return self.xid

    def get_workspace(self):
        return self.workspace

    def is_pinned(self):
        return self.pinned

    def is_sticky(self):
        return self.sticky

    def move_to_workspace(self, workspace):
        def apply():
            self.workspace = workspace
        self.screen.request("move", (self.xid, workspace.number), apply)

    def activate(self, timestamp):
        def apply():
            self.screen.focused = self
        self.screen.request("focus", (self.xid, timestamp), apply)

    def close(self, *_args):
        raise AssertionError("desktop operations must never close a window")


class Screen:
    def __init__(self, count=4, active=2):
        self.workspaces = [Workspace(self, index) for index in range(count)]
        self.active = self.workspaces[min(active, count - 1)]
        self.windows = [Window(self, index + 100, workspace)
                        for index, workspace in enumerate(self.workspaces)]
        self.focused = self.windows[self.active.number]
        self.requests, self.pending = [], []

    def request(self, kind, data, apply):
        self.requests.append((kind, data))
        self.pending.append((kind, apply))

    def acknowledge(self, kind=None):
        waiting, self.pending = self.pending, []
        for item_kind, apply in waiting:
            if kind is None or kind == item_kind:
                apply()
            else:
                self.pending.append((item_kind, apply))

    def force_update(self):
        pass

    def get_workspace_count(self):
        return len(self.workspaces)

    def get_workspaces(self):
        return self.workspaces[:]

    def get_workspace(self, index):
        return self.workspaces[index] if 0 <= index < len(self.workspaces) else None

    def get_active_workspace(self):
        return self.active

    def get_active_window(self):
        return self.focused

    def get_windows(self):
        return self.windows[:]

    def change_workspace_count(self, count):
        def apply():
            while len(self.workspaces) < count:
                self.workspaces.append(Workspace(self, len(self.workspaces)))
            self.workspaces = self.workspaces[:count]
        self.request("count", count, apply)


class WorkspaceLifecycleTests(unittest.TestCase):
    def setUp(self):
        self.loop = Loop()
        self.screen = Screen()
        self.busy, self.results, self.errors = [], [], []
        self.controller = self.make_controller()

    def make_controller(self, **kwargs):
        return WorkspaceLifecycle(self.screen, glib=self.loop,
            timestamp=lambda: 1234, on_busy=self.busy.append,
            on_complete=self.results.append, on_error=self.errors.append,
            clock=lambda: self.loop.now, **kwargs)

    def drive(self, limit=30):
        for _ in range(limit):
            if not self.controller.busy:
                return
            self.screen.acknowledge()
            self.loop.tick()
        self.fail("operation did not complete")

    def test_constructing_preserves_existing_desktop_count_and_windows(self):
        self.assertEqual(self.screen.get_workspace_count(), 4)
        self.assertEqual(self.screen.requests, [])
        self.assertEqual(self.loop.sources, {})

    def test_add_waits_for_both_count_and_activation_acknowledgments(self):
        self.assertTrue(self.controller.add_workspace())
        self.assertEqual(self.screen.requests, [("count", 5)])
        self.loop.tick()
        self.assertEqual(self.results, [])
        self.assertEqual(self.screen.get_workspace_count(), 4)
        self.screen.acknowledge("count")
        self.loop.tick()
        self.assertEqual(self.screen.requests[-1], ("activate", (4, 1234)))
        self.loop.tick()
        self.assertTrue(self.controller.busy)
        self.screen.acknowledge("activate")
        self.loop.tick()
        self.assertEqual((self.results[0].action, self.results[0].count,
                          self.results[0].active_index), ("add", 5, 4))
        self.assertEqual(self.busy, [True, False])
        self.assertEqual(self.loop.sources, {})

    def test_rapid_add_and_close_are_serialized_by_rejecting_while_busy(self):
        self.assertTrue(self.controller.add_workspace())
        for _ in range(20):
            self.assertFalse(self.controller.add_workspace())
            self.assertFalse(self.controller.close_workspace(1))
        self.drive()
        self.assertEqual(self.screen.get_workspace_count(), 5)
        self.assertTrue(self.controller.close_workspace(4))
        self.drive()
        self.assertEqual(self.screen.get_workspace_count(), 4)
        self.assertEqual(self.errors, [])

    def test_all_closed_and_active_indices_keep_window_and_focus_mapping(self):
        for closed in range(4):
            for active in range(4):
                with self.subTest(closed=closed, active=active):
                    self.screen = Screen(active=active)
                    self.controller = self.make_controller()
                    originals = self.screen.windows[:]
                    focused = self.screen.focused
                    self.assertTrue(self.controller.close_workspace(closed))
                    self.drive()
                    self.assertEqual(self.screen.get_workspace_count(), 3)
                    expected = lambda index: (max(0, closed - 1) if index == closed
                                             else index - 1 if index > closed else index)
                    self.assertEqual([window.workspace.number for window in originals],
                                     [expected(index) for index in range(4)])
                    self.assertEqual(self.screen.active.number, expected(active))
                    self.assertIs(self.screen.focused, focused)
                    self.assertEqual(len(self.screen.windows), 4)
                    self.assertEqual(self.loop.sources, {})

    def test_move_and_active_acknowledgments_precede_tail_count_reduction(self):
        self.assertTrue(self.controller.close_workspace(1))
        self.assertEqual(self.screen.requests,
                         [("move", (101, 0)), ("move", (102, 1)), ("move", (103, 2))])
        for _ in range(3):
            self.loop.tick()
        self.assertFalse(any(kind == "count" for kind, _ in self.screen.requests))
        self.screen.acknowledge("move")
        self.loop.tick()
        self.loop.tick()
        self.assertEqual(self.screen.requests[-1], ("activate", (1, 1234)))
        self.loop.tick()
        self.assertFalse(any(kind == "count" for kind, _ in self.screen.requests))
        self.screen.acknowledge("activate")
        self.loop.tick()
        self.assertEqual(self.screen.requests[-1], ("count", 3))
        self.drive()

    def test_pinned_and_workspace_less_windows_are_never_moved(self):
        self.screen.windows += [Window(self.screen, 200, None, pinned=True),
                                Window(self.screen, 201, None, sticky=True, pinned=True),
                                Window(self.screen, 202, None)]
        self.assertTrue(self.controller.close_workspace(1))
        self.drive()
        moved = {data[0] for kind, data in self.screen.requests if kind == "move"}
        self.assertFalse(moved & {200, 201, 202})
        self.assertTrue(self.screen.windows[-3].pinned)
        self.assertTrue(self.screen.windows[-2].sticky)

    def test_viewport_sticky_window_compacts_desktop_without_losing_sticky_flag(self):
        sticky = Window(self.screen, 201, self.screen.workspaces[2], sticky=True)
        self.screen.windows.append(sticky)
        self.assertTrue(self.controller.close_workspace(1))
        self.drive()
        self.assertIn(("move", (201, 1)), self.screen.requests)
        self.assertEqual(sticky.workspace.number, 1)
        self.assertTrue(sticky.sticky)
        self.assertFalse(sticky.pinned)

    def test_unaffected_lower_desktop_windows_get_no_move_request(self):
        self.assertTrue(self.controller.close_workspace(2))
        self.drive()
        moved = {data[0] for kind, data in self.screen.requests if kind == "move"}
        self.assertEqual(moved, {102, 103})

    def test_new_windows_are_remapped_while_movement_is_pending(self):
        self.assertTrue(self.controller.close_workspace(1))
        new = Window(self.screen, 300, self.screen.workspaces[3])
        self.screen.windows.append(new)
        self.loop.tick()
        self.assertIn(("move", (300, 2)), self.screen.requests)
        self.drive()
        self.assertEqual(new.workspace.number, 2)
        self.assertEqual(sum(data[0] == 300 for kind, data in self.screen.requests
                             if kind == "move"), 1)

    def test_already_shifted_windows_are_not_moved_twice(self):
        self.assertTrue(self.controller.close_workspace(1))
        self.screen.acknowledge("move")
        for _ in range(8):
            self.loop.tick()
        self.assertEqual([data for kind, data in self.screen.requests if kind == "move"],
                         [(101, 0), (102, 1), (103, 2)])
        self.drive()

    def test_window_that_exits_during_operation_does_not_prevent_closure(self):
        self.assertTrue(self.controller.close_workspace(1))
        self.screen.windows.remove(self.screen.focused)
        self.screen.focused = None
        self.drive()
        self.assertEqual(self.screen.get_workspace_count(), 3)
        self.assertEqual(self.errors, [])

    def test_original_focus_is_restored_after_native_relocation(self):
        focused = self.screen.focused
        self.assertTrue(self.controller.close_workspace(1))
        self.screen.focused = self.screen.windows[0]
        self.drive()
        self.assertIs(self.screen.focused, focused)
        self.assertIn(("focus", (focused.xid, 1234)), self.screen.requests)

    def test_final_desktop_cannot_be_closed(self):
        self.screen = Screen(count=1)
        self.controller = self.make_controller()
        self.assertFalse(self.controller.close_workspace(0))
        self.assertFalse(self.controller.busy)
        self.assertEqual(self.screen.requests, [])

    def test_invalid_and_destroyed_workspace_targets_are_rejected(self):
        stale = Workspace(self.screen, 1)
        for target in (-1, 4, None, stale, True, "1"):
            self.assertFalse(self.controller.close_workspace(target))
        self.assertEqual(self.screen.requests, [])

    def test_current_wnck_workspace_object_is_accepted(self):
        self.assertTrue(self.controller.close_workspace(self.screen.workspaces[1]))
        self.drive()
        self.assertEqual(self.results[-1].closed_index, 1)

    def test_unacknowledged_moves_timeout_keeps_mapping_and_rejects_reentry(self):
        self.controller = self.make_controller(timeout_ms=100)
        self.assertTrue(self.controller.close_workspace(1))
        self.loop.tick(.101)
        self.assertTrue(self.controller.busy)
        self.assertEqual(self.screen.get_workspace_count(), 4)
        self.assertFalse(any(kind == "count" for kind, _ in self.screen.requests))
        self.assertEqual(self.busy, [True])
        self.assertIn("move-windows", self.errors[0])
        self.assertEqual(self.results, [])
        self.assertFalse(self.controller.close_workspace(1))
        self.assertFalse(self.controller.add_workspace())
        self.loop.tick(1)
        self.assertEqual(len(self.errors), 1)
        self.drive()
        self.assertEqual(self.screen.get_workspace_count(), 3)
        self.assertEqual(self.busy, [True, False])
        self.assertEqual(self.loop.sources, {})
        self.assertIsNone(self.controller.error)

    def test_add_timeout_reconciles_late_count_and_does_not_accept_duplicate(self):
        self.controller = self.make_controller(timeout_ms=100)
        self.assertTrue(self.controller.add_workspace())
        self.loop.tick(.101)
        self.assertTrue(self.controller.busy)
        self.assertIn("add-count", self.errors[0])
        self.assertEqual(self.screen.get_workspace_count(), 4)
        self.assertFalse(self.controller.add_workspace())
        self.drive()
        self.assertEqual(self.screen.get_workspace_count(), 5)
        self.assertEqual(self.screen.active.number, 4)

    def test_partial_move_timeout_cannot_shift_original_windows_twice(self):
        self.controller = self.make_controller(timeout_ms=100)
        self.assertTrue(self.controller.close_workspace(1))
        for _ in range(2):
            _kind, apply = self.screen.pending.pop(0)
            apply()
        self.loop.tick(.101)
        self.assertEqual([window.workspace.number for window in self.screen.windows],
                         [0, 0, 1, 3])
        self.assertFalse(self.controller.close_workspace(1))
        self.drive()
        self.assertEqual([window.workspace.number for window in self.screen.windows],
                         [0, 0, 1, 2])
        self.assertEqual([data for kind, data in self.screen.requests if kind == "move"],
                         [(101, 0), (102, 1), (103, 2)])

    def test_external_workspace_count_change_aborts_before_reduction(self):
        self.assertTrue(self.controller.close_workspace(1))
        self.screen.workspaces.append(Workspace(self.screen, 4))
        self.loop.tick()
        self.assertFalse(self.controller.busy)
        self.assertTrue(self.errors)
        self.assertFalse(any(kind == "count" for kind, _ in self.screen.requests))

    def test_native_exception_is_reported_and_does_not_leave_busy_state(self):
        def fail(_count):
            raise RuntimeError("WM unavailable")
        self.screen.change_workspace_count = fail
        self.assertFalse(self.controller.add_workspace())
        self.assertFalse(self.controller.busy)
        self.assertIn("WM unavailable", self.errors[0])
        self.assertEqual(self.busy, [True, False])

    def test_completion_can_start_a_new_operation_without_leaking_old_timer(self):
        def complete(result):
            self.results.append(result)
            if len(self.results) == 1:
                self.controller.add_workspace()
        self.controller.on_complete = complete
        self.controller.add_workspace()
        self.drive()
        self.assertEqual(self.screen.get_workspace_count(), 6)
        self.assertEqual(len(self.results), 2)
        self.assertEqual(self.loop.sources, {})

    def test_dispose_cancels_only_readback(self):
        self.controller.add_workspace()
        self.controller.dispose()
        self.assertEqual(self.loop.sources, {})
        self.assertFalse(self.controller.busy)
        self.assertEqual(self.screen.requests, [("count", 5)])


if __name__ == "__main__":
    unittest.main()
