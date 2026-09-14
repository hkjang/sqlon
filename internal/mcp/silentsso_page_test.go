package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// The decision of *when* to try a silent sign-in lives in login.html. These
// scenarios run that inline script under node's vm with a fake browser so the
// loop guards are checked, not just the server side. Skipped without node.

const silentSSOHarness = `
'use strict';
const vm = require('vm'), fs = require('fs');
const script = fs.readFileSync(process.argv[2], 'utf8');

function run(opts) {
  const store = {};
  const sessionStorage = opts.storageThrows
    ? { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); }, removeItem() { throw new Error('blocked'); } }
    : { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } };
  Object.assign(store, opts.store || {});
  const assigned = [], hrefs = [];
  const el = () => ({ style: {}, classList: { remove() {}, add() {} }, innerHTML: '', textContent: '' });
  const location = { search: opts.search || '', assign: u => assigned.push(u) };
  Object.defineProperty(location, 'href', { set: v => hrefs.push(v), get: () => '' });
  const sandbox = {
    sessionStorage, location, URLSearchParams, encodeURIComponent, console,
    document: { getElementById: el, querySelector: el },
    fetch: () => Promise.resolve({ json: () => Promise.resolve(opts.me) }),
  };
  vm.runInNewContext(script, sandbox);
  return new Promise(res => setTimeout(() => res({ assigned, hrefs, store }), 10));
}

const on = { auth_enabled: true, authenticated: false, sso_enabled: true, sso_auto_login: true };
const off = { auth_enabled: true, authenticated: false, sso_enabled: true, sso_auto_login: false };
const A = 'sqlon.sso.silentAttempted', S = 'sqlon.sso.signedOut';
const cases = [
  { name: 'fresh tab with deep link tries once and returns there', o: { me: on, search: '?next=%2Fadmin%2Fdb%3Fx%3D1' },
    want: { assigned: ['/auth/sso/login?prompt=none&return_to=%2Fadmin%2Fdb%3Fx%3D1'], attempted: true } },
  { name: 'already attempted in this tab session → no retry', o: { me: on, store: { [A]: 'true' } }, want: { assigned: [] } },
  { name: 'signed out on purpose → no auto-login', o: { me: on, store: { [S]: 'true' } }, want: { assigned: [] } },
  { name: 'callback marker sso=none in the address → no retry', o: { me: on, search: '?sso=none&next=%2Fadmin' }, want: { assigned: [] } },
  { name: 'callback marker sso=error → no retry', o: { me: on, search: '?sso=error' }, want: { assigned: [] } },
  { name: 'storage unreadable (private mode) fails closed', o: { me: on, storageThrows: true }, want: { assigned: [] } },
  { name: 'auto_login off → nothing changes', o: { me: off }, want: { assigned: [] } },
  { name: 'unsafe next falls back to console root', o: { me: on, search: '?next=%2F%2Fevil.com' },
    want: { assigned: ['/auth/sso/login?prompt=none&return_to=%2Fadmin'] } },
  { name: 'existing session goes straight to next', o: { me: { auth_enabled: true, authenticated: true, sso_enabled: true, sso_auto_login: true }, search: '?next=%2Fadmin%2Fdb' },
    want: { assigned: [], hrefs: ['/admin/db'] } },
];

(async () => {
  let failed = 0;
  for (const c of cases) {
    const got = await run(c.o);
    const problems = [];
    if (JSON.stringify(got.assigned) !== JSON.stringify(c.want.assigned)) problems.push('assigned=' + JSON.stringify(got.assigned));
    if (c.want.hrefs && JSON.stringify(got.hrefs) !== JSON.stringify(c.want.hrefs)) problems.push('hrefs=' + JSON.stringify(got.hrefs));
    if (c.want.attempted && got.store[A] !== 'true') problems.push('attempted flag not set');
    if (problems.length) { failed++; console.log('FAIL ' + c.name + ': ' + problems.join(', ')); }
    else console.log('ok   ' + c.name);
  }
  process.exit(failed ? 1 : 0);
})();
`

func TestSilentSSOLoginPageRules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; login.html silent-SSO rules not exercised")
	}
	html, err := os.ReadFile(filepath.Join("webui", "login.html"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindSubmatch(html)
	if m == nil {
		t.Fatal("login.html has no inline script")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "login.js")
	harnessPath := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(scriptPath, m[1], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harnessPath, []byte(silentSSOHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harnessPath, scriptPath).CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("login.html silent SSO rules: %v", err)
	}
}
