"""Native observed tool lifecycle: no invented output, runs, or notifications."""
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from work_notifications import ToolProgress, WorkNotifications, wake_snapshot_events


def tool(call_id, status, *, run='run-a', event_id=10, title='shell'):
    return {'id': event_id, 'type': 'session_update', 'data': {'update': {
        'sessionUpdate': 'tool_call' if status == 'in_progress' else 'tool_call_update',
        'toolCallId': call_id, 'runId': run, 'title': title, 'status': status,
        'rawInput': {'private': 'must not display'}, 'rawOutput': 'must not persist or display'}}}


class ToolProgressTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / 'progress.json'
        self.persist = lambda path, value: path.write_text(json.dumps(value))
        self.progress = ToolProgress(self.path, 'chat', self.persist, running=True, run_id='run-a')

    def test_actual_tool_start_is_visible_and_raw_arguments_never_enter_progress(self):
        self.progress.consume(tool('one', 'in_progress'))
        state = self.progress.view()
        self.assertTrue(state['visible'])
        self.assertEqual(state['summary'], 'shell · 執行中')
        self.assertEqual(state['rows'][0]['statusLabel'], '執行中')
        self.assertNotIn('rawInput', self.path.read_text())
        self.assertNotIn('must not', json.dumps(state))

    def test_parallel_identical_tools_and_completion_without_start(self):
        self.progress.consume(tool('one', 'in_progress'))
        self.progress.consume(tool('two', 'in_progress', event_id=11))
        self.progress.consume(tool('one', 'completed', event_id=12))
        self.progress.consume(tool('three', 'failed', event_id=13))
        self.assertEqual(self.progress.view()['rows'][0]['status'], 'in_progress')
        self.assertEqual(sorted(row['status'] for row in self.progress.view()['rows']), ['completed', 'failed', 'in_progress'])

    def test_failed_operation_survives_completion_and_is_marked_previous_on_new_run(self):
        self.progress.consume(tool('one', 'failed'))
        self.progress.consume({'id': 11, 'type': 'turn_completed', 'data': {'stopReason': 'end_turn'}})
        self.assertEqual(self.progress.view()['rows'][0]['statusLabel'], '未完成')
        self.progress.consume({'id': 12, 'type': 'run_state', 'data': {'status': 'running', 'runId': 'run-b'}})
        state = self.progress.view()
        self.assertTrue(state['rows'][0]['previous'])
        self.assertNotIn('未完成', state['summary'])

    def test_reconnect_marks_uncertain_then_replays_current_run_without_duplicates(self):
        self.progress.consume(tool('one', 'in_progress'))
        self.progress.consume(tool('two', 'failed', event_id=12))
        restored = ToolProgress(self.path, 'chat', self.persist, running=True, run_id='run-a')
        self.assertEqual(restored.rows[0]['status'], 'unknown')
        restored.consume(tool('one', 'in_progress'))
        restored.consume(tool('two', 'in_progress', event_id=11))
        restored.consume(tool('two', 'failed', event_id=12))
        self.assertEqual(len(restored.rows), 2)
        self.assertEqual([row['status'] for row in restored.rows], ['in_progress', 'failed'])
        restored.set_disconnected()
        self.assertEqual(next(row for row in restored.view()['rows'] if row['id'].endswith(':one'))['statusLabel'], '結果尚未確認')
        self.assertFalse(restored.view()['active'])

    def test_permission_is_separate_from_parallel_tool_and_resolution_restores_progress(self):
        self.progress.consume(tool('one', 'in_progress'))
        self.progress.consume({'id': 11, 'type': 'permission_request', 'data': {'requestId': 'permission'}})
        self.assertEqual(self.progress.view()['summary'], '等待你的確認')
        self.assertEqual(len(self.progress.rows), 2)
        self.progress.consume({'id': 12, 'type': 'permission_resolved', 'data': {'requestId': 'permission'}})
        self.assertEqual(self.progress.view()['summary'], 'shell · 執行中')

    def test_argument_drafts_are_not_execution_and_terminal_never_implies_missing_tool_succeeded(self):
        self.assertFalse(self.progress.consume({'id': 1, 'type': 'session_update', 'data': {'update': {
            'sessionUpdate': 'tool_call_draft', 'title': 'shell', 'argumentsDelta': 'private'}}}))
        self.assertEqual(self.progress.rows, [])
        self.progress.consume(tool('one', 'in_progress'))
        self.progress.consume({'id': 11, 'type': 'turn_completed', 'data': {'stopReason': 'loop_detected'}})
        self.assertEqual(self.progress.rows[0]['status'], 'unknown')
        self.assertIn('尚未完成', self.progress.view()['summary'])

    def test_runtime_wake_is_observation_not_verified_completion_or_human_input(self):
        self.progress.consume({'id': 1, 'type': 'background_wake', 'data': {'notificationId': 'p1', 'source': 'process'}})
        self.assertEqual(self.progress.rows, [])  # Legacy receipt has no delivery identity.
        for state in ('pending', 'running', 'interrupted'):
            self.progress.consume({'id': 2, 'type': 'background_wake_status', 'data': {
                'actor': 'runtime', 'wakeId': 'wake', 'status': state}})
        self.assertEqual(self.progress.rows[-1]['status'], 'unknown')

    def test_current_tool_remains_visible_after_many_previous_failures(self):
        for index in range(20):
            self.progress.consume(tool(str(index), 'failed', event_id=index + 1))
        self.progress.consume({'id': 22, 'type': 'run_state', 'data': {'status': 'running', 'runId': 'run-b'}})
        self.progress.consume(tool('current', 'in_progress', run='run-b', event_id=23, title='current shell'))
        state = self.progress.view()
        self.assertEqual(state['summary'], 'current shell · 執行中')
        self.assertEqual(len(state['rows']), 21)
        self.assertEqual(state['rows'][0]['title'], 'current shell')
        self.assertTrue(any(row['previous'] and row['status'] == 'failed' for row in state['rows']))

    def test_budget_and_unverified_terminal_states_do_not_look_complete(self):
        for reason in ('time_budget', 'token_budget', 'token_limit', 'empty', 'audit_inconclusive'):
            with self.subTest(reason=reason):
                self.progress.consume({'type': 'turn_completed', 'data': {'stopReason': reason}})
                self.assertNotEqual(self.progress.view()['summary'], '本輪回覆已結束')
        self.progress.consume({'type': 'turn_completed', 'data': {'stopReason': 'needs_input'}})
        self.assertEqual(self.progress.view()['summary'], '等待你的回答')

    def test_wake_delivery_links_one_row_and_survives_foreground_run_boundary(self):
        self.progress.consume({'id': 1, 'type': 'background_wake_status', 'data': {
            'actor': 'runtime', 'wakeId': 'wake', 'status': 'pending'}})
        self.progress.consume({'id': 2, 'type': 'background_wake', 'data': {'wakeId': 'wake', 'notificationId': 'p1'}})
        self.assertEqual(len(self.progress.rows), 1)
        self.assertEqual(self.progress.rows[0]['status'], 'pending')
        self.progress.consume({'id': 3, 'type': 'run_state', 'data': {'status': 'running', 'runId': 'run-b'}})
        self.assertEqual(len(self.progress.rows), 1)
        self.progress.consume({'id': 4, 'type': 'background_wake_status', 'data': {
            'actor': 'runtime', 'wakeId': 'wake', 'status': 'running'}})
        self.progress.consume({'id': 5, 'type': 'turn_completed', 'data': {'stopReason': 'end_turn'}})
        self.assertIn('執行中', self.progress.view()['summary'])
        self.progress.consume({'id': 6, 'type': 'background_wake_status', 'data': {
            'actor': 'runtime', 'wakeId': 'wake', 'status': 'completed'}})
        self.assertFalse(self.progress.view()['active'])
        self.assertEqual(self.progress.rows[0]['status'], 'completed')

    def test_identical_event_replay_never_rewrites_cache_or_rebuilds_ui(self):
        writes = []
        self.progress.persist = lambda path, value: writes.append(value)
        event = tool('one', 'in_progress')
        self.assertTrue(self.progress.consume(event))
        self.assertFalse(self.progress.consume(event))
        self.assertEqual(len(writes), 1)

    def test_blocked_wake_merges_one_row_and_actual_retry_can_continue(self):
        self.progress.consume({'id': 1, 'type': 'background_wake_status', 'data': {
            'actor': 'runtime', 'wakeId': 'wake', 'status': 'pending'}})
        event = {'id': 2, 'type': 'background_wake_blocked', 'data': {
            'wakeId': 'wake', 'notificationId': 'p1', 'message': 'private admission details'}}
        self.assertTrue(self.progress.consume(event))
        self.assertFalse(self.progress.consume(event))
        self.assertEqual(len(self.progress.rows), 1)
        self.assertEqual(self.progress.rows[0]['status'], 'blocked')
        self.progress.consume({'id': 3, 'type': 'background_wake_status', 'data': {
            'actor': 'runtime', 'wakeId': 'wake', 'status': 'running'}})
        self.assertEqual(self.progress.rows[0]['status'], 'in_progress')
        self.assertNotIn('private', self.path.read_text())
        self.progress.consume({'id': 4, 'type': 'background_wake_status', 'data': {
            'actor': 'runtime', 'wakeId': 'wake', 'status': 'completed'}})
        self.assertFalse(self.progress.consume(event))
        self.assertEqual(self.progress.rows[0]['status'], 'completed')

    def test_replayed_wake_failure_does_not_push_again(self):
        path = Path(self.tmp.name) / 'notice.json'
        gate = WorkNotifications(path, 'session', self.persist, replay_through=10)
        event = {'id': 5, 'type': 'background_wake_blocked', 'data': {'sessionId': 'session', 'source': 'process', 'notificationId': 'p1'}}
        self.assertFalse(gate.consume(event)[0]['push'])
        event['id'] = 11
        self.assertTrue(gate.consume(event)[0]['push'])
        event['id'] = 12
        self.assertEqual(gate.consume(event), [])

    def test_resume_wake_snapshot_restores_repair_state_without_notifications_or_observation(self):
        row = {'sessionId': 'session', 'chatId': 'chat', 'wakeId': 'wake', 'actor': 'runtime',
               'status': 'interrupted', 'repairRequired': True, 'observation': 'private raw result'}
        session = {'sessionId': 'session', 'chatId': 'chat', 'backgroundWakes': [row,
            {**row, 'chatId': 'other'}, {**row, 'actor': 'user'}]}
        events = wake_snapshot_events(session)
        self.assertEqual(len(events), 1)
        gate = WorkNotifications(Path(self.tmp.name) / 'notice.json', 'session', self.persist)
        self.progress.consume(events[0])
        notices = gate.consume(events[0], historical=True)
        self.assertEqual(self.progress.rows[0]['status'], 'unknown')
        self.assertTrue(notices[0]['display'])
        self.assertFalse(notices[0]['push'])
        self.assertEqual(gate.consume(events[0], historical=True), [])
        self.assertNotIn('private', json.dumps(events))
        self.assertNotIn('private', self.path.read_text())

    def test_newest_first_resume_snapshot_preserves_latest_forty_deliveries(self):
        session = {'backgroundWakes': [
            {'actor': 'runtime', 'wakeId': str(index), 'status': 'running' if index == 63 else 'completed'}
            for index in reversed(range(64))]}
        for event in wake_snapshot_events(session):
            self.progress.consume(event)
        self.assertEqual(len(self.progress.rows), 40)
        self.assertEqual(self.progress.rows[0]['id'], 'wake:24')
        self.assertEqual(self.progress.rows[-1]['id'], 'wake:63')
        self.assertEqual(self.progress.view()['rows'][0]['id'], 'wake:63')
        self.assertEqual(self.progress.view()['rows'][0]['status'], 'in_progress')


if __name__ == '__main__':
    unittest.main()
