// Minimal polling shim used by branch_detail.html for the checks table.
// Any element with `data-poll-url` is refreshed periodically; the server
// returns an HTML fragment that replaces the element's innerHTML.
(function () {
  function attach(el) {
    var url = el.getAttribute('data-poll-url');
    var ms = parseInt(el.getAttribute('data-poll-interval') || '10000', 10);
    if (!url) return;
    setInterval(function () {
      fetch(url, { credentials: 'same-origin' }).then(function (r) {
        if (!r.ok) return;
        return r.text().then(function (t) { el.innerHTML = t; });
      }).catch(function () { /* ignore transient errors */ });
    }, ms);
  }
  document.addEventListener('DOMContentLoaded', function () {
    var els = document.querySelectorAll('[data-poll-url]');
    for (var i = 0; i < els.length; i++) attach(els[i]);
  });

  // Theme toggle. Cycles document <html data-theme>, persists to localStorage.
  function currentTheme() {
    var t = document.documentElement.getAttribute('data-theme');
    if (t === 'light' || t === 'dark') return t;
    try { t = localStorage.getItem('ds-theme'); } catch (e) {}
    return (t === 'light') ? 'light' : 'dark';
  }
  function applyTheme(t) {
    document.documentElement.setAttribute('data-theme', t);
    try { localStorage.setItem('ds-theme', t); } catch (e) {}
  }
  document.addEventListener('DOMContentLoaded', function () {
    var btns = document.querySelectorAll('[data-theme-toggle]');
    for (var i = 0; i < btns.length; i++) {
      btns[i].addEventListener('click', function () {
        applyTheme(currentTheme() === 'light' ? 'dark' : 'light');
      });
    }
  });

  // Tab switching. Clicking [data-tab-url] fetches the URL and swaps the
  // innerHTML of the element matching [data-tab-target] on the same tab link.
  document.addEventListener('click', function (e) {
    var el = e.target.closest('[data-tab-url]');
    if (!el) return;
    var url = el.getAttribute('data-tab-url');
    var targetSel = el.getAttribute('data-tab-target');
    if (!url || !targetSel) return;
    e.preventDefault();
    var target = document.querySelector(targetSel);
    if (!target) return;
    // Update active class on siblings within [data-tab-group].
    var group = el.closest('[data-tab-group]');
    if (group) {
      var tabs = group.querySelectorAll('[data-tab-url]');
      for (var i = 0; i < tabs.length; i++) tabs[i].classList.remove('active');
    }
    el.classList.add('active');
    fetch(url, { credentials: 'same-origin' }).then(function (r) {
      if (!r.ok) return;
      return r.text().then(function (t) { target.innerHTML = t; });
    }).catch(function () {});
  });

  // Minimal HTMX-like click handler for hx-get, hx-target, hx-swap attributes.
  // Supports two patterns:
  //   hx-swap="afterend" + hx-target="closest .selector" — inserts fetched HTML after closest ancestor
  //   hx-swap="innerHTML" + hx-target="#id"              — replaces innerHTML of target element
  document.addEventListener('click', function (e) {
    var el = e.target.closest('[hx-get]');
    if (!el) return;
    var url = el.getAttribute('hx-get');
    var targetSel = el.getAttribute('hx-target');
    var swap = el.getAttribute('hx-swap') || 'innerHTML';
    if (!url || !targetSel) return;
    e.preventDefault();
    fetch(url, { credentials: 'same-origin' }).then(function (r) {
      if (!r.ok) return;
      return r.text().then(function (html) {
        if (swap === 'afterend') {
          // targetSel is like "closest .check-row"
          var parts = targetSel.match(/^closest\s+(.+)$/);
          var anchor = parts ? el.closest(parts[1]) : el;
          if (!anchor) return;
          anchor.insertAdjacentHTML('afterend', html);
        } else {
          // innerHTML: targetSel is a CSS selector like "#some-id"
          var target = document.querySelector(targetSel);
          if (!target) return;
          target.innerHTML = html;
        }
      });
    }).catch(function () {});
  });

  // Branch-list filter. Hides tbody rows whose first cell text doesn't match.
  document.addEventListener('DOMContentLoaded', function () {
    var inputs = document.querySelectorAll('[data-branch-filter]');
    for (var i = 0; i < inputs.length; i++) {
      inputs[i].addEventListener('input', function (e) {
        var q = e.target.value.trim().toLowerCase();
        var rows = document.querySelectorAll('table.grid tbody tr');
        for (var j = 0; j < rows.length; j++) {
          var cell = rows[j].querySelector('td');
          if (!cell) continue;
          var match = !q || cell.textContent.toLowerCase().indexOf(q) !== -1;
          rows[j].style.display = match ? '' : 'none';
        }
      });
    }
  });
})();
