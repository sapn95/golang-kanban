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

  function refreshCounts() {
    document.querySelectorAll('[data-column]').forEach(function (col) {
      var badge = document.querySelector('[data-count-for="' + col.dataset.column + '"]');
      if (!badge) { return; }
      var n = col.querySelectorAll('[data-id]').length;
      var limit = parseInt(badge.dataset.limit || '0', 10);
      badge.textContent = limit > 0 ? n + ' / ' + limit : String(n);
      badge.classList.toggle('text-red-500', limit > 0 && n > limit);
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
    if (target.id.indexOf('card-') === 0) {
      hideModal('editCardModal');
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
