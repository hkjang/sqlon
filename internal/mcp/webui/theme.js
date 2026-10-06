/*
 * Runs in <head>, before the first paint: applies the saved colour theme and
 * sidebar width so a page never flashes light-then-dark or jumps sideways
 * when the shell (nav.js, at the end of <body>) mounts.
 */
(function () {
  var d = document.documentElement;
  var s = document.currentScript;
  try {
    var pref = localStorage.getItem('sqlon-theme') || 'system';
    var dark = pref === 'dark' || (pref === 'system' && window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches);
    d.setAttribute('data-theme', dark ? 'dark' : 'light');
    if (localStorage.getItem('sqlon-sidebar') === 'mini') d.classList.add('sb-mini');
    // one "current database" for every page (older pages kept their own)
    if (!localStorage.getItem('sqlonObserveProfile')) {
      var old = localStorage.getItem('jasqlLastProfile') || localStorage.getItem('sqlonWorkloadProfile');
      if (old) localStorage.setItem('sqlonObserveProfile', old);
    }
  } catch (e) {
    d.setAttribute('data-theme', 'light');
  }
  if (s && s.hasAttribute('data-shell')) d.classList.add('shell');
})();
