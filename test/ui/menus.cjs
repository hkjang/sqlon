// Browser check for 메뉴 관리 (/admin/menus) against a running sqlon.
//
// Login mode (meta DB) is required; the admin account comes from SQLON_ADMIN
// and the check creates a DBA and a regular user if they are missing. It
// resets the menu switches first, so it can run any number of times.
//
//   SQLON_URL=http://127.0.0.1:6791 SQLON_ADMIN=admin:AdminPass123 \
//   PLAYWRIGHT=/path/to/node_modules/playwright node test/ui/menus.cjs
//
// Optional: SQLON_STANDALONE_URL + SQLON_STANDALONE_TOKEN also check a
// standalone server (no meta DB); SHOTS=<dir> saves screenshots.
// docs/development.md explains how to start the servers.
'use strict';
const { chromium } = require(process.env.PLAYWRIGHT || 'playwright');
const fs = require('fs');

const BASE = (process.env.SQLON_URL || 'http://127.0.0.1:6791').replace(/\/$/, '');
const [ADMIN, ADMIN_PW] = (process.env.SQLON_ADMIN || 'admin:AdminPass123').split(':');
const USERS = { dba: ['menucheck-dba', 'MenuCheck123'], user: ['menucheck-user', 'MenuCheck123'] };
const SHOTS = process.env.SHOTS || '';
if (SHOTS) fs.mkdirSync(SHOTS, { recursive: true });

let failed = 0, passed = 0;
const check = (name, ok, detail) => {
  if (ok) passed++; else failed++;
  console.log((ok ? '  ok   ' : '  FAIL ') + name + (detail ? ' — ' + detail : ''));
};
const errors = [];
const shot = (page, name, full) => SHOTS ? page.screenshot({ path: SHOTS + '/' + name + '.png', fullPage: !!full }) : null;

async function session(browser, base, user, pw, opts = {}) {
  const ctx = await browser.newContext({ viewport: opts.viewport || { width: 1366, height: 900 }, colorScheme: opts.dark ? 'dark' : 'light' });
  if (user) {
    const r = await ctx.request.post(base + '/auth/login', { data: { username: user, password: pw } });
    if (!r.ok()) throw new Error('login ' + user + ': ' + r.status());
  }
  const page = await ctx.newPage();
  page.on('console', m => { if (m.type() === 'error' && !/status of 40[19]/.test(m.text())) errors.push((user || 'standalone') + ': ' + m.text()); });
  page.on('pageerror', e => errors.push((user || 'standalone') + ': ' + e));
  return { ctx, page };
}
const sidebar = page => page.$$eval('.jsb nav .jlink .lbl', ns => ns.map(n => n.textContent.trim()));
const text = (page, sel) => page.$eval(sel, n => n.textContent.replace(/\s+/g, ' ').trim());
const pathOf = page => new URL(page.url()).pathname;
const saveVisible = page => page.evaluate(() => {
  const b = document.querySelector('#btnSave').getBoundingClientRect();
  const hit = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
  return !!hit && !!hit.closest('#btnSave');
});

// every visible text node in <main> against its real (blended) background
const contrast = page => page.evaluate(() => {
  const parse = c => { const m = c && c.match(/rgba?\(([^)]+)\)/); if (!m) return null; const p = m[1].split(',').map(parseFloat); return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 }; };
  const over = (t, b) => { const a = t.a + b.a * (1 - t.a); return { r: (t.r * t.a + b.r * b.a * (1 - t.a)) / a, g: (t.g * t.a + b.g * b.a * (1 - t.a)) / a, b: (t.b * t.a + b.b * b.a * (1 - t.a)) / a, a }; };
  const bgOf = el => {
    const layers = [];
    for (let n = el; n; n = n.parentElement) {
      const c = parse(getComputedStyle(n).backgroundColor);
      if (c && c.a > 0) { layers.push(c); if (c.a >= 1) break; }
    }
    let acc = { r: 255, g: 255, b: 255, a: 1 };
    for (let i = layers.length - 1; i >= 0; i--) acc = over(layers[i], acc);
    return acc;
  };
  const lum = c => { const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }; return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b); };
  const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
  const fails = []; let n = 0;
  const w = document.createTreeWalker(document.querySelector('main'), NodeFilter.SHOW_TEXT);
  while (w.nextNode()) {
    const t = w.currentNode; if (!t.textContent.trim()) continue;
    const el = t.parentElement; const rect = el.getBoundingClientRect();
    if (!rect.width || !rect.height || getComputedStyle(el).visibility === 'hidden') continue;
    if (el.closest('.dim, .na, [disabled], .sr-only')) continue; // disabled controls are exempt (WCAG 1.4.3)
    const bg = bgOf(el); const r = ratio(over(parse(getComputedStyle(el).color), bg), bg);
    const cs = getComputedStyle(el); const size = parseFloat(cs.fontSize); const bold = parseInt(cs.fontWeight, 10) >= 700;
    n++;
    if (r < (size >= 24 || (bold && size >= 18.66) ? 3 : 4.5)) fails.push(t.textContent.trim().slice(0, 24) + ' ' + r.toFixed(2));
  }
  // an off switch's track against the card it sits on (WCAG 1.4.11: 3:1)
  const sw = document.querySelector('.mm-row .ui-switch:not(:checked):not(:disabled)');
  let track = null;
  if (sw) { const c = parse(getComputedStyle(sw).backgroundColor); const bg = bgOf(sw.parentElement); track = ratio(over(c, bg), bg).toFixed(2); if (track < 3) fails.push('off switch track ' + track); }
  return { n, fails, track };
});

async function ensureUser(req, name, pw, role) {
  const list = await (await req.get(BASE + '/api/users')).json();
  if ((list.users || []).some(u => u.username === name)) return;
  const r = await req.post(BASE + '/api/users', { data: { username: name, password: pw, role, display_name: name } });
  if (!r.ok()) throw new Error('create ' + name + ': ' + r.status() + ' ' + await r.text());
}

(async () => {
  const browser = await chromium.launch();
  const A = await session(browser, BASE, ADMIN, ADMIN_PW);
  const api = A.ctx.request;
  await ensureUser(api, USERS.dba[0], USERS.dba[1], 'dba');
  await ensureUser(api, USERS.user[0], USERS.user[1], 'user');
  const reset = await api.put(BASE + '/api/console/menus', { data: { menus: {} } });
  if (!reset.ok()) throw new Error('reset menus: ' + reset.status());
  const rev = async () => (await (await api.get(BASE + '/api/console/menus')).json()).revision;

  // ---------------------------------------------------------------- admin
  console.log('admin · the page');
  const p = A.page;
  await p.goto(BASE + '/admin/menus');
  await p.waitForSelector('.mm-row');
  check('sidebar lists 메뉴 관리', (await sidebar(p)).includes('메뉴 관리'));
  check('every menu is listed', (await p.$$('.mm-row')).length === 26, String((await p.$$('.mm-row')).length));
  check('메뉴 관리 cannot be switched off', await p.getByRole('switch', { name: '메뉴 관리 켜기' }).isDisabled());
  check('사용자 menu offers only the admin role', await p.$eval('input[data-key="users"][data-role="user"]', n => n.disabled && n.closest('label').classList.contains('na')));
  const sw = await p.$eval('.mm-row .ui-switch', n => { const r = n.getBoundingClientRect(); return r.width + 'x' + r.height; });
  check('switches render as switches', sw === '38x22', sw);
  const before = await text(p, '#preview');
  check('preview counts per role', /관리자 26 \/ 26/.test(before) && /DBA 23 \/ 23/.test(before) && /사용자 21 \/ 21/.test(before), before);
  check('cards are padded and bordered', await p.evaluate(() => ['.intro', '.mm-history'].every(sel => {
    const cs = getComputedStyle(document.querySelector(sel));
    return parseFloat(cs.paddingLeft) >= 12 && parseFloat(cs.borderTopWidth) >= 1;
  })));
  check('the storage location does not reveal the server path', (await text(p, '#metaLine')).includes('<data>/operations/console/menus.json'));
  await shot(p, 'admin-initial', true);

  // group switch, and the mixed state
  await p.click('[data-gswitch="meta"]');
  check('group switch turns the whole group off', (await p.$$eval('[data-group="meta"] .mm-row.off', ns => ns.length)) === 6);
  await p.click('[data-gswitch="meta"]');
  check('…and back on, leaving nothing to save', (await p.$$eval('[data-group="meta"] .mm-row.off', ns => ns.length)) === 0 && await p.isDisabled('#btnSave'));
  await p.getByRole('switch', { name: 'OpenMetadata 켜기' }).click();
  const mixed = await p.$eval('[data-gswitch="meta"]', n => ({ ind: n.indeterminate, role: n.getAttribute('role'), bg: getComputedStyle(n).backgroundImage }));
  check('a partly-on group shows a mixed state (and is not announced as a switch)', mixed.ind && mixed.role === null && /gradient/.test(mixed.bg), JSON.stringify(mixed));
  check('an off row disables its role chips', await p.$eval('input[data-key="openmetadata"][data-role="dba"]', n => n.disabled));

  // roles, and the last-role guard
  await p.click('input[data-key="ask"][data-role="user"]');
  await p.click('input[data-key="ask"][data-role="dba"]');
  await p.click('input[data-key="ask"][data-role="admin"]');
  check('the last role cannot be removed', await p.isChecked('input[data-key="ask"][data-role="admin"]') && (await text(p, '#msg')).includes('메뉴를 끄세요'));
  await p.click('input[data-key="ask"][data-role="dba"]');
  const after = await text(p, '#preview');
  check('preview counts follow before saving', /관리자 25 \/ 26/.test(after) && /DBA 22 \/ 23/.test(after) && /사용자 19 \/ 21/.test(after), after);
  check('unsaved changes are counted', (await text(p, '#dirty')) === '저장하지 않은 변경 2건');

  // view as a role
  await p.click('button.mm-count[data-view="user"]');
  const unseen = await p.$$eval('.mm-row.unseen .mm-name', ns => ns.map(n => n.childNodes[0].textContent));
  check('view as 사용자 marks what a user will not see', unseen.includes('SQL Lab') && unseen.includes('OpenMetadata') && unseen.includes('사용자') && !unseen.includes('운영 현황'), unseen.join(','));
  check('…and says so', (await text(p, '#viewNote')).includes('사용자로 보는 중 — 보이는 메뉴 19개, 꺼서 숨긴 메뉴 2개(SQL Lab, OpenMetadata)') &&
    (await p.getAttribute('button.mm-count[data-view="user"]', 'aria-pressed')) === 'true', await text(p, '#viewNote'));
  await shot(p, 'admin-view-as-user', true);
  await p.click('#viewOff');
  check('그만 보기 clears it', (await p.$$('.mm-row.unseen')).length === 0);

  // search
  await p.fill('#filter', '품질');
  check('search narrows to matching menus', (await p.$$('.mm-row')).length === 1 && (await text(p, '#shown')) === '1 / 26개 메뉴');
  await p.fill('#filter', '/admin/sess');
  check('search matches addresses too', (await p.$$eval('.mm-row .mm-name', ns => ns.map(n => n.childNodes[0].textContent))).join() === '세션 · 잠금');
  await p.fill('#filter', 'zzz');
  check('no match says so', (await text(p, '#groups')).includes('"zzz"에 맞는 메뉴가 없습니다'));
  await p.fill('#filter', '');

  // keyboard
  await p.getByRole('switch', { name: '메타 품질 켜기' }).focus();
  await p.keyboard.press('Space');
  const focused = await p.evaluate(() => document.activeElement && document.activeElement.getAttribute('aria-label'));
  check('Space toggles a switch and focus stays on it', focused === '메타 품질 켜기' && !(await p.isChecked('.ui-switch[data-key="quality"]')), focused);
  await p.keyboard.press('Space');

  // the sticky bar stays usable at the bottom of the list
  await p.evaluate(() => window.scrollTo(0, document.body.scrollHeight));
  await p.waitForTimeout(150);
  check('scrolled down: the save button stays visible and clickable', await saveVisible(p));
  await shot(p, 'admin-scrolled');
  await p.evaluate(() => window.scrollTo(0, 0));

  // a role left with nothing
  for (const g of ['monitor', 'diagnose', 'sql', 'meta', 'settings']) {
    if (await p.isChecked('[data-gswitch="' + g + '"]') || await p.$eval('[data-gswitch="' + g + '"]', n => n.indeterminate)) {
      await p.click('[data-gswitch="' + g + '"]');
      if (await p.isChecked('[data-gswitch="' + g + '"]')) await p.click('[data-gswitch="' + g + '"]');
    }
  }
  check('a role with nothing left is warned about before saving', (await text(p, '#zeroWarn')).includes('사용자 역할이 열 수 있는 화면이 없습니다') &&
    await p.$eval('button.mm-count[data-view="user"]', n => n.classList.contains('zero')), await text(p, '#zeroWarn'));
  await p.click('#btnRevert');
  check('되돌리기 restores the loaded settings', (await p.$$('.mm-row.off')).length === 0 && await p.isDisabled('#btnSave'));

  // redo the two changes and save with Ctrl+S
  await p.getByRole('switch', { name: 'OpenMetadata 켜기' }).click();
  await p.click('input[data-key="ask"][data-role="user"]');
  await p.keyboard.press('Control+s');
  await p.waitForSelector('#msg.ok');
  const sb = await sidebar(p);
  check('Ctrl+S saves; the sidebar is redrawn at once', !sb.includes('OpenMetadata') && sb.includes('SQL Lab') && sb.includes('메뉴 관리'), sb.join(','));
  check('nothing left unsaved', await p.isHidden('#dirty'));
  const h1 = await text(p, '#history li:first-child');
  check('최근 변경 lists who changed what', h1.includes(ADMIN) && h1.includes('SQL Lab 관리자·DBA만') && h1.includes('OpenMetadata 끔'), h1);
  await p.keyboard.press('Control+k');
  await p.fill('.jpalette input', 'OpenMetadata');
  check('Ctrl+K no longer offers it', await p.isVisible('.jp-empty'));
  await p.keyboard.press('Escape');
  await p.reload();
  await p.waitForSelector('.mm-row');
  check('persisted across a reload', !(await p.isChecked('.ui-switch[data-key="openmetadata"]')) && !(await p.isChecked('input[data-key="ask"][data-role="user"]')));

  // two admins at once
  console.log('admin · another admin saves first');
  await p.getByRole('switch', { name: '통계 켜기' }).click(); // mine: stats off
  await p.getByRole('switch', { name: '메타 검토 켜기' }).click(); // mine: reviews off
  const theirs = await api.put(BASE + '/api/console/menus', { data: { revision: await rev(), menus: {
    openmetadata: { enabled: false }, ask: { roles: ['admin', 'dba'] }, editor: { enabled: false }, reviews: { roles: ['admin'] } } } });
  check('(the other admin saved)', theirs.ok());
  await p.click('#btnSave');
  await p.waitForSelector('#msg.warn');
  const warn = await text(p, '#msg');
  check('a stale save is refused and explained', warn.includes('먼저 저장했습니다') && warn.includes('내 변경 2건을 그 위에 다시 얹었습니다') && warn.includes('같은 메뉴(메타 검토)는 내 값으로'), warn);
  check('their change is kept, mine are reapplied', !(await p.isChecked('.ui-switch[data-key="editor"]')) && !(await p.isChecked('.ui-switch[data-key="stats"]')) &&
    !(await p.isChecked('.ui-switch[data-key="reviews"]')) && (await text(p, '#dirty')) === '저장하지 않은 변경 2건');
  await p.click('#btnSave');
  await p.waitForSelector('#msg.ok');
  const latest = await (await api.get(BASE + '/api/console/menus')).json();
  const off = latest.menus.filter(m => !m.enabled).map(m => m.key).sort().join();
  check('the second save has both admins\' changes', off === 'editor,openmetadata,reviews,stats', off);

  // ----------------------------------------------------------------- user
  // leave: openmetadata off, ask admin+dba, db admin+dba, fleet off
  await api.put(BASE + '/api/console/menus', { data: { menus: {
    openmetadata: { enabled: false }, ask: { roles: ['admin', 'dba'] }, db: { roles: ['admin', 'dba'] } } } });
  console.log('user');
  const U = await session(browser, BASE, USERS.user[0], USERS.user[1]);
  const u = U.page;
  await u.goto(BASE + '/');
  await u.waitForSelector('.ui-firstrun');
  const usb = await sidebar(u);
  check('user sidebar drops SQL Lab, OpenMetadata, DB 연결, 메뉴 관리', !['SQL Lab', 'OpenMetadata', 'DB 연결', '메뉴 관리'].some(x => usb.includes(x)), usb.join(','));
  check('a shortcut to a switched-off page is dropped, others stay', !(await u.isVisible('.panel-head .actions a[href="/admin/ask"]')) && await u.isVisible('.panel-head .actions a[href="/admin/dba"]'));
  check('first-run hint asks the admin instead of linking to DB 연결', (await text(u, '.ui-firstrun')).includes('관리자에게 DB 등록을 요청') && (await u.$('.ui-firstrun a')) === null);
  check('a link inside a sentence stays', await u.$eval('main a[href="/admin/db"]', a => getComputedStyle(a).display !== 'none' && /등록/.test(a.parentElement.textContent)));
  await u.goto(BASE + '/admin/ask');
  await u.waitForSelector('.ui-notice');
  check('a typed address lands on the first open page, with the reason', pathOf(u) === '/' && (await text(u, '.ui-notice')).includes('‘SQL Lab’ 화면은 꺼져 있습니다'));
  check('menu_off is removed from the address bar', !u.url().includes('menu_off'), u.url());
  await shot(u, 'user-refused');
  const uc = await contrast(u);
  check('notice: AA contrast', uc.fails.length === 0, uc.n + ' texts ' + uc.fails.join(' | '));
  await u.click('.ui-notice-x');
  check('the notice closes', (await u.$('.ui-notice')) === null);
  await u.goto(BASE + '/admin/menus');
  check('a user is sent away from 메뉴 관리 without a misleading notice', pathOf(u) === '/' && (await u.$('.ui-notice')) === null, u.url());
  await api.put(BASE + '/api/console/menus', { data: { menus: { fleet: { enabled: false } } } });
  await u.goto(BASE + '/');
  await u.waitForSelector('.jsb');
  check('the default landing switched off: moved on quietly', pathOf(u) === '/admin/alerts' && (await u.$('.ui-notice')) === null, u.url());

  // a switched-off 예방 경보 sends no signal (the summary is faked: a test
  // server has no alerts)
  await api.put(BASE + '/api/console/menus', { data: { menus: { alerts: { roles: ['admin', 'dba'] } } } });
  const fake = route => route.fulfill({ json: { alerts: { critical: 3, warning: 1 }, changes: { awaiting_approval: 2 } } });
  await u.route('**/api/console/summary', fake);
  await u.goto(BASE + '/admin/sessions');
  await u.waitForSelector('.jsb');
  await u.waitForTimeout(300);
  check('alerts switched off: no "(긴급 N)" in the tab title', !(await u.title()).startsWith('(긴급'), await u.title());

  console.log('dba');
  await api.put(BASE + '/api/console/menus', { data: { menus: { openmetadata: { enabled: false }, ask: { roles: ['admin', 'dba'] }, alerts: { roles: ['admin', 'dba'] } } } });
  const D = await session(browser, BASE, USERS.dba[0], USERS.dba[1]);
  await D.page.goto(BASE + '/');
  await D.page.waitForSelector('.jsb');
  const dsb = await sidebar(D.page);
  check('dba keeps SQL Lab, loses OpenMetadata', dsb.includes('SQL Lab') && !dsb.includes('OpenMetadata'), dsb.join(','));
  await D.page.route('**/api/console/summary', fake);
  await D.page.goto(BASE + '/admin/sessions');
  await D.page.waitForSelector('.jbadge[data-badge="alerts"]:not([hidden])');
  check('…while a role that sees 예방 경보 still gets the badge and title', (await D.page.title()).startsWith('(긴급 3)') && (await text(D.page, '.jbadge[data-badge="alerts"]')) === '3', await D.page.title());

  // ------------------------------------------------- phone width, dark
  for (const [label, opts] of [['390px', { viewport: { width: 390, height: 844 } }], ['dark', { dark: true }], ['390px dark', { viewport: { width: 390, height: 844 }, dark: true }], ['light', {}]]) {
    console.log('admin · ' + label);
    const S = await session(browser, BASE, ADMIN, ADMIN_PW, opts);
    await S.page.goto(BASE + '/admin/menus');
    await S.page.waitForSelector('.mm-row');
    check('no horizontal overflow', await S.page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1));
    await S.page.click('button.mm-count[data-view="user"]');
    const c = await contrast(S.page);
    check('AA text contrast, 3:1 off switch', c.fails.length === 0, c.n + ' texts, off track ' + c.track + ' ' + c.fails.join(' | '));
    await S.page.click('.ui-switch[data-key="quality"]');
    await S.page.evaluate(() => window.scrollTo(0, document.body.scrollHeight));
    await S.page.waitForTimeout(150);
    check('scrolled: the save button stays reachable', await saveVisible(S.page));
    await shot(S.page, 'admin-' + label.replace(/\s+/g, '-'));
    await S.ctx.close();
  }

  // ------------------------------------------------------- standalone
  if (process.env.SQLON_STANDALONE_URL) {
    const SB = process.env.SQLON_STANDALONE_URL.replace(/\/$/, '');
    console.log('standalone');
    const S = await session(browser, SB);
    const sp = S.page;
    await S.ctx.request.put(SB + '/api/console/menus', { data: { menus: {} }, headers: { 'X-Admin-Token': process.env.SQLON_STANDALONE_TOKEN || '' } });
    await sp.goto(SB + '/admin/menus');
    await sp.waitForSelector('.mm-row');
    check('standalone: no role chips, one count', !(await sp.isVisible('.mm-roles')) && /보이는 메뉴 21 \/ 21/.test(await text(sp, '#preview')));
    await sp.getByRole('switch', { name: '세션 · 잠금 켜기' }).click();
    await sp.click('#btnSave');
    await sp.waitForSelector('#msg.bad');
    check('saving without the admin token says what to do', (await text(sp, '#msg')).includes('관리 토큰이 필요합니다'));
    await sp.click('.ui-tool:has-text("관리 토큰")');
    await sp.fill('#adminToken', process.env.SQLON_STANDALONE_TOKEN || '');
    await sp.click('#btnSave');
    await sp.waitForSelector('#msg.ok');
    check('with the token it saves and redraws the sidebar', !(await sidebar(sp)).includes('세션 · 잠금'));
    await sp.goto(SB + '/admin/sessions');
    await sp.waitForSelector('.ui-notice');
    check('a typed address is refused with the reason', pathOf(sp) === '/' && (await text(sp, '.ui-notice')).includes('세션 · 잠금'));
    await S.ctx.request.put(SB + '/api/console/menus', { data: { menus: {} }, headers: { 'X-Admin-Token': process.env.SQLON_STANDALONE_TOKEN || '' } });
  }

  // leave the server as it was found
  await api.put(BASE + '/api/console/menus', { data: { menus: {} } });
  check('no console errors', errors.length === 0, errors.join(' | '));
  await browser.close();
  console.log((failed ? failed + ' FAILED, ' : 'ALL PASS, ') + passed + ' passed');
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
