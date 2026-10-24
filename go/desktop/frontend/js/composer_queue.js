// Inline editor for server-owned pending prompts. No separate scheduler,
// semantic routing, automatic guidance, or success notifications live here.
const queueIcon = (paths) => {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.6');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  for (const d of paths) {
    const path = document.createElementNS(svg.namespaceURI, 'path');
    path.setAttribute('d', d);
    svg.append(path);
  }
  return svg;
};

export function renderComposerQueue(root, items, busyIDs, onAction, paused = false) {
  if (!root) return;
  root.cancelQueueDrag?.();
  if (!root.dataset.initialized) {
    root.dataset.initialized = 'true';
    const closeMenus = () =>
      root
        .querySelectorAll('details[open]')
        .forEach((details) => details.removeAttribute('open'));
    root.addEventListener('scroll', closeMenus);
    document.addEventListener('click', (event) => {
      if (!event.target.closest('#taskQueue .queue-more')) closeMenus();
    });
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') closeMenus();
    });
  }
  const pending = items.filter((item) => item.status === 'pending');
  const openID = root.querySelector('details[open]')?.closest('.queue-row')
    ?.dataset.queueId;
  root.replaceChildren();
  root.hidden = pending.length === 0;
  if (paused && pending.length) {
    const notice = document.createElement('div');
    notice.className = 'queue-paused-notice';
    notice.setAttribute('role', 'status');
    notice.textContent = '佇列已暫停。送出新訊息後才會恢復；可先編輯或刪除待辦。';
    root.appendChild(notice);
  }
  pending.forEach((item, index) => {
    const row = document.createElement('div');
    row.className = 'queue-row';
    row.dataset.queueId = item.id;
    const grip = document.createElement('button');
    grip.type = 'button';
    grip.className = 'queue-grip';
    grip.setAttribute('aria-label', '拖拉排序');
    grip.title = '拖拉排序；也可用上下方向鍵移動';
    grip.disabled = busyIDs.size > 0 || pending.length < 2;
    grip.append(queueIcon(['M9 5h.01M15 5h.01M9 12h.01M15 12h.01M9 19h.01M15 19h.01']));
    grip.addEventListener('keydown', (event) => {
      if (!['ArrowUp', 'ArrowDown'].includes(event.key)) return;
      event.preventDefault();
      if ((event.key === 'ArrowUp' && index === 0) ||
          (event.key === 'ArrowDown' && index === pending.length - 1)) return;
      Promise.resolve(onAction(event.key === 'ArrowUp' ? 'up' : 'down', item.id)).then(() => {
        // Rendering the receipt replaces this handle. Restore keyboard focus
        // only if it fell back to body, never over a newer user focus choice.
        if (document.activeElement !== document.body) return;
        [...root.querySelectorAll('.queue-row')]
          .find((node) => node.dataset.queueId === item.id)
          ?.querySelector('.queue-grip')?.focus();
      });
    });
    grip.addEventListener('pointerdown', (event) => {
      if (event.button !== 0 || grip.disabled) return;
      event.preventDefault();
      const pointerId = event.pointerId;
      const startY = event.clientY;
      const expectedOrder = pending.map((entry) => entry.id);
      let moved = false;
      let beforeId = null;
      const clearMarks = () => root.querySelectorAll('.queue-row').forEach((node) =>
        node.classList.remove('queue-drop-before', 'queue-drop-after'));
      const cleanup = () => {
        clearMarks();
        row.classList.remove('queue-dragging');
        grip.removeEventListener('pointermove', move);
        grip.removeEventListener('pointerup', drop);
        grip.removeEventListener('pointercancel', cleanup);
        grip.removeEventListener('lostpointercapture', cleanup);
        document.removeEventListener('keydown', escape);
        if (grip.hasPointerCapture(pointerId)) grip.releasePointerCapture(pointerId);
        root.cancelQueueDrag = null;
      };
      const escape = (keyEvent) => {
        if (keyEvent.key === 'Escape') cleanup();
      };
      const move = (pointer) => {
        if (pointer.pointerId !== pointerId) return;
        if (!moved && Math.abs(pointer.clientY - startY) < 4) return;
        moved = true;
        row.classList.add('queue-dragging');
        clearMarks();
        const bounds = root.getBoundingClientRect();
        if (pointer.clientX < bounds.left || pointer.clientX > bounds.right ||
            pointer.clientY < bounds.top - 20 || pointer.clientY > bounds.bottom + 20) {
          beforeId = null;
          return;
        }
        if (pointer.clientY < bounds.top + 18) root.scrollTop -= 12;
        if (pointer.clientY > bounds.bottom - 18) root.scrollTop += 12;
        const others = [...root.querySelectorAll('.queue-row')].filter((node) => node !== row);
        const target = others.find((node) => {
          const rect = node.getBoundingClientRect();
          return pointer.clientY < rect.top + rect.height / 2;
        });
        beforeId = target?.dataset.queueId || '';
        if (target) target.classList.add('queue-drop-before');
        else others.at(-1)?.classList.add('queue-drop-after');
      };
      const drop = (pointer) => {
        if (pointer.pointerId !== pointerId) return;
        const target = beforeId;
        cleanup();
        if (!moved || target === null) return;
        const proposed = expectedOrder.filter((id) => id !== item.id);
        proposed.splice(target ? proposed.indexOf(target) : proposed.length, 0, item.id);
        if (proposed.every((id, i) => id === expectedOrder[i])) return;
        void onAction('reorder', item.id, { beforeId: target, expectedOrder });
      };
      root.cancelQueueDrag = cleanup;
      grip.addEventListener('pointermove', move);
      grip.addEventListener('pointerup', drop);
      grip.addEventListener('pointercancel', cleanup);
      grip.addEventListener('lostpointercapture', cleanup);
      document.addEventListener('keydown', escape);
      grip.setPointerCapture(pointerId);
    });
    row.append(grip);
    const text = document.createElement('span');
    text.className = 'queue-text';
    text.textContent = item.text || '附件訊息';
    text.title = item.text || '附件訊息';
    row.append(text);
    if (item.attachments > 0) {
      const count = document.createElement('span');
      count.className = 'queue-attachments';
      count.textContent = `${item.attachments} 個附件`;
      row.append(count);
    }
    const actions = document.createElement('div');
    actions.className = 'queue-actions';
    const button = (name, label, disabled = false) => {
      const control = document.createElement('button');
      control.type = 'button';
      control.dataset.action = name;
      control.textContent = label;
      control.setAttribute('aria-label', label);
      control.title = label;
      if (name === 'cancel') control.replaceChildren(queueIcon(['M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7M14 10v7']));
      if (name === 'guide') control.prepend(queueIcon(['M4 5v7h15M15 8l4 4-4 4']));
      control.disabled = disabled || busyIDs.size > 0;
      control.addEventListener('click', () => {
        row.querySelector('details')?.removeAttribute('open');
        void onAction(name, item.id);
      });
      return control;
    };
    actions.append(button('guide', '引導'), button('cancel', '刪除'));
    const more = document.createElement('details');
    more.className = 'queue-more';
    more.open = openID === item.id;
    const summary = document.createElement('summary');
    summary.textContent = '⋯';
    summary.setAttribute('aria-label', '更多排隊操作');
    const menu = document.createElement('div');
    menu.className = 'queue-menu';
    menu.append(
      button('edit', '編輯訊息'),
      button('up', '往前移', index === 0),
      button('down', '往後移', index === pending.length - 1),
    );
    more.append(summary, menu);
    more.addEventListener('toggle', () => {
      if (!more.open) return;
      root.querySelectorAll('details[open]').forEach((other) => {
        if (other !== more) other.open = false;
      });
      const rect = summary.getBoundingClientRect();
      menu.style.left = `${Math.max(8, Math.min(rect.right - 148, window.innerWidth - 156))}px`;
      menu.style.top = `${Math.max(8, rect.top - menu.offsetHeight - 5)}px`;
    });
    actions.append(more);
    row.append(actions);
    root.append(row);
  });
}
