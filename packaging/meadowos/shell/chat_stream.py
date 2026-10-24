"""Group public Agent deltas by their observed response identity.

This is presentation state, not a planner: no synthetic acknowledgements, text
matching, task classification or private thought conversion. Completed snapshot
rows and the SSE replay cursor are supplied by the authoritative resume response.
"""
from dataclasses import dataclass


@dataclass(frozen=True)
class BubbleUpdate:
    key: tuple
    text: str
    delta: str
    create: bool


def _message_id(value):
    return value if isinstance(value, str) and value else None


class AssistantMessages:
    """Keep one bubble per real response, including a run's replayed deltas."""

    def __init__(self, messages=(), *, replay_after=0):
        # Recorded public responses are complete. Their run-start replay must
        # not append duplicate bubbles or add the same prefix to existing text.
        self.recorded = {
            message["messageId"] for message in messages
            if isinstance(message, dict) and message.get("role") == "assistant"
            and isinstance(message.get("content"), str) and message["content"]
            and _message_id(message.get("messageId"))
        }
        self.closed = set()
        self.texts = {}
        self.active = None
        self.run_id = None
        self.legacy_sequence = 0
        self.last_event_id = replay_after if type(replay_after) is int and replay_after > 0 else 0

    def _close_run(self):
        self.closed.update(key[1] for key in self.texts if key[0] == "message")
        self.texts.clear()  # Completed text is already owned by its GTK label.
        self.active = None

    def consume(self, event):
        """Return a display update only for a new public text chunk.

        start/end events never produce empty messages. IDs are opaque, and a
        late end for another message cannot close the current response. Old
        senders without messageId retain their per-turn stream compatibility.
        """
        event_id = event.get("id")
        if type(event_id) is int and event_id > 0:
            if event_id <= self.last_event_id:
                return None
            self.last_event_id = event_id
        kind, data = event.get("type"), event.get("data") or {}
        if kind == "run_state":
            incoming = _message_id(data.get("runId"))
            if data.get("status") == "running" and incoming and incoming != self.run_id:
                self._close_run()
                self.run_id = incoming
            return None
        if kind == "turn_completed":
            self._close_run()
            return None
        if kind != "session_update":
            return None
        update = data.get("update") or {}
        subtype = update.get("sessionUpdate")
        identity = _message_id(update.get("messageId"))
        key = ("message", identity) if identity else None
        if subtype == "agent_message_start":
            if identity and identity not in self.recorded and identity not in self.closed:
                self.active = key
            return None
        if subtype == "agent_message_end":
            if identity:
                self.closed.add(identity)
                self.texts.pop(key, None)
            if key == self.active or (key is None and self.active and self.active[0] == "legacy"):
                self.active = None
            return None
        if subtype != "agent_message_chunk":
            return None  # Private thoughts, user echoes and tool data are not replies.
        content = update.get("content") or {}
        text = content.get("text") if isinstance(content, dict) else None
        if not isinstance(text, str) or not text:
            return None
        if identity and (identity in self.recorded or identity in self.closed):
            return None
        if key is None:
            key = self.active
            if key is None:
                self.legacy_sequence += 1
                key = ("legacy", self.legacy_sequence)
        if key[0] == "message" and (key[1] in self.recorded or key[1] in self.closed):
            return None
        created = key not in self.texts
        combined = self.texts.get(key, "") + text
        self.texts[key], self.active = combined, key
        return BubbleUpdate(key=key, text=combined, delta=text, create=created)
