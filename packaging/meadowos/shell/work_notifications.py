"""Present observed lifecycle facts. Never schedule, retry or infer a task result."""
from __future__ import annotations

import json
from pathlib import Path

ACTIVE = frozenset({'queued', 'guiding', 'running', 'cancelling'})
FAILURE_TEXT = {
    'failed': '工作發生錯誤，尚未完成。',
    'error': '工作發生錯誤，尚未完成。',
    'incomplete': '工作尚未完成，請查看對話中的說明。',
    'max_iterations': '工作已達本輪操作上限，尚未完成。',
    'loop_detected': '工作因重複操作而停止，尚未完成。',
    'interrupted': '工作已中斷，尚未確認結果。',
    'time_budget': '本輪已達時間上限，工作尚未完成。',
    'token_budget': '本輪已達處理用量上限，工作尚未完成。',
    'token_limit': '本輪回覆已達長度上限，工作尚未完成。',
    'empty': '工作已結束，但尚未收到模型回覆或交付結果。',
    'audit_inconclusive': '工作結果尚未完成驗證，請查看對話中的說明。',
}
FAILED = frozenset(FAILURE_TEXT)
REPLIED = frozenset({'end_turn', 'completed', 'needs_input'})

PROGRESS_STATUS = {'in_progress': '執行中', 'completed': '已回傳', 'failed': '未完成',
                   'waiting': '等待你的確認', 'unknown': '結果尚未確認', 'cancelled': '已取消',
                   'observed': '已收到', 'pending': '等待接續', 'blocked': '暫時無法接續'}


def wake_snapshot_events(session):
    """Turn the same-chat authoritative resume metadata into historical facts."""
    rows = session.get('backgroundWakes')
    if not isinstance(rows, list):
        return []
    events = []
    # Core sends newest-first. Restore oldest-first so the bounded ledger
    # retains the newest delivery facts when a snapshot exceeds its capacity.
    for row in reversed(rows[:64]):
        if not isinstance(row, dict) or row.get('actor') != 'runtime' or not isinstance(row.get('wakeId'), str) or not row['wakeId']:
            continue
        if any(row.get(key) and session.get(key) and row[key] != session[key] for key in ('sessionId', 'chatId')):
            continue
        data = {key: row[key] for key in ('sessionId', 'chatId', 'wakeId', 'source', 'status', 'repairRequired', 'actor') if key in row}
        events.append({'id': 0, 'type': 'background_wake_status', 'data': data})
    return events


class ToolProgress:
    """Small durable presentation ledger; never runs or infers any operation."""
    def __init__(self, path, chat_id, persist, *, running=False, run_id=''):
        self.path, self.chat_id, self.persist = path, chat_id, persist
        self.rows, self.run_id, self.last_event_id = [], '', 0
        self.running, self.connected, self.terminal = bool(running), True, ''
        self.background = False
        try:
            saved = json.loads(path.read_text()) if path.exists() and path.stat().st_size <= 128 * 1024 else {}
            if saved.get('version') == 1 and saved.get('chatId') == chat_id:
                self.last_event_id = saved.get('lastEventId', 0) if type(saved.get('lastEventId')) is int else 0
                self.run_id = str(saved.get('runId') or '')
                for item in saved.get('rows', [])[-40:]:
                    if isinstance(item, dict) and all(isinstance(item.get(key), str) for key in ('id', 'title', 'status', 'runId')) and item['status'] in PROGRESS_STATUS:
                        self.rows.append({key: item[key] for key in ('id', 'title', 'status', 'runId')})
                # Reconnection knows whether the run is active, but cannot
                # infer that a particular tool continued during a transport gap.
                for item in self.rows:
                    if item['status'] in {'in_progress', 'waiting', 'pending'}:
                        item['status'] = 'unknown'
        except (OSError, ValueError, TypeError, AttributeError):
            pass
        self.run_id = str(run_id or self.run_id)

    def save(self):
        try:
            self.persist(self.path, {'version': 1, 'chatId': self.chat_id, 'runId': self.run_id,
                                    'lastEventId': self.last_event_id, 'rows': self.rows[-40:]})
        except OSError:
            pass

    def set_disconnected(self):
        self.connected = False
        for row in self.rows:
            if row['status'] in {'in_progress', 'waiting', 'pending'}:
                row['status'] = 'unknown'

    def put(self, key, title, status, run_id=''):
        existing = next((item for item in self.rows if item['id'] == key), None)
        if existing is None:
            existing = {'id': key, 'title': str(title)[:120], 'status': status, 'runId': run_id}
            self.rows.append(existing)
        else:
            # Resume replays current-run starts. A durable terminal result
            # must never regress to a running badge while replay catches up.
            if existing['status'] in {'completed', 'failed', 'cancelled'} and status in {'in_progress', 'pending', 'observed', 'blocked'}:
                return
            existing.update(title=str(title or existing['title'])[:120], status=status)
        self.rows = self.rows[-40:]

    def consume(self, event):
        data = event.get('data') or {}
        kind, event_id = event.get('type'), event.get('id')
        # Resume replays a running turn after its user echo. Progress consumes
        # that replay by stable run/call identity; notification dedupe is separate.
        update = data.get('update') or {}
        before = (self.run_id, self.running, self.terminal, self.background,
                  [(row['id'], row['title'], row['status'], row['runId']) for row in self.rows])
        changed = True
        if kind == 'run_state':
            incoming = str(data.get('runId') or self.run_id)
            if incoming != self.run_id:
                self.rows = [row for row in self.rows if row['status'] in {'failed', 'unknown'} or row['id'].startswith('wake:')]
                self.run_id, self.terminal = incoming, ''
            self.running = data.get('status') in {'running', 'cancelling'}
        elif kind == 'session_update' and update.get('sessionUpdate') in {'tool_call', 'tool_call_update'}:
            call_id, status = update.get('toolCallId'), update.get('status')
            if not isinstance(call_id, str) or status not in {'in_progress', 'completed', 'failed'}:
                return False
            run_id = str(update.get('runId') or self.run_id)
            self.put('tool:' + run_id + ':' + call_id, update.get('title') or update.get('kind') or '工具', status, run_id)
        elif kind in {'permission_request', 'user_input_request'}:
            request_id = data.get('requestId')
            if not isinstance(request_id, str):
                return False
            title = '操作許可' if kind == 'permission_request' else '需要你的回答'
            self.put('request:' + request_id, title, 'waiting', self.run_id)
        elif kind in {'permission_cancelled', 'user_input_cancelled', 'permission_resolved', 'user_input_resolved'}:
            key = 'request:' + str(data.get('requestId'))
            self.rows = [row for row in self.rows if row['id'] != key]
        elif kind == 'turn_completed':
            self.running = False
            reason = data.get('stopReason')
            self.terminal = FAILURE_TEXT.get(reason, {
                'cancelled': '本輪已取消', 'needs_input': '等待你的回答',
                'end_turn': '本輪回覆已結束', 'completed': '本輪回覆已結束',
            }.get(reason, '本輪已停止 · 結果尚未確認'))
            for row in self.rows:
                if row['status'] in {'in_progress', 'waiting'} and not row['id'].startswith('wake:'):
                    row['status'] = 'unknown'
            self.background = False
        elif kind in {'background_wake', 'background_wake_blocked'}:
            blocked = kind == 'background_wake_blocked'
            # New runtimes link the source receipt to the durable delivery. A
            # legacy receipt is summary-only: its ID is not a delivery ID.
            identity = data.get('wakeId')
            self.background = not blocked and not identity
            if identity:
                key = 'wake:' + str(identity)
                if blocked or not any(row['id'] == key for row in self.rows):
                    self.put(key, '背景結果接續處理', 'blocked' if blocked else 'observed', self.run_id)
            elif blocked:
                self.put('wake-blocked:' + str(data.get('notificationId') or event_id), '背景接續未啟動', 'failed', self.run_id)
        elif kind == 'background_wake_status' and data.get('actor') == 'runtime':
            status = data.get('status')
            mapped = {'pending': 'pending', 'accepted': 'pending', 'running': 'in_progress',
                      'completed': 'completed', 'cancelled': 'cancelled', 'interrupted': 'unknown'}
            if status not in mapped or not data.get('wakeId'):
                return False
            self.background = False
            self.put('wake:' + str(data['wakeId']), '背景結果接續處理', mapped[status], self.run_id)
        else:
            changed = False
        after = (self.run_id, self.running, self.terminal, self.background,
                 [(row['id'], row['title'], row['status'], row['runId']) for row in self.rows])
        changed = changed and after != before
        if changed:
            if type(event_id) is int:
                self.last_event_id = max(event_id, self.last_event_id)
            self.save()
        return changed

    def view(self):
        failures = [row for row in self.rows if row['status'] == 'failed']
        current_failures = [row for row in failures if row['runId'] == self.run_id]
        waiting = [row for row in self.rows if row['status'] == 'waiting']
        active = [row for row in self.rows if row['status'] == 'in_progress']
        wake_active = [row for row in active if row['id'].startswith('wake:')]
        wake_pending = [row for row in self.rows if row['id'].startswith('wake:') and row['status'] == 'pending']
        wake_blocked = [row for row in self.rows if row['id'].startswith('wake:') and row['status'] == 'blocked']
        if not self.connected:
            summary = '連線中斷 · 執行狀態尚未確認'
        elif waiting:
            summary = '等待你的確認' if waiting[-1]['title'] == '操作許可' else '等待你的回答'
        elif active and (self.running or wake_active):
            summary = active[-1]['title'] + ' · 執行中'
        elif self.running:
            summary = '正在接續背景結果' if self.background else 'Meadow 正在處理'
        elif wake_pending:
            summary = '背景結果已收到 · 等待接續處理'
        elif wake_blocked:
            summary = '背景結果已收到 · 暫時無法接續'
        elif self.background:
            summary = '背景結果已收到 · 等待接續處理'
        elif self.terminal:
            summary = self.terminal
        else:
            summary = '執行紀錄'
        if current_failures and '未完成' not in summary:
            summary += f' · {len(current_failures)} 項操作未完成'
        # The bounded ledger is scrollable in full so retained failures remain
        # inspectable. Live rows come first, followed by recent history.
        live = [row for row in reversed(self.rows) if row['status'] in {'in_progress', 'waiting', 'pending'}]
        selected = live + [row for row in reversed(self.rows) if row not in live]
        return {'summary': summary, 'active': bool(self.connected and (self.running or waiting or wake_active)),
                'visible': bool(self.rows or self.running or self.terminal or self.background),
                'rows': [{**row, 'previous': row['runId'] != self.run_id and not row['id'].startswith('wake:'),
                          'statusLabel': PROGRESS_STATUS[row['status']]} for row in selected]}


class WorkNotifications:
    def __init__(self, path: Path, chat_id: str, persist, *, replay_through=0):
        self.path, self.chat_id, self.persist = path, chat_id, persist
        self.replay_through = replay_through if type(replay_through) is int else 0
        self.tasks, self.seen, self.delivered = {}, [], []
        try:
            if path.exists() and path.stat().st_size <= 128 * 1024:
                data = json.loads(path.read_text())
                if data.get('version') in (1, 2) and data.get('chatId') == chat_id:
                    self.tasks = {str(k): str(v) for k, v in data.get('tasks', {}).items()}
                    self.seen = [str(key) for key in data.get('seen', [])][-160:]
                    self.delivered = [str(key) for key in data.get('delivered', [])][-160:]
        except (OSError, ValueError, AttributeError, TypeError):
            pass

    def save(self):
        try:
            self.persist(self.path, {'version': 2, 'chatId': self.chat_id,
                                    'tasks': self.tasks, 'seen': self.seen[-160:],
                                    'delivered': self.delivered[-160:]})
        except OSError:
            # A cache failure does not change Agent history or task outcome.
            pass

    def once(self, key, *, delivery=False):
        ledger = self.delivered if delivery else self.seen
        if key in ledger:
            return False
        ledger.append(key)
        del ledger[:-160]
        self.save()
        return True

    def notice(self, key, kind, text, *, fresh=False, foreground=False, display=True):
        # Initial snapshots may make a failure visible before its fresh SSE
        # terminal event arrives. Presentation and push therefore need separate
        # receipts. Foreground delivery is consumed too, preventing a later
        # reconnect from turning an already-seen live reply into a push.
        visible = self.once(key) if display else False
        push = fresh and self.once(key, delivery=True) and not foreground
        if visible or push:
            return [{'key': key, 'kind': kind, 'text': text,
                     'display': visible, 'push': push}]
        return []

    def consume(self, event, *, had_reply=False, foreground=False, historical=False):
        data = event.get('data') or {}
        sid = data.get('sessionId') or data.get('chatId')
        if sid and sid != self.chat_id:
            return []
        event_id = event.get('id')
        historical = historical or (type(event_id) is int and event_id <= self.replay_through)
        notices = []
        kind = event.get('type')
        if kind == 'background_tasks':
            rows = [row for row in data.get('items', []) if isinstance(row, dict)]
            previous = self.tasks
            self.tasks = {str(row['id']): str(row.get('status', '')) for row in rows[-120:] if row.get('id')}
            for row in rows:
                task_id, status = str(row.get('id', '')), row.get('status')
                if not task_id:
                    continue
                failed = status in FAILED
                success = status == 'completed' and row.get('kind') != 'queued_prompt'
                if not (failed or success):
                    continue
                transition = previous.get(task_id) in ACTIVE
                canonical = 'error' if status == 'failed' else str(status)
                key = 'task:' + task_id + ':' + canonical
                if failed:
                    notices += self.notice(key, 'turn_failed', FAILURE_TEXT[status],
                        fresh=transition and not historical, foreground=foreground)
                elif transition and not historical:
                    notices += self.notice(key, 'task_completed', '背景工作已完成，請查看對話中的結果。',
                        fresh=True, foreground=foreground)
            self.save()
        elif kind == 'turn_completed':
            reason = data.get('stopReason')
            key = str(data.get('taskId') or data.get('runId') or event.get('id'))
            if reason in FAILED:
                canonical = 'error' if reason == 'failed' else str(reason)
                notices += self.notice('task:' + key + ':' + canonical, 'turn_failed', FAILURE_TEXT[reason],
                    fresh=not historical, foreground=foreground)
            elif reason in REPLIED and had_reply and not historical:
                notices += self.notice('reply:' + key, 'reply', 'Meadow 有新回覆，點此開啟聊天。',
                    fresh=True, foreground=foreground, display=False)
        elif kind == 'work_center_error':
            notices += self.notice('work-center:' + str(event.get('id')), 'turn_failed',
                '背景工作狀態暫時無法同步，結果尚未確認。',
                fresh=not historical, foreground=foreground)
        elif kind == 'background_wake_blocked':
            identity = str(data.get('notificationId') or event.get('id'))
            notices += self.notice('wake-blocked:' + str(data.get('source') or '') + ':' + identity,
                'turn_failed', '背景結果已收到，但接續處理尚未啟動。請查看執行進度。',
                fresh=not historical, foreground=foreground)
        elif kind == 'background_wake_status' and data.get('actor') == 'runtime' and data.get('status') == 'interrupted':
            notices += self.notice('wake-interrupted:' + str(data.get('wakeId') or event.get('id')),
                'turn_failed', '背景接續已中斷，結果尚未確認。',
                fresh=not historical, foreground=foreground)
        return notices
