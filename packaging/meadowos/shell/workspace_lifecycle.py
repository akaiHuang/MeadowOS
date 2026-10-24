"""Acknowledged, serial workspace changes for the native X11 shell.

The caller supplies its existing Wnck.Screen.  Importing this module does not
load GTK, connect to a display, or change the initial workspace count.  The
screen and GLib arguments are deliberately duck typed for isolated tests.
"""
from __future__ import annotations

from dataclasses import dataclass, field
import logging
import time


@dataclass(frozen=True)
class WorkspaceResult:
    action: str
    count: int
    active_index: int
    closed_index: int | None = None


@dataclass
class _Operation:
    action: str
    count: int
    target: int
    phase: str
    deadline: float
    closed_index: int | None = None
    focused_xid: int | None = None
    # Track original window identities so an already shifted window is never
    # shifted twice while asynchronous EWMH requests are acknowledged.
    seen: set = field(default_factory=set)
    moves: dict = field(default_factory=dict)
    settled: bool = False


class WorkspaceLifecycle:
    """Perform one EWMH operation at a time on the GTK main loop.

    ``add_workspace`` and ``close_workspace`` return True only when accepted.
    While busy, further requests return False; controls should use on_busy to
    disable themselves.  on_complete receives WorkspaceResult and on_error a
    human readable string.  Each phase waits for native readback, with a bounded
    timeout notification. EWMH requests cannot be cancelled: after a timeout the
    controller stays busy and reconciles late acknowledgments before accepting
    another mutation. A failed move never triggers a workspace-count reduction.
    """

    def __init__(self, screen, *, timestamp=None, glib=None, on_busy=None,
                 on_complete=None, on_error=None, timeout_ms=4000, poll_ms=40,
                 clock=None):
        if glib is None:
            from gi.repository import GLib
            glib = GLib
        self.screen = screen
        self.glib = glib
        self.timestamp = timestamp or (lambda: 0)
        self.on_busy = on_busy
        self.on_complete = on_complete
        self.on_error = on_error
        self.timeout = max(1, int(timeout_ms)) / 1000
        self.poll_ms = max(1, int(poll_ms))
        self.clock = clock or time.monotonic
        self.error = None
        self._operation = None
        self._timer = None
        self._polling = False

    @property
    def busy(self):
        return self._operation is not None

    def add_workspace(self):
        if self.busy:
            return False
        try:
            self.screen.force_update()
            count = self.screen.get_workspace_count()
            if count < 1:
                return self._reject("尚未取得桌面清單，請稍後再試。")
            operation = _Operation("add", count, count, "add-count", 0)
            self._begin(operation)
            self.screen.change_workspace_count(count + 1)
            self._schedule()
            return True
        except Exception as exc:
            return self._fail(f"無法新增桌面：{exc}")

    def close_workspace(self, workspace_or_index):
        if self.busy:
            return False
        try:
            self.screen.force_update()
            workspaces = list(self.screen.get_workspaces())
            count = len(workspaces)
            if count <= 1:
                return False
            # A stale destroyed Wnck object must not close a different desktop.
            if isinstance(workspace_or_index, int) and not isinstance(workspace_or_index, bool):
                index = workspace_or_index
            elif workspace_or_index in workspaces:
                index = workspace_or_index.get_number()
            else:
                return False
            if not 0 <= index < count:
                return False
            active = self.screen.get_active_workspace()
            active_index = active.get_number() if active else 0
            target = self._destination(active_index, index)
            focused = self.screen.get_active_window()
            operation = _Operation("close", count, target, "move-windows", 0,
                                   closed_index=index,
                                   focused_xid=focused.get_xid() if focused else None)
            self._begin(operation)
            self._collect_moves(operation)
            self._schedule()
            return True
        except Exception as exc:
            return self._fail(f"無法關閉桌面：{exc}")

    @staticmethod
    def _destination(index, closed_index):
        if index == closed_index:
            return max(0, closed_index - 1)
        return index - 1 if index > closed_index else index

    def _begin(self, operation):
        self._operation = operation
        self.error = None
        self._phase(operation, operation.phase)
        self._emit(self.on_busy, True)

    def _schedule(self):
        if self.busy and self._timer is None:
            self._timer = self.glib.timeout_add(self.poll_ms, self._tick)

    def _phase(self, operation, phase):
        operation.phase = phase
        operation.deadline = self.clock() + self.timeout

    @staticmethod
    def _pinned(window):
        # Wnck sticky means viewport-fixed, whereas pinned means present on
        # every workspace.  Moving a viewport-sticky window preserves its flag;
        # moving a pinned window would unpin it, so only the latter is skipped.
        return window.is_pinned()

    def _collect_moves(self, operation):
        """Also catch windows opened while earlier requests were in flight."""
        added = False
        for window in self.screen.get_windows():
            xid = window.get_xid()
            if xid in operation.seen:
                continue
            operation.seen.add(xid)
            workspace = window.get_workspace()
            if workspace is None or self._pinned(window):
                continue
            index = workspace.get_number()
            destination = self._destination(index, operation.closed_index)
            if destination == index:
                continue
            target = self.screen.get_workspace(destination)
            if target is None:
                raise RuntimeError("目標桌面已變更")
            operation.moves[xid] = (window, destination)
            window.move_to_workspace(target)
            added = True
        if added:
            operation.settled = False
        return added

    def _moves_acknowledged(self, operation):
        current = {window.get_xid(): window for window in self.screen.get_windows()}
        for xid, (_original, destination) in operation.moves.items():
            window = current.get(xid)
            # Applications may finish normally during a desktop operation.  A
            # window pinned by its application/user should also remain pinned.
            if window is None or self._pinned(window):
                continue
            workspace = window.get_workspace()
            if workspace is None or workspace.get_number() != destination:
                return False
        return True

    def _active_index(self):
        active = self.screen.get_active_workspace()
        return active.get_number() if active else None

    def _activate(self, operation, phase):
        target = self.screen.get_workspace(operation.target)
        if target is None:
            raise RuntimeError("目標桌面已變更")
        self._phase(operation, phase)
        target.activate(self.timestamp())

    def _tick(self):
        operation = self._operation
        if operation is None:
            return False
        self._polling = True
        try:
            self.screen.force_update()
            count = self.screen.get_workspace_count()
            phase = operation.phase
            if phase == "add-count":
                if count == operation.count + 1:
                    self._activate(operation, "add-active")
                elif count != operation.count:
                    raise RuntimeError("桌面數量已由其他程式變更")
            elif phase == "add-active":
                if count != operation.count + 1:
                    raise RuntimeError("桌面數量已由其他程式變更")
                if self._active_index() == operation.target:
                    self._complete(operation)
            elif phase in ("move-windows", "close-active"):
                if count != operation.count:
                    raise RuntimeError("桌面數量已由其他程式變更；已停止關閉")
                added = self._collect_moves(operation)
                acknowledged = self._moves_acknowledged(operation)
                if not acknowledged or added:
                    operation.settled = False
                elif not operation.settled:
                    # One more main-loop cycle catches newly mapped windows
                    # before the final desktop can be removed by the WM.
                    operation.settled = True
                elif phase == "move-windows":
                    self._activate(operation, "close-active")
                elif self._active_index() == operation.target:
                    self._phase(operation, "close-count")
                    self.screen.change_workspace_count(operation.count - 1)
            elif phase == "close-count":
                if count == operation.count - 1:
                    self._activate(operation, "close-focus")
                    focused = self._focused_window(operation)
                    if focused is not None:
                        focused.activate(self.timestamp())
                elif count != operation.count:
                    raise RuntimeError("桌面數量已由其他程式變更")
            elif phase == "close-focus":
                if count != operation.count - 1:
                    raise RuntimeError("桌面數量已由其他程式變更")
                focused = self._focused_window(operation)
                active = self.screen.get_active_window()
                focus_ready = focused is None or (active is not None and
                    active.get_xid() == operation.focused_xid)
                if self._active_index() == operation.target and focus_ready:
                    self._complete(operation)
            if self._operation is operation and self.clock() >= operation.deadline:
                # A timeout cannot cancel requests already queued by the WM.
                # Keep the original identity mapping until readback catches up;
                # retrying a partial close would otherwise shift windows twice.
                operation.deadline = float("inf")
                self.error = (f"桌面操作尚未獲視窗管理員確認（{operation.phase}）；"
                              "正在等待確認，桌面操作暫停。")
                self._emit(self.on_error, self.error)
        except Exception as exc:
            self._fail(f"桌面操作失敗：{exc}")
        finally:
            self._polling = False
        # Completion callbacks can start a new operation.  Its own source must
        # not accidentally keep this operation's old GLib timer alive as well.
        return self._operation is operation

    def _focused_window(self, operation):
        return next((window for window in self.screen.get_windows()
                     if window.get_xid() == operation.focused_xid), None)

    def _complete(self, operation):
        result = WorkspaceResult(operation.action, self.screen.get_workspace_count(),
                                 operation.target, operation.closed_index)
        self.error = None
        self._finish()
        self._emit(self.on_complete, result)

    def _finish(self):
        source = self._timer
        self._timer = None
        self._operation = None
        if source is not None and not self._polling:
            self.glib.source_remove(source)
        self._emit(self.on_busy, False)

    def _reject(self, message):
        self.error = message
        self._emit(self.on_error, message)
        return False

    def _fail(self, message):
        self.error = message
        if self.busy:
            self._finish()
        self._emit(self.on_error, message)
        return False

    @staticmethod
    def _emit(callback, value):
        if callback is not None:
            try:
                callback(value)
            except Exception:
                logging.exception("Workspace lifecycle callback failed")

    def dispose(self):
        """Stop readback when the shell exits; never alter desktop state."""
        if self.busy:
            self._finish()
