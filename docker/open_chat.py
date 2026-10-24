#!/usr/bin/env python3
"""Invoke the existing Shell's accessible Chat button; never launch another Shell."""
from collections import deque
import time
import gi

gi.require_version("Atspi", "2.0")
from gi.repository import Atspi


def main():
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        queue = deque([(Atspi.get_desktop(0), 0)])
        visited = 0
        while queue and visited < 6000 and time.monotonic() < deadline:
            node, depth = queue.popleft()
            visited += 1
            try:
                if (node.get_name() == "開啟 Meadow 聊天"
                        and node.get_state_set().contains(Atspi.StateType.SHOWING)):
                    action = node.get_action_iface()
                    if action and action.get_n_actions() and action.do_action(0):
                        return 0
                if depth < 12:
                    for index in range(min(node.get_child_count(), 200)):
                        queue.append((node.get_child_at_index(index), depth + 1))
            except Exception:
                continue
        time.sleep(.2)
    raise RuntimeError("Existing native Chat accessibility button was not discovered")


if __name__ == "__main__":
    raise SystemExit(main())
