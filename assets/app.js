// Board page behaviour: dark mode, modals, drag and drop, subtask editor.
// Depends on htmx and SortableJS, both vendored under /assets/vendor/.
(function () {
  'use strict';

  // --- dark mode -----------------------------------------------------------
  function initDarkMode() {
    var toggle = document.getElementById('darkModeToggle');
    var html = document.documentElement;
    var saved = null;
    try { saved = localStorage.getItem('theme'); } catch (e) { /* private mode */ }
    if (saved === 'dark' || (!saved && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
      html.classList.add('dark');
    }
    if (toggle) {
      toggle.addEventListener('click', function () {
        html.classList.toggle('dark');
        try { localStorage.setItem('theme', html.classList.contains('dark') ? 'dark' : 'light'); } catch (e) { /* ignore */ }
      });
    }
  }

  // --- modals --------------------------------------------------------------
  window.showModal = function (id) { document.getElementById(id).classList.add('show'); };
  window.hideModal = function (id) { document.getElementById(id).classList.remove('show'); };
  document.addEventListener('click', function (e) {
    if (e.target.classList.contains('modal')) { e.target.classList.remove('show'); }
  });

  // --- drag and drop -------------------------------------------------------
  // One request per drop, to the destination column. The server treats the
  // list as the authoritative order of that column and moves a card in from
  // its old column when needed, so the origin needs no request.
  function postOrder(container) {
    var board = document.body.dataset.board;
    var ids = Array.prototype.map.call(container.children, function (el) { return el.dataset.id; })
      .filter(function (id) { return id; });
    fetch('/b/' + board + '/columns/' + container.dataset.column + '/order', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ order: ids })
    }).then(function (r) {
      if (!r.ok) {
        r.text().then(function (msg) { if (msg) { alert(msg); } location.reload(); });
      } else {
        refreshCounts();
      }
    });
  }

  // A drop changes a column's count without the server redrawing the page, so
  // everything the WIP limit shows has to be brought along by hand: the count,
  // its colour, the bar under the header and the line under that. The copy
  // stays in the template — both notices are rendered and one is hidden.
  function refreshCounts() {
    document.querySelectorAll('[data-column]').forEach(function (col) {
      var id = col.dataset.column;
      var badge = document.querySelector('[data-count-for="' + id + '"]');
      if (!badge) { return; }
      var n = col.querySelectorAll('[data-id]').length;
      var limit = parseInt(badge.dataset.limit || '0', 10);
      var over = limit > 0 && n > limit;
      var at = limit > 0 && n === limit;

      badge.textContent = limit > 0 ? n + ' / ' + limit : String(n);
      badge.classList.toggle('text-red-600', over);
      badge.classList.toggle('dark:text-red-400', over);
      badge.classList.toggle('text-amber-600', at);
      badge.classList.toggle('dark:text-amber-400', at);
      badge.classList.toggle('text-gray-500', !over && !at);
      badge.classList.toggle('dark:text-gray-400', !over && !at);

      var bar = document.querySelector('[data-limit-bar-for="' + id + '"]');
      if (bar && limit > 0) {
        var fill = bar.firstElementChild;
        fill.style.width = Math.min(Math.round((n / limit) * 100), 100) + '%';
        fill.classList.toggle('bg-red-500', over);
        fill.classList.toggle('bg-amber-500', at);
        fill.classList.toggle('bg-blue-500', !over && !at);
        bar.title = n + ' of ' + limit + ' allowed in this column';
      }

      document.querySelectorAll('[data-limit-note-for="' + id + '"]').forEach(function (note) {
        var wanted = note.dataset.state === 'over' ? over : at;
        note.classList.toggle('hidden', !wanted);
      });
    });
  }

  function initSortable() {
    if (typeof Sortable === 'undefined') { return; }
    document.querySelectorAll('[data-column]').forEach(function (container) {
      new Sortable(container, {
        group: 'kanban',
        animation: 150,
        // On a touch screen a drag and a scroll begin with the same gesture, so
        // a card has to be held before it starts moving. Without this the board
        // cannot be scrolled on a phone at all: the first touch always picks a
        // card up instead. delayOnTouchOnly keeps the mouse immediate, and the
        // threshold lets a finger wobble during the hold without cancelling it.
        delay: 200,
        delayOnTouchOnly: true,
        touchStartThreshold: 5,
        onEnd: function (evt) { postOrder(evt.to); }
      });
    });
  }

  // --- subtasks ------------------------------------------------------------
  // The form posts one hidden field, "subtasks", in the line format
  // "flag|title" where flag is 1 for done.
  function subtaskRow(text, done) {
    var row = document.createElement('div');
    row.className = 'subtask-row flex items-center space-x-2 mb-2';
    row.innerHTML =
      '<input type="checkbox" class="subtask-complete w-4 h-4 text-blue-600 rounded focus:ring-blue-500">' +
      '<input type="text" class="subtask-text flex-1 px-3 py-2 border border-gray-300 dark:border-gray-600 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-transparent bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm" placeholder="Subtask">' +
      '<button type="button" class="remove-subtask-btn text-red-500 hover:text-red-700 p-1" title="Remove"><i class="bi bi-x text-lg"></i></button>';
    row.querySelector('.subtask-complete').checked = !!done;
    row.querySelector('.subtask-text').value = text || '';
    return row;
  }

  function initSubtasks(form) {
    var container = form.querySelector('.subtasks-container');
    if (!container || container.dataset.ready) { return; }
    container.dataset.ready = '1';
    container.addEventListener('click', function (e) {
      var btn = e.target.closest('.remove-subtask-btn');
      if (btn) { btn.closest('.subtask-row').remove(); }
    });
    form.querySelectorAll('.add-subtask-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var row = subtaskRow('', false);
        container.appendChild(row);
        row.querySelector('.subtask-text').focus();
      });
    });
  }

  window.prepareSubtasks = function (form) {
    var container = form.querySelector('.subtasks-container');
    var hidden = form.querySelector('.subtasks-hidden');
    var lines = [];
    container.querySelectorAll('.subtask-row').forEach(function (row) {
      var text = row.querySelector('.subtask-text').value.trim();
      if (text !== '') {
        lines.push((row.querySelector('.subtask-complete').checked ? '1' : '0') + '|' + text);
      }
    });
    hidden.value = lines.join('\n');
    return true;
  };

  // --- htmx hooks ----------------------------------------------------------
  function initHtmxHooks() {
  document.body.addEventListener('htmx:afterSwap', function (evt) {
    var target = evt.detail.target;
    if (!target || !target.id) { return; }
    if (target.id.indexOf('cards-') === 0) {
      hideModal('addCardModal');
      var form = document.getElementById('addCardForm');
      if (form) {
        form.reset();
        form.querySelector('.subtasks-container').innerHTML = '';
      }
      refreshCounts();
    }
    if (target.id === 'editCardModalContent') {
      showModal('editCardModal');
      initSubtasks(target.querySelector('form'));
    }
    if (target.id.indexOf('comments-') === 0) {
      var empty = target.querySelector('.no-comments');
      if (empty) { empty.remove(); }
      var poster = evt.detail.requestConfig && evt.detail.requestConfig.elt;
      if (poster && poster.tagName === 'FORM') {
        poster.reset();
        var box = poster.querySelector('textarea[name=body]');
        if (box) { box.focus(); }
      }
    }
    if (target.id.indexOf('card-') === 0) {
      // A comment answers with the card face as an out-of-band swap so its
      // badge keeps up. That must not close the modal being typed in.
      var path = (evt.detail.pathInfo && evt.detail.pathInfo.requestPath) || '';
      if (path.indexOf('/comments') === -1) { hideModal('editCardModal'); }
    }
  });
  document.body.addEventListener('htmx:afterRequest', function (evt) {
    if (evt.detail.requestConfig && evt.detail.requestConfig.verb === 'post' &&
        evt.detail.pathInfo && /\/delete$/.test(evt.detail.pathInfo.requestPath)) {
      setTimeout(refreshCounts, 0);
    }
  });
  document.body.addEventListener('htmx:responseError', function (evt) {
    var xhr = evt.detail.xhr;
    alert(xhr && xhr.responseText ? xhr.responseText : 'Request failed');
  });
  }

  function init() {
    initDarkMode();
    initHtmxHooks();
    initSortable();
    var addForm = document.getElementById('addCardForm');
    if (addForm) { initSubtasks(addForm); }
  }

  // The script is loaded in <head>; wait for the body before touching it.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();

// --- multi-select -----------------------------------------------------------
// Selecting several cards and acting on them at once. The selected ids are
// injected into every toolbar form on submit rather than being kept in hidden
// inputs, so the source of truth is the checkboxes on screen and the two
// cannot drift apart.
(function () {
  const bar = document.getElementById('selectionBar');
  if (!bar) return;

  const countEl = document.getElementById('selectionCount');
  // Shift-click extends from the last box clicked, and only within one
  // column: a range across columns has no meaning the user could predict.
  let anchor = null;

  const boxes = () => Array.from(document.querySelectorAll('.card-select'));
  const selected = () => boxes().filter((b) => b.checked);

  function paint() {
    const n = selected().length;
    countEl.textContent = String(n);
    // A class, not the hidden attribute: Tailwind's display utilities beat
    // [hidden] on specificity, so the attribute alone left the bar on screen.
    bar.classList.toggle('hidden', n === 0);
    bar.classList.toggle('flex', n > 0);
    boxes().forEach((b) => {
      const card = b.closest('[data-id]');
      if (!card) return;
      card.classList.toggle('ring-2', b.checked);
      card.classList.toggle('ring-blue-500', b.checked);
    });
  }

  function onClick(e) {
    const box = e.target.closest('.card-select');
    if (!box) return;
    if (e.shiftKey && anchor && anchor !== box) {
      const column = box.closest('[data-column]');
      if (column && anchor.closest('[data-column]') === column) {
        const inColumn = Array.from(column.querySelectorAll('.card-select'));
        const [from, to] = [inColumn.indexOf(anchor), inColumn.indexOf(box)].sort((x, y) => x - y);
        inColumn.slice(from, to + 1).forEach((b) => { b.checked = box.checked; });
      }
    }
    anchor = box;
    paint();
  }

  // Delegated, because cards are replaced by htmx swaps and a listener bound
  // to a card would go with it.
  document.addEventListener('click', onClick);

  document.getElementById('clearSelection')?.addEventListener('click', () => {
    boxes().forEach((b) => { b.checked = false; });
    anchor = null;
    paint();
  });

  // A card the user is dragging should not stay selected somewhere else.
  document.addEventListener('htmx:afterSwap', paint);

  bar.querySelectorAll('form').forEach((form) => {
    form.addEventListener('htmx:configRequest', (e) => {
      const ids = selected().map((b) => b.value);
      if (!ids.length) return;
      // htmx serialises repeated names from an array value.
      e.detail.parameters.ids = ids;
    });
  });

  paint();
})();
