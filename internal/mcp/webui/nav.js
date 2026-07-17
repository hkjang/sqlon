'use strict';
/*
 * JASQL app shell: fixed left sidebar + top-right profile menu.
 *
 * Every console page includes <script src="/admin/nav.js"></script> and calls
 *   JASQL.mount({ page: 'ask', onReady(me){ ... } })
 * where `page` is the current nav key (for highlighting) and onReady fires
 * after auth resolves with the /auth/me payload.
 *
 * The shell injects a left sidebar (grouped nav, current-tab highlight,
 * responsive) and shifts the page body to its right. Each page keeps its own
 * <header> as the top bar; the shell appends a profile button on the right that
 * opens a dropdown (개인정보/비밀번호/키 관리/로그아웃 + version footer). The legacy
 * master-token box is hidden by default behind a small toggle in standalone mode.
 */
(function () {
  var SBW = 224; // sidebar width (px)

  // nav groups. show: always | auth (meta DB) | admin
  var GROUPS = [
    { title: '작업', items: [
      { key: 'ask',      href: '/admin/ask',     icon: '💬', label: '질의',      show: 'always' },
      { key: 'history',  href: '/admin/history', icon: '🕘', label: '내 이력',   show: 'auth' },
      { key: 'stats',    href: '/admin/stats',   icon: '📊', label: '통계',      show: 'auth' },
      { key: 'datasets', href: '/admin',         icon: '🗂', label: '데이터셋',  show: 'always' },
      { key: 'editor',   href: '/admin/editor',  icon: '✏️', label: '테이블 편집', show: 'always' },
      { key: 'db',       href: '/admin/db',      icon: '🔌', label: 'DB · 쿼리',  show: 'always' },
      { key: 'dba',      href: '/admin/dba',     icon: '🩺', label: 'DBA 코파일럿', show: 'always' },
      { key: 'dba-console', href: '/admin/dba-console', icon: '🛢', label: 'DBA 콘솔', show: 'dba' },
      { key: 'reviews',  href: '/admin/reviews', icon: '🧾', label: '메타 검토',  show: 'always' },
    ]},
    { title: '관리', items: [
      { key: 'quality',  href: '/admin/quality',  icon: '📈', label: '메타 품질', show: 'always' },
      { key: 'openmetadata', href: '/admin/openmetadata', icon: '🔗', label: 'OpenMetadata', show: 'always' },
      { key: 'profcat',  href: '/admin/profile-catalogs', icon: '🗃', label: '프로파일 카탈로그', show: 'always' },
      { key: 'users',    href: '/admin/users',    icon: '👥', label: '사용자',   show: 'admin' },
      { key: 'settings', href: '/admin/settings', icon: '⚙️', label: '서버 설정', show: 'admin' },
      { key: 'keys',     href: '/admin/keys',     icon: '🔑', label: 'MCP 키',   show: 'auth' },
    ]},
    { title: '문서', items: [
      { key: 'onboarding', icon: '📖', label: '온보딩 가이드', show: 'always', onboarding: true },
      { key: 'docs',     href: '/docs',   icon: '📘', label: 'API 문서', show: 'always', target: '_blank' },
    ]},
  ];

  // Per-page user guides, rendered on demand in a modal via the header
  // "❓ 가이드" button. Keyed by the page key passed to JASQL.mount({page}).
  var GUIDES = {
    ask: '## 질의 (NL2SQL)\n\n자연어 질문으로 SQL을 만들고 실행합니다.\n\n1. 질문을 입력하면 **prepare_sql_context**가 테이블·컬럼·조인·시간조건·검증 힌트를 한 번에 묶어 줍니다.\n2. 응답이 **재질문(needs_clarification)** 이면 제시된 질문에 답한 뒤 다시 실행하세요.\n3. `ready`가 되면 스켈레톤의 `/* SLOT */`만 채워 SQL을 완성합니다.\n4. **검증 → 실행계획 → 실행** 순서로 진행합니다.\n\n> 팁: 특정 DB 프로파일의 카탈로그로 질의하려면 profile을 지정하세요(멀티 DB).',
    datasets: '## 데이터셋 관리\n\n이 서버는 Text2SQL 정확도를 위해 **18개의 JSON 데이터셋**을 참조합니다.\n\n### 1. 데이터셋 이해하기 (목록 뱃지)\n- **필수** — 물리/논리 모델. 서버 기동에 필수라 제거 불가(교체는 가능)\n- **선택** — 없으면 해당 기능 축소 또는 내장 기본값 사용\n- **편집가능** — 이 화면에서 교체/제거 가능\n- **시스템** — feedback/audit. 서버가 자동 기록, 조회만 가능\n- **이슈 N** — 이 파일의 로드 오류/경고 수(상세는 행 클릭)\n\n### 2. 내용 확인\n- 행 클릭 → 용도·기대 스키마·사용 도구·내용 샘플 표시\n- **[현재 내용 불러오기]** → 파일 전체를 편집기에 로드\n- 각 데이터셋의 "기대 스키마"가 곧 작성 규칙입니다\n\n### 3. 넣기/바꾸기 (교체)\n편집기 내용이 **파일의 새 전체 내용**이 됩니다(부분 병합 아님).\n- **[① JSON 검사]** — 문법·형식 사전 확인\n- **[② 적용]** — 자동으로 백업 → 저장 → 재컴파일 → **핫스왑**(재기동 불필요)\n- 컴파일 실패/신규 오류 시 **자동 롤백**. 알고도 적용하려면 **강제 적용** 체크\n\n### 4. 빼기/되돌리기\n- **[데이터셋 제거]** — 백업 후 삭제+핫스왑(필수·시스템은 거부)\n- 모든 변경 전 상태는 **백업/복원** 섹션에 남고, 복원 직전 파일도 재백업되어 복원도 되돌릴 수 있습니다\n\n### 5. 파일을 직접 수정했다면\n볼륨/SSH로 파일을 직접 바꾼 경우 **[🔄 카탈로그 리로드]**로 재컴파일하세요. 실패 시 이전 카탈로그 유지.\n\n### 6. 보안\n`-admin-token`(또는 환경변수) 설정 시 변경 작업에 토큰 필요 — 상단 입력란에 넣으면 자동 전송됩니다.\n\n### 7. MCP 도구 대응\n화면 작업은 MCP 도구와 동일 코드로 실행됩니다: 목록 `list_datasets`, 상세 `get_dataset`, 교체 `put_dataset`, 제거 `remove_dataset`, 리로드 `reload_catalog` (REST `GET/PUT/DELETE /api/datasets`, `POST /api/reload`).',
    editor: '## 테이블 편집\n\n데이터셋을 표(그리드)로 편집하고 **[저장]** 하면 즉시 반영됩니다.\n\n- **셀 수정**: 셀 클릭 → 입력 → `Enter` 확정 / `Esc` 취소. 숫자·true/false·null·JSON(배열/객체) 타입 자동 보존 (예: `["별칭1","별칭2"]`는 배열로 저장)\n- **행 추가**: `[+ 행 추가]` — 빈 행이 맨 위에 생성. 행 번호 옆 `⧉` 복제 / `✕` 삭제\n- **컬럼 추가/이름변경/삭제**: `[+ 컬럼 추가]`, 머리글 호버 시 `✎`(모든 행의 키 변경) / `✕`(모든 행에서 키 제거)\n- **빈 셀**: 저장 시 해당 키를 넣지 않음(빈 문자열이 필요하면 `""` 입력)\n- **저장**: 표 전체가 파일의 새 내용이 됨. 서버가 **백업 → 컴파일 검증 → 핫스왑**, 문제 시 **자동 롤백**. 되돌리기는 데이터셋 화면의 백업/복원 사용\n- **검색**: 일치 행만 표시하되 편집·저장은 전체 데이터 기준\n\n> 중첩이 깊은 `overrides`와 시스템(feedback/audit) 데이터셋은 이 화면에서 제외 — 데이터셋 화면(JSON 콘솔)을 사용하세요.',
    db: '## DB · 쿼리\n\nDB 접속 **프로파일**(postgres/mysql/mariadb)을 등록·테스트하고, 콘솔에서 Read-Only 쿼리를 실행합니다.\n\n### 프로파일 등록\n- **접속 문자열**: `host:port/dbname` 형식. postgres/mysql/mariadb 지원.\n- **비밀번호**: 평문 저장 금지 — `env:변수명`(권장) 또는 `file:/run/secrets/파일`. `plain:값`은 개발용.\n- **계정**: SELECT 권한만 가진 전용 계정을 사용하세요 — 서버 차단과 별개로 DB 권한이 최종 방어선입니다.\n\n### 안전장치\n- SELECT/WITH만 허용, DML/DDL/트랜잭션·다중 statement 차단\n- 쿼리 타임아웃, `max_rows` 제한(초과 시 truncated), 응답 바이트 캡\n- PII 값 마스킹, 연속 실패 시 서킷브레이커, 전 실행 감사 로그(`audit/query-*.jsonl`)\n- 실행 전 **실행계획 게이트** — 위험(full scan/카티션/예상행 과다) 쿼리는 승인 전 차단\n- 프로파일 정책 **비용 상한**(max_plan_cost/rows) 초과 쿼리는 하드 차단\n\n### 실행 순서\n프로파일 등록 → **[접속 테스트]** 초록 확인 → ①**[검증]** → ②**[실행계획]**(실제 EXPLAIN, high면 조건 보강) → ③**[미리보기]** → ④필요 시 **[실행]**. 오래 걸리면 실행 중 목록에서 **[취소]**.\n\n> 실행 성공 응답의 🩺 SQL 린트는 권고이며 실행을 막지 않습니다.',
    reviews: '## 메타 검토\n\n규칙 엔진·OpenMetadata가 만든 **후보**(논리명·의미타입·설명·코드사전·지표·관계)를 승인/반려합니다.\n\n1. 상태(대기/승인/반려)·종류로 필터해 검토합니다.\n2. 행별 또는 일괄로 승인/반려하고 검토자·메모를 남깁니다.\n3. **승인분 반영 + 리로드**로 overrides/metrics/relations에 병합합니다(백업·기존값 보존).',
    quality: '## 메타 품질\n\n테이블별 메타데이터 품질을 7차원(완전성·일관성·관계성·프로파일링·지표연결·사용성·보안성)으로 채점하고 A–E 등급·릴리스 게이트를 보여줍니다.\n\n- **게이트 차단** 항목(지표/조인 손상, PII 미분류, 품질 하한 미달)을 먼저 해소하세요.\n- 하단에서 **감사 로그 해시 체인 무결성**을 검증할 수 있습니다.',
    openmetadata: '## OpenMetadata 연동\n\n전사 카탈로그 OpenMetadata와 양방향 연동합니다.\n\n- **연결 설정**: URL/토큰을 저장(무재기동, `<data>/openmetadata.json`).\n- **Import**: 설명·PII·용어집을 **빈 필드에만** 후보로 가져오기(미리보기 → 반영).\n- **Export**: jamypg 설명을 OM의 빈 컬럼에 push.\n- **Drift**: 두 카탈로그의 불일치(gap/conflict) 대조.',
    profcat: '## 프로파일 카탈로그\n\n등록된 DB 프로파일마다 **독립 카탈로그 워크스페이스**(`<data>/profiles/<profile>/`)를 관리합니다.\n\n1. **라이브 DB로 구축/갱신** — 물리 모델을 워크스페이스에 수집(기존 설명 보존).\n2. **전체 구축** — 모든 프로파일 일괄 구축.\n3. 데이터셋 JSON을 조회·편집(검증·백업·롤백).\n4. **활성 카탈로그로 전환** — 무재기동 핫스왑(단독 모드).\n\n> 요청 단위로 `profile`을 지정하면 전역 전환 없이 그 워크스페이스로 질의·검증됩니다(멀티 DB).',
    dba: '## DBA 코파일럿\n\n연결된 DB와 감사 로그를 근거로 한 **읽기 전용 DBA 진단** 도구 모음입니다. 어떤 것도 자동으로 실행·변경하지 않으며, 결과는 검토용 권고입니다.\n\n- **헬스 점검** — 시스템 카탈로그를 읽어 PK 없는 테이블·미인덱스 FK·미사용 인덱스·오래된 통계·대형 테이블을 진단(엔진별 지원 범위 상이).\n- **인덱스 제안** — 감사 로그의 느린 쿼리에서 인덱스 없는 필터/조인/정렬 컬럼을 집계해 `CREATE INDEX` 후보를 영향도 순으로 제시(직접 검토 후 수행).\n- **워크로드** — 기간별 쿼리량·오류율·지연 분포·핫 테이블·피크 시간대 리포트.\n- **SQL 린트** — 단일 문장의 안티패턴(SELECT * , 선두 와일드카드 LIKE, 비-sargable 조건 등) 정적 진단.\n- **자연어 설명** — SQL이 무엇을 하는지 카탈로그 논리명으로 한국어 요약.\n\n> 상단에서 DB 프로파일을 선택하세요. 인덱스/워크로드는 프로파일을 비우면 전체 로그를 대상으로 합니다.',
    'dba-console': '## DBA 콘솔 (dba/admin 전용)\n\n권한 있는(쓰기 가능) DBA 세션으로 실제 DB를 관리합니다. 읽기 전용 쿼리 계정과 **분리된 커넥션**을 사용하며, 모든 변경은 감사 로그(`dba:*`)에 기록됩니다.\n\n### 사전 준비 — 프로파일에 DBA 자격증명\n**DB · 쿼리** 화면에서 프로파일을 편집해 DBA 블록을 설정해야 콘솔이 동작합니다:\n- `dba.enabled` = true\n- `dba.username` / `dba.password_ref` — 권한 있는 계정(예: postgres superuser 또는 CREATEROLE/CREATEDB 보유 롤). `password_ref`는 `env:이름` 또는 `file:경로` 권장\n- `dba.connect_string`(선택) — postgres에서 `CREATE/DROP DATABASE`를 사용자 DB 안에서 실행하지 않도록 `postgres` 관리 DB로 지정\n\n### 탭\n- **개요** — 방언·서버 버전·역할/DB 수\n- **사용자·역할** — 생성/속성·비밀번호 변경/삭제 (postgres LOGIN·SUPERUSER·CREATEDB·CREATEROLE)\n- **데이터베이스** — 생성/삭제(소유자·인코딩)\n- **권한** — GRANT/REVOKE (`WITH GRANT OPTION`)\n- **설정** — 조회 및 변경(postgres `ALTER SYSTEM`+reload, mysql `SET GLOBAL/SESSION`)\n- **세션** — 활성 세션 조회, 쿼리 취소/세션 종료\n- **유지보수** — VACUUM/ANALYZE/REINDEX (mysql ANALYZE/OPTIMIZE)\n- **SQL 콘솔** — 구조화 도구로 안 되는 작업을 위한 임의 권한 SQL 실행(확인 체크 필요, 원문 감사)\n\n> 삭제(사용자/DB)와 SQL 콘솔은 되돌릴 수 없습니다. 최소 권한 원칙에 따라 DBA 계정 권한을 필요한 만큼만 부여하세요.',
    stats: '## 통계\n\nMCP 활동량과 SQL 유효율, 실행 상태, 최근 추이를 확인합니다.\n\n- 관리자는 **전체 사용자** 집계와 사용자별 호출량을 볼 수 있습니다.\n- 유효율이 낮으면 **내 이력**에서 invalid 생성을 확인하고, 재질문 학습 제안에 따라 지표·용어 사전을 보강하세요.\n- 서버 운영 지표는 `/metrics`의 Prometheus 형식으로도 제공됩니다.',
    history: '## 내 이력\n\n프롬프트·SQL·프로파일·도구명으로 과거 활동을 검색합니다.\n\n- **질의로 다시 사용**: 과거 프롬프트를 질의 화면에 채웁니다.\n- **DB 콘솔로 보내기**: 과거 SQL과 프로파일을 DB 콘솔로 전달합니다.\n- 관리자는 **전체 사용자** 범위로 전환할 수 있습니다.\n\n> 실행 실패와 invalid 생성을 검색해 반복 오류를 찾고, 통계 화면의 품질 신호와 함께 확인하세요.',
    users: '## 사용자 (관리자)\n\n로컬 계정·역할을 관리합니다. admin 역할이 전체 관리 권한을 가집니다.',
    keys: '## MCP 키\n\nMCP 클라이언트 인증용 API 키(jsk_...)를 발급·회전·폐기합니다.',
    settings: '## 서버 설정 (관리자)\n\n마스터 토큰·허용 Origin·Keycloak SSO를 메타 DB에 저장하고 즉시 적용합니다.',
  };

  var esc = function (s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); };

  // mdToHtml: minimal, safe Markdown → HTML (headings, lists, code, bold,
  // inline code, links, paragraphs). Input is escaped first, so it is XSS-safe.
  function mdToHtml(md) {
    var lines = String(md || '').split('\n');
    var html = '', inUl = false, inCode = false;
    var inline = function (t) {
      return esc(t)
        .replace(/`([^`]+)`/g, '<code>$1</code>')
        .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
        .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
    };
    lines.forEach(function (ln) {
      if (/^```/.test(ln)) { if (inCode) { html += '</pre>'; inCode = false; } else { if (inUl) { html += '</ul>'; inUl = false; } html += '<pre>'; inCode = true; } return; }
      if (inCode) { html += esc(ln) + '\n'; return; }
      var m;
      if ((m = ln.match(/^(#{1,4})\s+(.*)/))) { if (inUl) { html += '</ul>'; inUl = false; } html += '<h' + (m[1].length + 2) + '>' + inline(m[2]) + '</h' + (m[1].length + 2) + '>'; return; }
      if ((m = ln.match(/^\s*[-*]\s+(.*)/))) { if (!inUl) { html += '<ul>'; inUl = true; } html += '<li>' + inline(m[1]) + '</li>'; return; }
      if ((m = ln.match(/^\s*\d+\.\s+(.*)/))) { if (!inUl) { html += '<ul>'; inUl = true; } html += '<li>' + inline(m[1]) + '</li>'; return; }
      if ((m = ln.match(/^>\s?(.*)/))) { if (inUl) { html += '</ul>'; inUl = false; } html += '<blockquote>' + inline(m[1]) + '</blockquote>'; return; }
      if (ln.trim() === '') { if (inUl) { html += '</ul>'; inUl = false; } return; }
      if (inUl) { html += '</ul>'; inUl = false; }
      html += '<p>' + inline(ln) + '</p>';
    });
    if (inUl) html += '</ul>';
    if (inCode) html += '</pre>';
    return html;
  }

  // guideModal renders markdown in a scrollable read-only modal.
  function guideModal(title, md) {
    var host = document.createElement('div');
    host.className = 'jmodal';
    host.innerHTML = '<div class="box guidebox"><div class="ghead"><h3>' + esc(title) +
      '</h3><button type="button" class="jb" data-x>닫기</button></div><div class="gbody">' +
      mdToHtml(md) + '</div></div>';
    document.body.appendChild(host);
    var close = function () { host.remove(); };
    host.addEventListener('click', function (e) { if (e.target === host) close(); });
    host.querySelector('[data-x]').onclick = close;
    document.addEventListener('keydown', function esc2(e) { if (e.key === 'Escape') { close(); document.removeEventListener('keydown', esc2); } });
    requestAnimationFrame(function () { host.classList.add('open'); });
  }

  function openOnboarding() {
    guideModal('sqlon 온보딩 가이드', '불러오는 중…');
    fetch('/admin/onboarding.md').then(function (r) { return r.text(); }).then(function (md) {
      var body = document.querySelector('.jmodal.open .gbody');
      if (body) body.innerHTML = mdToHtml(md);
    }).catch(function () {
      var body = document.querySelector('.jmodal.open .gbody');
      if (body) body.innerHTML = '<p>온보딩 문서를 불러오지 못했습니다.</p>';
    });
  }

  function injectStyles() {
    if (document.getElementById('jasql-shell-style')) return;
    var st = document.createElement('style');
    st.id = 'jasql-shell-style';
    st.textContent = [
      ':root{--jsb-w:' + SBW + 'px;--jsb-bg:#0d1626;--jsb-bg2:#111f38;--jsb-ink:#e8eefc;--jsb-sub:#8fa3c4;--jsb-active:#2563eb;}',
      'body{padding-left:var(--jsb-w);transition:padding-left .18s ease;}',
      '@media(max-width:860px){body{padding-left:0;}}',
      // sidebar
      '.jsb{position:fixed;top:0;left:0;width:var(--jsb-w);height:100vh;background:linear-gradient(180deg,#0d1626,#0f1c33);',
      '  color:var(--jsb-ink);display:flex;flex-direction:column;z-index:60;border-right:1px solid #1e2f4d;transition:transform .18s ease;}',
      '.jsb .jbrand{display:flex;align-items:center;gap:9px;padding:16px 18px;font-size:17px;font-weight:800;color:#fff;text-decoration:none;letter-spacing:.3px;}',
      '.jsb .jbrand .dot{font-size:20px;}',
      '.jsb nav{flex:1;overflow-y:auto;padding:6px 10px;}',
      '.jsb .jgrp{margin:10px 4px 4px;font-size:11px;letter-spacing:.6px;text-transform:uppercase;color:var(--jsb-sub);}',
      '.jsb a.jlink{display:flex;align-items:center;gap:10px;padding:9px 12px;margin:2px 0;border-radius:9px;color:#c6d4ec;',
      '  text-decoration:none;font-size:14px;line-height:1.2;}',
      '.jsb a.jlink .ic{width:20px;text-align:center;font-size:15px;}',
      '.jsb a.jlink:hover{background:#17294690;color:#fff;}',
      '.jsb a.jlink.active{background:var(--jsb-active);color:#fff;font-weight:700;box-shadow:0 2px 10px rgba(37,99,235,.4);}',
      '.jsb .jfoot{padding:12px 16px;border-top:1px solid #1e2f4d;font-size:11.5px;color:var(--jsb-sub);}',
      // mobile toggle
      '.jsb-toggle{display:none;position:fixed;top:10px;left:10px;z-index:70;background:#0d1626;color:#fff;border:1px solid #35507f;',
      '  border-radius:8px;padding:7px 11px;font-size:16px;cursor:pointer;}',
      '@media(max-width:860px){.jsb{transform:translateX(-100%);} body.jsb-open .jsb{transform:translateX(0);} .jsb-toggle{display:block;}',
      '  header{padding-left:56px !important;}}',
      '.jsb-scrim{display:none;position:fixed;inset:0;background:rgba(3,8,18,.5);z-index:55;}',
      'body.jsb-open .jsb-scrim{display:block;}',
      // profile menu
      '.jprofile{position:relative;}',
      '.jprofile>button.javatar{display:flex;align-items:center;gap:8px;background:#1d2b47;border:1px solid #35507f;color:#e8eefc;',
      '  border-radius:20px;padding:5px 12px 5px 6px;font-size:13px;cursor:pointer;}',
      '.jprofile>button.javatar:hover{background:#26375a;}',
      '.jprofile .jav{width:26px;height:26px;border-radius:50%;background:#2563eb;color:#fff;display:flex;align-items:center;justify-content:center;font-size:13px;font-weight:700;}',
      '.jmenu{position:absolute;right:0;top:calc(100% + 8px);width:250px;background:#fff;color:#1c2430;border:1px solid #dde4ec;',
      '  border-radius:12px;box-shadow:0 12px 34px rgba(16,26,46,.22);overflow:hidden;display:none;z-index:80;}',
      '.jprofile.open .jmenu{display:block;}',
      '.jmenu .jmhead{padding:14px 16px;background:#f5f7fa;border-bottom:1px solid #eef1f5;}',
      '.jmenu .jmhead b{display:block;font-size:14px;}',
      '.jmenu .jmhead .jmsub{font-size:12px;color:#5b6b7f;margin-top:2px;word-break:break-all;}',
      '.jmenu button.jmitem{display:flex;width:100%;align-items:center;gap:10px;background:none;border:0;text-align:left;',
      '  padding:10px 16px;font-size:13.5px;color:#1c2430;cursor:pointer;}',
      '.jmenu button.jmitem:hover{background:#f2f6ff;}',
      '.jmenu button.jmitem.danger{color:#b91c1c;}',
      '.jmenu .jmsep{height:1px;background:#eef1f5;margin:4px 0;}',
      '.jmenu .jmfoot{padding:9px 16px;border-top:1px solid #eef1f5;font-size:11.5px;color:#8794a8;background:#fafbfc;}',
      // modal
      '.jmodal{position:fixed;inset:0;background:rgba(10,18,32,.5);display:none;align-items:center;justify-content:center;z-index:90;}',
      '.jmodal.open{display:flex;}',
      '.jmodal .box{background:#fff;color:#1c2430;border-radius:14px;width:min(420px,92vw);padding:22px 24px;box-shadow:0 20px 60px rgba(0,0,0,.3);}',
      '.jmodal h3{margin:0 0 4px;font-size:17px;}',
      '.jmodal p.sub{margin:0 0 14px;font-size:12.5px;color:#5b6b7f;}',
      '.jmodal label{display:block;font-size:12.5px;color:#5b6b7f;margin:10px 0 3px;}',
      '.jmodal input{width:100%;border:1px solid #dde4ec;border-radius:8px;padding:9px 11px;font-size:14px;box-sizing:border-box;}',
      '.jmodal .acts{display:flex;justify-content:flex-end;gap:8px;margin-top:18px;}',
      '.jmodal .jb{border:1px solid #dde4ec;background:#fff;border-radius:8px;padding:8px 15px;font-size:13.5px;cursor:pointer;}',
      '.jmodal .jb.primary{background:#2563eb;border-color:#2563eb;color:#fff;font-weight:600;}',
      '.jmodal .msg{font-size:12.5px;margin-top:10px;min-height:16px;}',
      '.jmodal .msg.ok{color:#15803d;} .jmodal .msg.bad{color:#b91c1c;}',
      // help button + guide modal
      'button.jhelp{background:#1d2b47;border:1px solid #35507f;color:#cfe0ff;border-radius:8px;padding:6px 11px;font-size:13px;cursor:pointer;margin-left:6px;}',
      'button.jhelp:hover{background:#26375a;}',
      '.jmodal .box.guidebox{width:min(760px,94vw);max-height:86vh;display:flex;flex-direction:column;padding:0;}',
      '.jmodal .guidebox .ghead{display:flex;align-items:center;justify-content:space-between;padding:14px 20px;border-bottom:1px solid #eef1f5;position:sticky;top:0;background:#fff;border-radius:14px 14px 0 0;}',
      '.jmodal .guidebox .ghead h3{margin:0;}',
      '.jmodal .guidebox .gbody{padding:8px 22px 22px;overflow-y:auto;font-size:14px;line-height:1.7;}',
      '.jmodal .gbody h2{font-size:16px;margin:16px 0 8px;} .jmodal .gbody h3{font-size:14px;margin:14px 0 6px;} .jmodal .gbody h4{font-size:13px;margin:12px 0 4px;}',
      '.jmodal .gbody p{margin:8px 0;} .jmodal .gbody ul{margin:8px 0 8px 4px;padding-left:20px;} .jmodal .gbody li{margin:3px 0;}',
      '.jmodal .gbody code{background:#f0f3f8;border-radius:4px;padding:1px 5px;font-family:ui-monospace,Consolas,monospace;font-size:12.5px;}',
      '.jmodal .gbody pre{background:#0d1626;color:#dbe6ff;border-radius:10px;padding:12px 14px;overflow-x:auto;font-family:ui-monospace,Consolas,monospace;font-size:12.5px;}',
      '.jmodal .gbody pre code{background:none;padding:0;color:inherit;}',
      '.jmodal .gbody blockquote{margin:8px 0;padding:6px 12px;border-left:3px solid #2563eb;background:#f2f6ff;color:#334;border-radius:0 6px 6px 0;}',
      '.jmodal .gbody a{color:#2563eb;}',
    ].join('\n');
    document.head.appendChild(st);
  }

  function tokenHeaders(json) {
    var h = {};
    if (json) h['Content-Type'] = 'application/json';
    var tok = document.getElementById('adminToken');
    if (tok && tok.value) h['X-Admin-Token'] = tok.value;
    return h;
  }

  function buildSidebar(me) {
    var authed = !!(me && me.auth_enabled);
    var role = (authed && me.authenticated && me.user && me.user.role) || '';
    var admin = role === 'admin';
    var dba = admin || role === 'dba';
    var cur = window.JASQL.page;
    var show = function (rule) {
      // standalone (auth disabled) is locally trusted → show dba/admin items too
      if (rule === 'dba') return dba || !authed;
      return rule === 'always' || (rule === 'auth' && authed) || (rule === 'admin' && admin);
    };

    var aside = document.createElement('aside');
    aside.className = 'jsb';
    var html = '<a class="jbrand" href="/"><img src="/admin/logo-transparent.png" style="width:24px;height:24px;object-fit:contain;border-radius:4px;"/> sqlon</a><nav>';
    GROUPS.forEach(function (g) {
      var items = g.items.filter(function (it) { return show(it.show); });
      if (!items.length) return;
      html += '<div class="jgrp">' + esc(g.title) + '</div>';
      items.forEach(function (it) {
        if (it.onboarding) {
          html += '<a class="jlink" href="#" data-onboarding="1">' +
            '<span class="ic">' + it.icon + '</span><span>' + esc(it.label) + '</span></a>';
          return;
        }
        html += '<a class="jlink' + (it.key === cur ? ' active' : '') + '" href="' + it.href + '"' +
          (it.target ? ' target="' + it.target + '"' : '') + '>' +
          '<span class="ic">' + it.icon + '</span><span>' + esc(it.label) + '</span></a>';
      });
    });
    html += '</nav>';
    var ver = (me && me.version) ? ('v' + me.version) : '';
    html += '<div class="jfoot">sqlon ' + esc(ver) + (authed ? '' : ' · 단독 모드') + '</div>';
    aside.innerHTML = html;
    document.body.appendChild(aside);
    var ob = aside.querySelector('[data-onboarding]');
    if (ob) ob.onclick = function (e) { e.preventDefault(); document.body.classList.remove('jsb-open'); openOnboarding(); };

    // mobile hamburger + scrim
    var toggle = document.createElement('button');
    toggle.className = 'jsb-toggle'; toggle.setAttribute('aria-label', '메뉴'); toggle.textContent = '☰';
    toggle.onclick = function () { document.body.classList.toggle('jsb-open'); };
    document.body.appendChild(toggle);
    var scrim = document.createElement('div');
    scrim.className = 'jsb-scrim';
    scrim.onclick = function () { document.body.classList.remove('jsb-open'); };
    document.body.appendChild(scrim);
  }

  // buildHelp adds a "❓ 가이드" button to the page header that opens this
  // page's guide modal. Also exposes JASQL.guide() for pages to call directly.
  function buildHelp() {
    var md = GUIDES[window.JASQL.page];
    if (!md) return;
    var header = document.querySelector('header');
    if (!header) return;
    if (!header.querySelector('.grow')) {
      var grow = document.createElement('span'); grow.className = 'grow'; grow.style.flex = '1';
      header.appendChild(grow);
    }
    var btn = document.createElement('button');
    btn.type = 'button'; btn.className = 'jhelp'; btn.textContent = '❓ 가이드';
    btn.onclick = function () {
      var link = (window.JASQL.pageLabel || window.JASQL.page || '') + ' 가이드';
      guideModal(link, md);
    };
    header.appendChild(btn);
  }

  function buildProfileMenu(me) {
    var header = document.querySelector('header');
    if (!header) return;
    if (!header.querySelector('.grow')) {
      var grow = document.createElement('span'); grow.className = 'grow'; grow.style.flex = '1';
      header.appendChild(grow);
    }
    var u = me.user;
    var initial = (u.display_name || u.username || '?').trim().charAt(0).toUpperCase();
    var wrap = document.createElement('div');
    wrap.className = 'jprofile';
    var isLocal = u.provider === 'local' || !u.provider;
    wrap.innerHTML =
      '<button class="javatar" type="button"><span class="jav">' + esc(initial) + '</span>' +
        '<span>' + esc(u.display_name || u.username) + '</span> <span style="opacity:.7">▾</span></button>' +
      '<div class="jmenu">' +
        '<div class="jmhead"><b>' + esc(u.display_name || u.username) + '</b>' +
          '<div class="jmsub">' + esc(u.email || u.username) + ' · ' + esc(u.role) + '</div></div>' +
        '<button class="jmitem" data-act="profile">👤 개인정보 변경</button>' +
        (isLocal ? '<button class="jmitem" data-act="password">🔒 비밀번호 변경</button>' : '') +
        '<button class="jmitem" data-act="keys">🔑 MCP 키 관리</button>' +
        '<div class="jmsep"></div>' +
        '<button class="jmitem danger" data-act="logout">⏻ 로그아웃</button>' +
        '<div class="jmfoot">sqlon v' + esc(me.version || '') + '</div>' +
      '</div>';
    header.appendChild(wrap);

    var btn = wrap.querySelector('.javatar');
    btn.onclick = function (e) { e.stopPropagation(); wrap.classList.toggle('open'); };
    document.addEventListener('click', function () { wrap.classList.remove('open'); });
    wrap.querySelector('.jmenu').addEventListener('click', function (e) { e.stopPropagation(); });
    wrap.querySelectorAll('.jmitem').forEach(function (item) {
      item.onclick = function () {
        wrap.classList.remove('open');
        var act = item.getAttribute('data-act');
        if (act === 'logout') { fetch('/auth/logout', { method: 'POST' }).then(function () { location.href = '/auth/login'; }); }
        else if (act === 'keys') { location.href = '/admin/keys'; }
        else if (act === 'profile') { openProfileModal(u); }
        else if (act === 'password') { openPasswordModal(); }
      };
    });
  }

  // ---- modals ----
  function modalShell(title, sub, bodyHTML, onSubmit) {
    var host = document.createElement('div');
    host.className = 'jmodal';
    host.innerHTML =
      '<div class="box"><h3>' + esc(title) + '</h3><p class="sub">' + esc(sub) + '</p>' +
      '<form>' + bodyHTML +
      '<div class="msg"></div>' +
      '<div class="acts"><button type="button" class="jb" data-x>취소</button>' +
      '<button type="submit" class="jb primary">저장</button></div></form></div>';
    document.body.appendChild(host);
    var close = function () { host.remove(); };
    host.addEventListener('click', function (e) { if (e.target === host) close(); });
    host.querySelector('[data-x]').onclick = close;
    var msg = host.querySelector('.msg');
    host.querySelector('form').onsubmit = function (e) {
      e.preventDefault();
      onSubmit(host, function (ok, text) {
        msg.className = 'msg ' + (ok ? 'ok' : 'bad'); msg.textContent = text || '';
        if (ok) setTimeout(close, 800);
      });
    };
    requestAnimationFrame(function () { host.classList.add('open'); });
    var f = host.querySelector('input'); if (f) f.focus();
    return host;
  }

  function openProfileModal(u) {
    modalShell('개인정보 변경', '표시 이름과 이메일을 수정합니다. 역할/비밀번호는 여기서 바뀌지 않습니다.',
      '<label>표시 이름</label><input name="display_name" value="' + esc(u.display_name || '') + '" placeholder="' + esc(u.username) + '">' +
      '<label>이메일</label><input name="email" type="email" value="' + esc(u.email || '') + '" placeholder="you@example.com">',
      function (host, done) {
        var body = JSON.stringify({
          display_name: host.querySelector('[name=display_name]').value.trim(),
          email: host.querySelector('[name=email]').value.trim(),
        });
        fetch('/auth/profile', { method: 'PUT', headers: tokenHeaders(true), body: body })
          .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
          .then(function (res) {
            if (!res.ok) return done(false, res.d.error || '저장 실패');
            done(true, '저장되었습니다');
            setTimeout(function () { location.reload(); }, 700);
          }).catch(function (e) { done(false, e.message); });
      });
  }

  function openPasswordModal() {
    modalShell('비밀번호 변경', '현재 비밀번호 확인 후 새 비밀번호로 변경합니다.',
      '<label>현재 비밀번호</label><input name="old" type="password" autocomplete="current-password">' +
      '<label>새 비밀번호</label><input name="new" type="password" autocomplete="new-password">' +
      '<label>새 비밀번호 확인</label><input name="new2" type="password" autocomplete="new-password">',
      function (host, done) {
        var np = host.querySelector('[name=new]').value, np2 = host.querySelector('[name=new2]').value;
        if (np.length < 8) return done(false, '새 비밀번호는 8자 이상이어야 합니다');
        if (np !== np2) return done(false, '새 비밀번호가 일치하지 않습니다');
        fetch('/auth/password', { method: 'PUT', headers: tokenHeaders(true),
          body: JSON.stringify({ old_password: host.querySelector('[name=old]').value, new_password: np }) })
          .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
          .then(function (res) { done(res.ok, res.ok ? '변경되었습니다' : (res.d.error || '변경 실패')); })
          .catch(function (e) { done(false, e.message); });
      });
  }

  function handleTokenBox(authed) {
    var tok = document.getElementById('adminToken');
    if (!tok) return;
    tok.style.display = 'none';
    if (authed) return; // session replaces the token in auth mode
    var header = document.querySelector('header');
    if (!header) return;
    if (!header.querySelector('.grow')) {
      var grow = document.createElement('span'); grow.className = 'grow'; grow.style.flex = '1';
      header.appendChild(grow);
    }
    var gear = document.createElement('button');
    gear.type = 'button'; gear.title = '관리 토큰 입력 (필요 시)'; gear.textContent = '🔧';
    gear.style.cssText = 'background:#1d2b47;border:1px solid #35507f;color:#cfe0ff;border-radius:8px;padding:6px 11px;cursor:pointer;font-size:14px;';
    gear.onclick = function () { tok.style.display = tok.style.display === 'none' ? '' : 'none'; if (tok.style.display === '') tok.focus(); };
    header.appendChild(gear);
  }

  window.JASQL = {
    page: null,
    guide: guideModal,
    onboarding: openOnboarding,
    async mount(opts) {
      opts = opts || {};
      this.page = opts.page || null;
      this.pageLabel = opts.label || null;
      if (opts.guide) GUIDES[this.page] = opts.guide; // page-supplied override
      injectStyles();
      var me = { auth_enabled: false };
      try { me = await (await fetch('/auth/me')).json(); } catch (e) { /* standalone fallback */ }
      if (me.auth_enabled && !me.authenticated) {
        location.href = '/auth/login?next=' + encodeURIComponent(location.pathname);
        return;
      }
      window.AUTH = me.auth_enabled && me.authenticated ? me : null;
      buildSidebar(me);
      buildHelp();
      if (me.auth_enabled && me.authenticated) buildProfileMenu(me);
      handleTokenBox(!!me.auth_enabled);
      if (typeof opts.onReady === 'function') {
        try { opts.onReady(me); } catch (e) { console.error('onReady', e); }
      }
    },
  };
})();
