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

  // Tab navigation. Clicks on [data-tab-url] fetch the URL and replace the
  // innerHTML of the element with id matching [data-tab-target].
  document.addEventListener('click', function (e) {
    var el = e.target.closest('[data-tab-url]');
    if (!el) return;
    e.preventDefault();
    var url = el.getAttribute('data-tab-url');
    var targetId = el.getAttribute('data-tab-target');
    if (!url || !targetId) return;
    var siblings = el.parentElement ? el.parentElement.querySelectorAll('[data-tab-url]') : [];
    for (var i = 0; i < siblings.length; i++) siblings[i].classList.remove('active');
    el.classList.add('active');
    fetch(url, { credentials: 'same-origin' }).then(function (r) {
      if (!r.ok) return;
      return r.text().then(function (t) {
        var target = document.getElementById(targetId);
        if (target) target.innerHTML = t;
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
