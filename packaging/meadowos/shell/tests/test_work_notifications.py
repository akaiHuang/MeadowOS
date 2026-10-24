import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from work_notifications import WorkNotifications


def save(path, data):
    path.write_text(json.dumps(data))


def snapshot(status, *, kind='queued_prompt', sid='chat', task='task'):
    return {'id': 3, 'type': 'background_tasks', 'data': {'sessionId': sid,
        'items': [{'id': task, 'status': status, 'kind': kind}]}}


class WorkNotificationTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / 'push.json'
        self.gate = WorkNotifications(self.path, 'chat', save)

    def test_queue_completion_and_user_cancel_never_push(self):
        self.gate.consume(snapshot('running'))
        self.assertEqual(self.gate.consume(snapshot('completed')), [])
        self.gate.consume(snapshot('running', task='cancel'))
        self.assertEqual(self.gate.consume(snapshot('cancelled', task='cancel')), [])

    def test_old_failure_is_visible_once_without_a_historical_push(self):
        notices = self.gate.consume(snapshot('loop_detected'))
        self.assertEqual(notices[0]['kind'], 'turn_failed')
        self.assertFalse(notices[0]['push'])
        self.assertIn('尚未完成', notices[0]['text'])
        restored = WorkNotifications(self.path, 'chat', save)
        self.assertEqual(restored.consume(snapshot('loop_detected')), [])

    def test_snapshot_and_terminal_event_deduplicate_the_same_failed_task(self):
        self.gate.consume(snapshot('running'))
        first = self.gate.consume(snapshot('loop_detected'))
        self.assertTrue(first[0]['push'])
        end = {'type': 'turn_completed', 'data': {'sessionId': 'chat', 'taskId': 'task',
               'runId': 'run', 'stopReason': 'loop_detected'}}
        self.assertEqual(self.gate.consume(end, had_reply=True), [])

    def test_new_background_completion_survives_reconnect_without_replay(self):
        self.gate.consume(snapshot('running', kind='process'))
        restored = WorkNotifications(self.path, 'chat', save)
        notice = restored.consume(snapshot('completed', kind='process'))[0]
        self.assertEqual(notice['kind'], 'task_completed')
        self.assertTrue(notice['push'])
        self.assertEqual(restored.consume(snapshot('completed', kind='process')), [])

    def test_a_spoken_promise_does_not_turn_loop_failure_into_success(self):
        event = {'type': 'turn_completed', 'data': {'sessionId': 'chat', 'taskId': 'task',
                 'stopReason': 'loop_detected'}}
        notice = self.gate.consume(event, had_reply=True)[0]
        self.assertEqual(notice['kind'], 'turn_failed')

    def test_observed_reply_notifies_only_when_outside_chat(self):
        event = {'type': 'turn_completed', 'data': {'sessionId': 'chat', 'taskId': 'task',
                 'stopReason': 'end_turn'}}
        self.assertEqual(self.gate.consume(event, had_reply=True, foreground=True), [])
        self.assertEqual(self.gate.consume(event, had_reply=True), [])
        event['data']['taskId'] = 'next'
        self.assertTrue(self.gate.consume(event, had_reply=True)[0]['push'])
        event['data']['taskId'] = 'empty'
        self.assertEqual(self.gate.consume(event, had_reply=False), [])

    def test_hydrated_terminal_snapshot_cannot_swallow_fresh_terminal_push(self):
        old = self.gate.consume(snapshot('loop_detected'), historical=True)
        self.assertTrue(old[0]['display'])
        event = {'id': 9, 'type': 'turn_completed', 'data': {'sessionId': 'chat',
            'taskId': 'task', 'stopReason': 'loop_detected'}}
        notice = self.gate.consume(event)[0]
        self.assertTrue(notice['push'])
        self.assertFalse(notice['display'])
        self.assertEqual(self.gate.consume(event), [])

    def test_resume_watermark_does_not_push_replayed_old_transitions_or_replies(self):
        gate = WorkNotifications(self.path, 'chat', save, replay_through=18)
        gate.consume(snapshot('running'))
        self.assertFalse(gate.consume(snapshot('loop_detected'))[0]['push'])
        end = {'id': 18, 'type': 'turn_completed', 'data': {'sessionId': 'chat',
            'taskId': 'task', 'stopReason': 'loop_detected'}}
        self.assertEqual(gate.consume(end), [])
        end['data']['stopReason'] = 'end_turn'
        self.assertEqual(gate.consume(end, had_reply=True), [])
        end['id'] = 19
        self.assertTrue(gate.consume(end, had_reply=True)[0]['push'])

    def test_initial_tasks_hydration_never_pushes_a_cached_running_transition(self):
        self.gate.consume(snapshot('running', kind='process'))
        restored = WorkNotifications(self.path, 'chat', save)
        self.assertEqual(restored.consume(snapshot('completed', kind='process'), historical=True), [])

    def test_failed_snapshot_matches_error_terminal_event(self):
        self.gate.consume(snapshot('running'))
        notice = self.gate.consume(snapshot('failed'))[0]
        self.assertTrue(notice['push'])
        self.assertIn('尚未完成', notice['text'])
        event = {'id': 4, 'type': 'turn_completed', 'data': {'sessionId': 'chat',
            'taskId': 'task', 'stopReason': 'error'}}
        self.assertEqual(self.gate.consume(event), [])

    def test_unrelated_session_cannot_notify_or_change_the_current_ledger(self):
        self.assertEqual(self.gate.consume(snapshot('loop_detected', sid='other')), [])
        self.assertEqual(self.gate.tasks, {})


if __name__ == '__main__':
    unittest.main()
