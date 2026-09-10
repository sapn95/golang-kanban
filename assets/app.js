// Board page behaviour: dark mode, modals, drag and drop, subtask editor.
// Depends on htmx and SortableJS, both vendored under /assets/vendor/.
(function () {
  'use strict';

  // --- dark mode -----------------------------------------------------------
  // The class itself is set by an inline script in the head, before the first
  // paint. This script is deferred, so doing it here made every navigation
  // flash white. All that is left is the toggle.
  function initDarkMode() {
    var toggle = document.getElementById('darkModeToggle');
    var html = document.documentElement;
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
    // Through htmx rather than fetch, because the answer is the board's column
    // headers marked hx-swap-oob and htmx is what puts them back where they
    // belong. Nothing is swapped into the column that asked.
    htmx.ajax('POST', '/b/' + board + '/columns/' + container.dataset.column + '/order', {
      target: container, swap: 'none', values: { order: ids }
    });
  }

  // Dragging a selection. SortableJS moves the one card that was picked up, so
  // dragging three ticked cards left the other two behind. Its MultiDrag plugin
  // is a second script with a selection model of its own, which would compete
  // with the checkboxes the board already has, so the rest of the selection is
  // moved here instead, after the drop.
  //
  // The set is read from the checkboxes at drop time rather than kept in a
  // variable, for the same reason the toolbar reads them on submit: what is on
  // screen is the only state that cannot drift.
  function dragged(item) {
    var cards = Array.prototype.map.call(document.querySelectorAll('.card-select:checked'), function (box) {
      return box.closest('[data-id]');
    }).filter(function (card) { return card; });
    // Grabbing a card that is not ticked moves that card, whatever else is
    // selected. Anything else would move cards the pointer never touched.
    return cards.indexOf(item) === -1 ? [item] : cards;
  }

  function moveSelection(evt) {
    var after = evt.item;
    dragged(evt.item).forEach(function (card) {
      if (card === evt.item) { return; }
      // In document order, so the cards keep their order among themselves and
      // land together under the one that was dragged.
      after.insertAdjacentElement('afterend', card);
      after = card;
    });
    // One request, to the destination: it is the authoritative order of that
    // column and the server moves each card in from wherever it was, so the
    // columns the cards left need no request of their own.
    postOrder(evt.to);
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
        onEnd: function (evt) { moveSelection(evt); }
      });
    });
  }

  // --- quick edit ----------------------------------------------------------
  // The assignee and label menus on the card face. One open at a time, and a
  // click anywhere that is neither a trigger nor inside an open panel closes
  // them, which is what an outside click has to do for a menu.
  function closePanels(except) {
    document.querySelectorAll('.quick-panel:not(.hidden)').forEach(function (panel) {
      if (panel !== except) { panel.classList.add('hidden'); }
    });
  }

  // Opens one panel and puts the cursor in it. The date field is the only one
  // worth focusing, and showPicker only saves the click that would open the
  // calendar anyway: not every browser has it, and Firefox and Safari open on
  // focus, so a failure here is not one.
  function openPanel(trigger) {
    var panel = document.getElementById(trigger.dataset.panel);
    if (!panel) { return; }
    closePanels(panel);
    panel.classList.remove('hidden');
    var field = panel.querySelector('input[type=date]');
    if (!field) { return; }
    field.focus();
    try { field.showPicker(); } catch (err) { /* the focus is the fallback */ }
  }

  function initQuickEdit() {
    // Delegated: cards are replaced wholesale by htmx swaps, so a listener
    // bound to a trigger would go out with the card it was on.
    document.addEventListener('click', function (e) {
      var trigger = e.target.closest('.quick-toggle');
      if (trigger) {
        var panel = document.getElementById(trigger.dataset.panel);
        closePanels(panel);
        if (panel) { panel.classList.toggle('hidden'); }
        return;
      }
      // Enter or Space on the due date fires a click that counts no clicks,
      // which is the keyboard's way in. Without it the date would open on a
      // double click and so with a mouse only.
      var pressed = e.target.closest('.dbl-toggle');
      if (pressed && e.detail === 0) {
        openPanel(pressed);
        return;
      }
      // A click inside an open panel is somebody using it.
      if (!e.target.closest('.quick-panel')) { closePanels(null); }
    });
    // A double click opens what it landed on: the due date opens a picker, and
    // the card itself opens its edit form, because the pencil is a small target
    // on a phone and the card is what the finger is already on.
    //
    // A button, a link, a field or an open panel is somebody using the card
    // face, so those are left alone. The archive draws its own rows without a
    // data-id and has no edit modal to swap into, which is why this asks for
    // one rather than for any card.
    document.addEventListener('dblclick', function (e) {
      var trigger = e.target.closest('.dbl-toggle');
      if (trigger) {
        openPanel(trigger);
        return;
      }
      var card = e.target.closest('[data-id]');
      if (!card || typeof htmx === 'undefined') { return; }
      if (e.target.closest('button, a, input, select, textarea, label, .quick-panel')) { return; }
      // A double click selects the word under it, and that selection would sit
      // behind the modal until the next click somewhere else.
      var selection = window.getSelection();
      if (selection) { selection.removeAllRanges(); }
      htmx.ajax('GET', '/cards/' + card.dataset.id + '/edit',
        { target: '#editCardModalContent', swap: 'innerHTML' });
    });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { closePanels(null); }
    });
  }

  // --- settings rows -------------------------------------------------------
  // The settings page carries one Save per column and one per label, which on a
  // board with three of each is six blue buttons shouting at once, none of them
  // with anything to save. A row's Save waits until something in that row has
  // been typed or picked.
  //
  // The template renders them visible and this hides them, not the other way
  // round: with no script every button is simply there, which is how the page
  // worked before.
  //
  // Two ways of hiding, because the button sits differently at the two widths.
  // From sm up it is inline with the WIP field or the swatches, so invisible
  // keeps its width and revealing it shifts nothing. On a phone the label rows
  // put it on a line of its own, where reserving the space leaves a visibly
  // empty line in every row, so there it is taken out of the layout instead.
  var IDLE = ['hidden', 'sm:inline-block', 'sm:invisible'];

  function initRowSaves() {
    var saves = document.querySelectorAll('.row-save');
    if (!saves.length) { return; }
    saves.forEach(function (btn) { btn.classList.add.apply(btn.classList, IDLE); });
    // input is the typing, change is the colour swatches and the number
    // stepper. The form the field belongs to is the row, so only that row's
    // Save comes back.
    ['input', 'change'].forEach(function (type) {
      document.addEventListener(type, function (e) {
        var form = e.target.form;
        if (!form) { return; }
        form.querySelectorAll('.row-save').forEach(function (btn) {
          btn.classList.remove.apply(btn.classList, IDLE);
        });
      });
    });
  }

  // --- subtasks ------------------------------------------------------------
  // The form posts one hidden field, "subtasks", in the line format
  // "flag|title" where flag is 1 for done.
  // A checklist row is markup the server owns, so the Add Subtask button is an
  // hx-get and there is nothing here that builds one. What is left is the two
  // things markup cannot say: take a row away, and put the cursor in a row
  // that has just arrived. Both are delegated from the document, so a form
  // htmx swaps in needs no wiring of its own.
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('.remove-subtask-btn');
    if (btn) { btn.closest('.subtask-row').remove(); }
  });

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
    }
    if (target.classList.contains('subtasks-container')) {
      var last = target.lastElementChild;
      var box = last && last.querySelector('.subtask-text');
      if (box) { box.focus(); }
    }
    if (target.id === 'editCardModalContent') {
      showModal('editCardModal');
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
      // Only the edit form closes the modal. Everything else that answers with
      // the card face does so as a side effect — a comment brings it along so
      // its badge keeps up, a quick edit redraws it in place — and none of
      // those should shut a form somebody is typing in.
      var path = (evt.detail.pathInfo && evt.detail.pathInfo.requestPath) || '';
      if (/^\/cards\/[^/]+$/.test(path)) { hideModal('editCardModal'); }
    }
  });
  document.body.addEventListener('htmx:responseError', function (evt) {
    var xhr = evt.detail.xhr;
    alert(xhr && xhr.responseText ? xhr.responseText : 'Request failed');
    // A refused drop leaves the card where it was let go of, which is not
    // where the server has it. Nothing short of a redraw puts that right.
    var path = (evt.detail.pathInfo && evt.detail.pathInfo.requestPath) || '';
    if (/\/order$/.test(path)) { location.reload(); }
  });
  }

  // Registering the worker is what makes the board installable; the worker
  // itself caches nothing but the offline page. A service worker needs a secure
  // context, so over plain http on a LAN address this does nothing and the board
  // works exactly as it did, which is the right way round: the tunnel is where
  // it gets installed from.
  function initServiceWorker() {
    if (!('serviceWorker' in navigator) || !window.isSecureContext) { return; }
    navigator.serviceWorker.register('/sw.js').catch(function (err) {
      console.warn('service worker not registered', err);
    });
  }

  function init() {
    initServiceWorker();
    initDarkMode();
    initHtmxHooks();
    initQuickEdit();
    initRowSaves();
    initSortable();
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
      card.classList.toggle('ring-indigo-500', b.checked);
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
