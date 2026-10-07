'use strict';
/*
 * sqlon console shell: sidebar, page header extras, command palette,
 * theme, and the small behaviours every page shares.
 *
 * Every console page includes, in <head>,
 *   <link rel="stylesheet" href="/admin/ui.css"><script src="/admin/theme.js" data-shell></script>
 * and at the end of <body>
 *   <script src="/admin/nav.js"></script>
 * then calls
 *   JASQL.mount({ page: 'ask', onReady(me){ ... } })
 * where `page` is the current nav key and onReady fires after auth resolves
 * with the /auth/me payload.
 *
 * The shell adds a grouped, collapsible sidebar (icons, live badges, mini
 * mode), decorates the page's own <header> (section label, search, guide,
 * profile menu), wraps wide tables so they scroll instead of the page, and
 * opens a command palette on Ctrl/⌘+K.
 */
(function () {
  // ---------------------------------------------------------------- icons
  var P = {
    dashboard: '<rect x="3" y="3" width="7" height="9" rx="1.5"/><rect x="14" y="3" width="7" height="5" rx="1.5"/><rect x="14" y="12" width="7" height="9" rx="1.5"/><rect x="3" y="16" width="7" height="5" rx="1.5"/>',
    bell: '<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>',
    lock: '<rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>',
    activity: '<path d="M22 12h-4l-3 9L9 3l-3 9H2"/>',
    drive: '<path d="M22 12H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/><path d="M6 16h.01"/><path d="M10 16h.01"/>',
    replicate: '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
    wrench: '<path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/>',
    stethoscope: '<path d="M4.8 2.3A.3.3 0 1 0 5 2H4a2 2 0 0 0-2 2v5a6 6 0 0 0 6 6 6 6 0 0 0 6-6V4a2 2 0 0 0-2-2h-1a.2.2 0 1 0 .3.3"/><path d="M8 15v1a6 6 0 0 0 6 6 6 6 0 0 0 6-6v-4"/><circle cx="20" cy="10" r="2"/>',
    shield: '<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/>',
    clipboard: '<rect x="8" y="2" width="8" height="4" rx="1"/><path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2"/><path d="m9 14 2 2 4-4"/>',
    pr: '<circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M13 6h3a2 2 0 0 1 2 2v7"/><path d="M6 9v12"/>',
    terminal: '<path d="m4 17 6-6-6-6"/><path d="M12 19h8"/>',
    flask: '<path d="M9 3h6"/><path d="M10 3v6L4.6 18.4A1.8 1.8 0 0 0 6.2 21h11.6a1.8 1.8 0 0 0 1.6-2.6L14 9V3"/><path d="M7.5 15h9"/>',
    history: '<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l4 2"/>',
    chart: '<path d="M3 3v18h18"/><path d="M18 17V9"/><path d="M13 17V5"/><path d="M8 17v-3"/>',
    folder: '<path d="M4 20h16a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.93a2 2 0 0 1-1.66-.9l-.82-1.2A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13c0 1.1.9 2 2 2z"/>',
    table: '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18"/><path d="M3 15h18"/><path d="M12 3v18"/>',
    check: '<path d="m9 11 3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/>',
    award: '<circle cx="12" cy="8" r="6"/><path d="M15.48 12.89 17 22l-5-3-5 3 1.52-9.11"/>',
    share: '<circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><path d="m8.59 13.51 6.83 3.98"/><path d="m15.41 6.51-6.82 3.98"/>',
    layers: '<path d="m12 2 10 5-10 5L2 7l10-5z"/><path d="m2 17 10 5 10-5"/><path d="m2 12 10 5 10-5"/>',
    database: '<ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5v14a9 3 0 0 0 18 0V5"/><path d="M3 12a9 3 0 0 0 18 0"/>',
    users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>',
    sliders: '<path d="M21 4h-7"/><path d="M10 4H3"/><path d="M21 12h-9"/><path d="M8 12H3"/><path d="M21 20h-5"/><path d="M12 20H3"/><path d="M14 2v4"/><path d="M8 10v4"/><path d="M16 18v4"/>',
    key: '<circle cx="7.5" cy="15.5" r="5.5"/><path d="m21 2-9.6 9.6"/><path d="m15.5 7.5 3 3L22 7l-3-3"/>',
    book: '<path d="M2 3h6a4 4 0 0 1 4 4v14a3 3 0 0 0-3-3H2z"/><path d="M22 3h-6a4 4 0 0 0-4 4v14a3 3 0 0 1 3-3h7z"/>',
    code: '<path d="M14.5 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7.5L14.5 2z"/><path d="M14 2v6h6"/><path d="m10 13-2 2 2 2"/><path d="m14 17 2-2-2-2"/>',
    search: '<circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/>',
    sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>',
    moon: '<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9z"/>',
    monitor: '<rect x="2" y="3" width="20" height="14" rx="2"/><path d="M8 21h8"/><path d="M12 17v4"/>',
    chevron: '<path d="m6 9 6 6 6-6"/>',
    panel: '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M9 3v18"/>',
    menu: '<path d="M4 6h16"/><path d="M4 12h16"/><path d="M4 18h16"/>',
    help: '<circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/><path d="M12 17h.01"/>',
    logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5"/><path d="M21 12H9"/>',
    user: '<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>',
    external: '<path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/>',
    x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
    enter: '<path d="M9 10 4 15l5 5"/><path d="M20 4v7a4 4 0 0 1-4 4H4"/>',
    toggle: '<rect x="2" y="6" width="20" height="12" rx="6"/><circle cx="16" cy="12" r="3"/>',
    token: '<path d="M2.586 17.414A2 2 0 0 0 2 18.828V21a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h1a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h.172a2 2 0 0 0 1.414-.586l.814-.814a6.5 6.5 0 1 0-4-4z"/><circle cx="16.5" cy="7.5" r=".5" fill="currentColor"/>',
  };
  function icon(name, cls) {
    return '<svg class="' + (cls || 'ic') + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + (P[name] || '') + '</svg>';
  }

  // ------------------------------------------------------- information map
  // show: always | auth (meta DB) | admin | dba (dba/admin, or standalone)
  //       | manage (admin, or standalone)
  // On top of `show`, an admin can switch menus off or narrow them to roles
  // (메뉴 관리); /auth/me lists what that hides as `hidden_menus`. The server
  // mirrors this map in internal/mcp/menus.go and refuses hidden pages.
  var GROUPS = [
    { id: 'monitor', title: '모니터링', items: [
      { key: 'fleet', href: '/', icon: 'dashboard', label: '운영 현황', show: 'always', hint: '전체 DB 상태·위험 순위' },
      { key: 'alerts', href: '/admin/alerts', icon: 'bell', label: '예방 경보', show: 'always', badge: 'alerts', hint: '디스크 고갈 예측·스키마 이상' },
      { key: 'sessions', href: '/admin/sessions', icon: 'lock', label: '세션 · 잠금', show: 'always', hint: '활성 세션·블로킹' },
      { key: 'workload', href: '/admin/workload', icon: 'activity', label: '워크로드', show: 'always', hint: 'QPS·TPS·Top SQL' },
      { key: 'capacity', href: '/admin/workload#capacity', icon: 'drive', label: '객체 · 용량', show: 'always', hint: '테이블·인덱스 크기' },
      { key: 'availability', href: '/admin/availability', icon: 'replicate', label: '복제 · 백업', show: 'always', hint: '복제 지연·아카이브' },
    ]},
    { id: 'diagnose', title: '진단 · 점검', items: [
      { key: 'maintenance', href: '/admin/maintenance', icon: 'wrench', label: '예방 점검', show: 'always', hint: 'wraparound·블로트·WAL' },
      { key: 'dba', href: '/admin/dba', icon: 'stethoscope', label: '인시던트 · 진단', show: 'always', hint: '헬스 점검·인덱스 제안' },
      { key: 'security', href: '/admin/security', icon: 'shield', label: '보안 · 권한', show: 'always', hint: '위험 권한·설정 드리프트' },
      { key: 'compliance', href: '/admin/compliance', icon: 'clipboard', label: '컴플라이언스', show: 'always', hint: '감사·보존 정책' },
    ]},
    { id: 'change', title: '변경 · 작업', items: [
      { key: 'changes', href: '/admin/changes', icon: 'pr', label: '변경 관리', show: 'dba', badge: 'changes', hint: '승인 기반 변경계획' },
      { key: 'dba-console', href: '/admin/dba-console', icon: 'terminal', label: 'DBA 콘솔', show: 'dba', hint: '사용자·권한·설정 작업' },
    ]},
    { id: 'sql', title: 'SQL Lab', items: [
      { key: 'ask', href: '/admin/ask', icon: 'flask', label: 'SQL Lab', show: 'always', hint: '자연어 → SQL' },
      { key: 'history', href: '/admin/history', icon: 'history', label: '내 이력', show: 'auth', hint: '지난 질의·SQL' },
      { key: 'stats', href: '/admin/stats', icon: 'chart', label: '통계', show: 'auth', hint: '호출량·유효율' },
    ]},
    { id: 'meta', title: '메타데이터', items: [
      { key: 'datasets', href: '/admin', icon: 'folder', label: '데이터셋', show: 'always', hint: '카탈로그 JSON 관리' },
      { key: 'editor', href: '/admin/editor', icon: 'table', label: '테이블 편집', show: 'always', hint: '데이터셋 표 편집' },
      { key: 'reviews', href: '/admin/reviews', icon: 'check', label: '메타 검토', show: 'always', hint: '후보 승인·반려' },
      { key: 'quality', href: '/admin/quality', icon: 'award', label: '메타 품질', show: 'always', hint: '7차원 품질 점수' },
      { key: 'profcat', href: '/admin/profile-catalogs', icon: 'layers', label: '프로파일 카탈로그', show: 'always', hint: 'DB별 카탈로그' },
      { key: 'openmetadata', href: '/admin/openmetadata', icon: 'share', label: 'OpenMetadata', show: 'always', hint: '전사 카탈로그 연동' },
    ]},
    { id: 'settings', title: '설정', items: [
      { key: 'db', href: '/admin/db', icon: 'database', label: 'DB 연결', show: 'always', hint: '프로파일 등록·쿼리 콘솔' },
      { key: 'users', href: '/admin/users', icon: 'users', label: '사용자', show: 'admin', hint: '계정·역할' },
      { key: 'settings', href: '/admin/settings', icon: 'sliders', label: '서버 설정', show: 'admin', hint: '토큰·Origin·SSO' },
      { key: 'menus', href: '/admin/menus', icon: 'toggle', label: '메뉴 관리', show: 'manage', hint: '메뉴 켜기·끄기·역할별 표시' },
      { key: 'keys', href: '/admin/keys', icon: 'key', label: 'MCP 키', show: 'auth', hint: 'API 키 발급' },
    ]},
  ];

  // Per-page user guides, rendered on demand in a modal via the header
  // "❓ 가이드" button. Keyed by the page key passed to JASQL.mount({page}).
  var GUIDES = {
    sessions: '## 세션 · 잠금\n\n선택한 DB의 읽기 전용 시스템 뷰를 조회해 활성·장기 세션과 blocker→blocked 관계를 표시합니다.\n\n- **블로킹 그래프**: 세션 간 차단 흐름을 방향 그래프로 시각화합니다. 루트 블로커(빨강)=병목의 근본 원인, 중간(노랑)=차단하며 차단됨, 차단된 세션(회색). 화살표는 차단 방향, 라벨은 잠금 유형·대기시간이며 60초 이상 대기는 굵게 표시됩니다. 루트 노드에는 영향받는 세션 수가 표시되어 어떤 세션을 먼저 처리할지 즉시 판단할 수 있습니다.\n- SQL 실행시간과 트랜잭션 지속시간은 별도 열입니다.\n- Oracle은 `INST_ID:SID:SERIAL#`를 세션 키로 사용합니다.\n- SYS·SYSTEM·백그라운드·복제 세션은 보호 대상으로 표시합니다.\n- SQL 본문과 bind 값은 민감정보 보호를 위해 표시하지 않습니다.\n- 권한 부족·라이선스 정책·수집 실패는 빈 목록과 구분됩니다.\n\n> 이 화면은 관찰 전용입니다. 취소·종료는 승인된 변경계획을 통해서만 수행합니다.',
    security: '## 보안 · 권한\n\n선택한 DB의 사용자·권한 상태를 읽기 전용으로 진단합니다.\n\n- **PostgreSQL**: 로그인 가능한 비기본 SUPERUSER(치명적), RLS 우회 로그인 역할, 만료 비밀번호(`pg_roles`).\n- **MySQL/MariaDB**: SUPER/FILE/PROCESS 등 위험 권한, 와일드카드 호스트(%) 고권한 계정(`information_schema.USER_PRIVILEGES`).\n- **Oracle**: 비기본 계정의 DBA 역할(치명적), GRANT ANY 계열 위험 시스템 권한, 만료됐지만 잠기지 않은 계정(`DBA_*` 뷰).\n\n### 설정 드리프트\n프로파일에 선언한 `config_baseline`(기대 파라미터 값)과 라이브 서버 파라미터(PostgreSQL `pg_settings`, MySQL/MariaDB `global_variables`, Oracle `V$PARAMETER`)를 대조해 비인가·미기록 변경을 감지합니다. `on/off`↔`true/false`↔`1/0`은 동치로 보고, 재시작이 필요한 항목은 표시합니다. 베이스라인 미선언 시 검사하지 않습니다. 드리프트는 플릿 위험도(운영 현황)에도 경고로 반영됩니다.\n\n> 조치(권한 회수·계정 잠금·파라미터 원복)는 반드시 **변경 관리**의 승인 흐름으로 수행하세요. 이 화면은 진단만 하며 어떤 변경도 실행하지 않습니다. 모니터링 계정의 가시 범위에 따라 결과가 제한될 수 있습니다.',
    availability: '## 복제 · 백업\n\n선택한 DB의 복제 토폴로지와 백업·아카이브 상태를 읽기 전용 시스템 뷰로 관찰합니다.\n\n### 복제\n- 역할(primary/replica/standby/standalone)과 구성 요소별 상태·지연을 표시합니다.\n- PostgreSQL: standby·복제 슬롯·WAL receiver / MySQL·MariaDB: 채널별 IO/SQL 스레드 / Oracle: Data Guard lag·archive destination(base 라이선스 뷰만).\n- **lag = -1은 "측정 불가"** 이며 0(지연 없음)과 다른 상태입니다.\n\n### 백업\n- 지속 아카이빙(PITR 기반) 활성 여부: PostgreSQL archive_mode, MySQL/MariaDB binlog, Oracle ARCHIVELOG.\n- PostgreSQL WAL 아카이버 성공/실패, Oracle RMAN 최근 작업과 FRA 사용률을 표시합니다.\n- pgBackRest·XtraBackup 등 **외부 백업 도구의 잡 상태는 DB 서버가 보고하지 못하므로 포함되지 않습니다** — 별도 연동 전까지 이 화면만으로 백업이 안전하다고 판단하지 마세요.\n\n> 복제 중단·백업 실패는 플릿 위험도에도 자동 반영됩니다(운영 현황 화면).',
    workload: '## 워크로드 · 용량\n\n대상 DB의 실제 누적 시스템 카운터를 주기적으로 저장하고 이전 스냅숏과 비교해 QPS/TPS와 용량 증가량을 계산합니다.\n\n- **저장 데이터 조회**는 대상 DB를 다시 조회하지 않습니다.\n- **지금 수집**은 엔진 Provider의 고정된 읽기 전용 시스템 쿼리만 실행합니다.\n- Top SQL은 SQL 원문 없이 fingerprint/SQL ID와 통계만 표시합니다.\n- Oracle 기본 수집은 AWR/ASH/ADDM/Tuning Advisor를 사용하지 않습니다.\n- 첫 스냅숏에는 비교 기준이 없어 변화율이 표시되지 않습니다.\n\n> 권한 부족, 부분 수집, 보존 상한은 limitation으로 표시되며 정상 데이터로 숨기지 않습니다.',
    ask: '## SQL Lab\n\n자연어 질문으로 SQL을 만들고 실행합니다.\n\n1. 질문을 입력하면 **prepare_sql_context**가 테이블·컬럼·조인·시간조건·검증 힌트를 한 번에 묶어 줍니다.\n2. 응답이 **재질문(needs_clarification)** 이면 제시된 질문에 답한 뒤 다시 실행하세요.\n3. `ready`가 되면 스켈레톤의 `/* SLOT */`만 채워 SQL을 완성합니다.\n4. **검증 → 실행계획 → 실행** 순서로 진행합니다.\n\n> 팁: 특정 DB 프로파일의 카탈로그로 질의하려면 profile을 지정하세요(멀티 DB).',
    datasets: '## 데이터셋 관리\n\n이 서버는 Text2SQL 정확도를 위해 **18개의 JSON 데이터셋**을 참조합니다.\n\n### 1. 데이터셋 이해하기 (목록 뱃지)\n- **필수** — 물리/논리 모델. 서버 기동에 필수라 제거 불가(교체는 가능)\n- **선택** — 없으면 해당 기능 축소 또는 내장 기본값 사용\n- **편집가능** — 이 화면에서 교체/제거 가능\n- **시스템** — feedback/audit. 서버가 자동 기록, 조회만 가능\n- **이슈 N** — 이 파일의 로드 오류/경고 수(상세는 행 클릭)\n\n### 2. 내용 확인\n- 행 클릭 → 용도·기대 스키마·사용 도구·내용 샘플 표시\n- **[현재 내용 불러오기]** → 파일 전체를 편집기에 로드\n- 각 데이터셋의 "기대 스키마"가 곧 작성 규칙입니다\n\n### 3. 넣기/바꾸기 (교체)\n편집기 내용이 **파일의 새 전체 내용**이 됩니다(부분 병합 아님).\n- **[① JSON 검사]** — 문법·형식 사전 확인\n- **[② 적용]** — 자동으로 백업 → 저장 → 재컴파일 → **핫스왑**(재기동 불필요)\n- 컴파일 실패/신규 오류 시 **자동 롤백**. 알고도 적용하려면 **강제 적용** 체크\n\n### 4. 빼기/되돌리기\n- **[데이터셋 제거]** — 백업 후 삭제+핫스왑(필수·시스템은 거부)\n- 모든 변경 전 상태는 **백업/복원** 섹션에 남고, 복원 직전 파일도 재백업되어 복원도 되돌릴 수 있습니다\n\n### 5. 파일을 직접 수정했다면\n볼륨/SSH로 파일을 직접 바꾼 경우 **[🔄 카탈로그 리로드]**로 재컴파일하세요. 실패 시 이전 카탈로그 유지.\n\n### 6. 보안\n`-admin-token`(또는 환경변수) 설정 시 변경 작업에 토큰 필요 — 상단 입력란에 넣으면 자동 전송됩니다.\n\n### 7. MCP 도구 대응\n화면 작업은 MCP 도구와 동일 코드로 실행됩니다: 목록 `list_datasets`, 상세 `get_dataset`, 교체 `put_dataset`, 제거 `remove_dataset`, 리로드 `reload_catalog` (REST `GET/PUT/DELETE /api/datasets`, `POST /api/reload`).',
    editor: '## 테이블 편집\n\n데이터셋을 표(그리드)로 편집하고 **[저장]** 하면 즉시 반영됩니다.\n\n- **셀 수정**: 셀 클릭 → 입력 → `Enter` 확정 / `Esc` 취소. 숫자·true/false·null·JSON(배열/객체) 타입 자동 보존 (예: `["별칭1","별칭2"]`는 배열로 저장)\n- **행 추가**: `[+ 행 추가]` — 빈 행이 맨 위에 생성. 행 번호 옆 `⧉` 복제 / `✕` 삭제\n- **컬럼 추가/이름변경/삭제**: `[+ 컬럼 추가]`, 머리글 호버 시 `✎`(모든 행의 키 변경) / `✕`(모든 행에서 키 제거)\n- **빈 셀**: 저장 시 해당 키를 넣지 않음(빈 문자열이 필요하면 `""` 입력)\n- **저장**: 표 전체가 파일의 새 내용이 됨. 서버가 **백업 → 컴파일 검증 → 핫스왑**, 문제 시 **자동 롤백**. 되돌리기는 데이터셋 화면의 백업/복원 사용\n- **검색**: 일치 행만 표시하되 편집·저장은 전체 데이터 기준\n\n> 중첩이 깊은 `overrides`와 시스템(feedback/audit) 데이터셋은 이 화면에서 제외 — 데이터셋 화면(JSON 콘솔)을 사용하세요.',
    db: '## DB 연결\n\nDB 접속 **프로파일**(postgres/mysql/mariadb)을 등록·테스트하고, 콘솔에서 Read-Only 쿼리를 실행합니다.\n\n### 프로파일 등록\n- **접속 문자열**: `host:port/dbname` 형식. postgres/mysql/mariadb 지원.\n- **비밀번호**: 평문 저장 금지 — `env:변수명`(권장) 또는 `file:/run/secrets/파일`. `plain:값`은 개발용.\n- **계정**: SELECT 권한만 가진 전용 계정을 사용하세요 — 서버 차단과 별개로 DB 권한이 최종 방어선입니다.\n\n### 안전장치\n- SELECT/WITH만 허용, DML/DDL/트랜잭션·다중 statement 차단\n- 쿼리 타임아웃, `max_rows` 제한(초과 시 truncated), 응답 바이트 캡\n- PII 값 마스킹, 연속 실패 시 서킷브레이커, 전 실행 감사 로그(`audit/query-*.jsonl`)\n- 실행 전 **실행계획 게이트** — 위험(full scan/카티션/예상행 과다) 쿼리는 승인 전 차단\n- 프로파일 정책 **비용 상한**(max_plan_cost/rows) 초과 쿼리는 하드 차단\n\n### 실행 순서\n프로파일 등록 → **[접속 테스트]** 초록 확인 → ①**[검증]** → ②**[실행계획]**(실제 EXPLAIN, high면 조건 보강) → ③**[미리보기]** → ④필요 시 **[실행]**. 오래 걸리면 실행 중 목록에서 **[취소]**.\n\n> 실행 성공 응답의 🩺 SQL 린트는 권고이며 실행을 막지 않습니다.',
    reviews: '## 메타 검토\n\n규칙 엔진·OpenMetadata가 만든 **후보**(논리명·의미타입·설명·코드사전·지표·관계)를 승인/반려합니다.\n\n1. 상태(대기/승인/반려)·종류로 필터해 검토합니다.\n2. 행별 또는 일괄로 승인/반려하고 검토자·메모를 남깁니다.\n3. **승인분 반영 + 리로드**로 overrides/metrics/relations에 병합합니다(백업·기존값 보존).',
    quality: '## 메타 품질\n\n테이블별 메타데이터 품질을 7차원(완전성·일관성·관계성·프로파일링·지표연결·사용성·보안성)으로 채점하고 A–E 등급·릴리스 게이트를 보여줍니다.\n\n- **게이트 차단** 항목(지표/조인 손상, PII 미분류, 품질 하한 미달)을 먼저 해소하세요.\n- 하단에서 **감사 로그 해시 체인 무결성**을 검증할 수 있습니다.',
    openmetadata: '## OpenMetadata 연동\n\n전사 카탈로그 OpenMetadata와 양방향 연동합니다.\n\n- **연결 설정**: URL/토큰을 저장(무재기동, `<data>/openmetadata.json`).\n- **Import**: 설명·PII·용어집을 **빈 필드에만** 후보로 가져오기(미리보기 → 반영).\n- **Export**: jamypg 설명을 OM의 빈 컬럼에 push.\n- **Drift**: 두 카탈로그의 불일치(gap/conflict) 대조.',
    profcat: '## 프로파일 카탈로그\n\n등록된 DB 프로파일마다 **독립 카탈로그 워크스페이스**(`<data>/profiles/<profile>/`)를 관리합니다.\n\n1. **라이브 DB로 구축/갱신** — 물리 모델을 워크스페이스에 수집(기존 설명 보존).\n2. **전체 구축** — 모든 프로파일 일괄 구축.\n3. 데이터셋 JSON을 조회·편집(검증·백업·롤백).\n4. **활성 카탈로그로 전환** — 무재기동 핫스왑(단독 모드).\n\n> 요청 단위로 `profile`을 지정하면 전역 전환 없이 그 워크스페이스로 질의·검증됩니다(멀티 DB).',
    dba: '## 인시던트 · 진단\n\n연결된 DB와 감사 로그를 근거로 한 **읽기 전용 DBA 진단** 도구 모음입니다. 어떤 것도 자동으로 실행·변경하지 않으며, 결과는 검토용 권고입니다.\n\n- **헬스 점검** — 시스템 카탈로그를 읽어 PK 없는 테이블·미인덱스 FK·미사용 인덱스·오래된 통계·대형 테이블을 진단(엔진별 지원 범위 상이).\n- **인덱스 제안** — 감사 로그의 느린 쿼리에서 인덱스 없는 필터/조인/정렬 컬럼을 집계해 `CREATE INDEX` 후보를 영향도 순으로 제시(직접 검토 후 수행).\n- **워크로드** — 기간별 쿼리량·오류율·지연 분포·핫 테이블·피크 시간대 리포트.\n- **SQL 린트** — 단일 문장의 안티패턴(SELECT * , 선두 와일드카드 LIKE, 비-sargable 조건 등) 정적 진단.\n- **자연어 설명** — SQL이 무엇을 하는지 카탈로그 논리명으로 한국어 요약.\n\n> 상단에서 DB 프로파일을 선택하세요. 인덱스/워크로드는 프로파일을 비우면 전체 로그를 대상으로 합니다.',
    changes: '## 변경 관리 (dba/admin 전용)\n\nSQLON의 승인 기반 변경 통제 워크스페이스입니다. 모든 쓰기 작업은 **변경계획(ChangePlan)** 으로 구조화되어 `초안 → 제출 → 승인 → 실행 → 검증` 흐름을 따르며, 실패 시 승인된 **보상(롤백) 작업**을 역순으로 실행합니다.\n\n### 핵심 규칙\n- 각 단계(step)는 실행 문장 + 사후 검증 문장 + 보상 문장이 **모두 필수**입니다.\n- 필요 승인 수는 서버 정책이 결정합니다: 낮음 0 · 중간/높음 1 · 치명적 2(서로 다른 승인자) · 비상 1.\n- 실행은 승인 ID(`X-Approval-ID`)를 요구하며 실행 직전 재검증을 거칩니다.\n- 계획·승인·실행 결과는 재시작 후에도 유지되고(디스크 영속화), 취소된 계획도 이력으로 보존됩니다.\n- AI와 MCP 클라이언트도 동일한 서비스 계층을 사용하므로 이 화면과 정책이 갈라지지 않습니다.\n\n> 문서: docs/change-control.md',
    'dba-console': '## DBA 콘솔 (dba/admin 전용)\n\n권한 있는(쓰기 가능) DBA 세션으로 실제 DB를 관리합니다. 읽기 전용 쿼리 계정과 **분리된 커넥션**을 사용하며, 모든 변경은 감사 로그(`dba:*`)에 기록됩니다.\n\n### 사전 준비 — 프로파일에 DBA 자격증명\n**DB 연결** 화면에서 프로파일을 편집해 DBA 블록을 설정해야 콘솔이 동작합니다:\n- `dba.enabled` = true\n- `dba.username` / `dba.password_ref` — 권한 있는 계정(예: postgres superuser 또는 CREATEROLE/CREATEDB 보유 롤). `password_ref`는 `env:이름` 또는 `file:경로` 권장\n- `dba.connect_string`(선택) — postgres에서 `CREATE/DROP DATABASE`를 사용자 DB 안에서 실행하지 않도록 `postgres` 관리 DB로 지정\n\n### 탭\n- **개요** — 방언·서버 버전·역할/DB 수\n- **사용자·역할** — 생성/속성·비밀번호 변경/삭제 (postgres LOGIN·SUPERUSER·CREATEDB·CREATEROLE)\n- **데이터베이스** — 생성/삭제(소유자·인코딩)\n- **권한** — GRANT/REVOKE (`WITH GRANT OPTION`)\n- **설정** — 조회 및 변경(postgres `ALTER SYSTEM`+reload, mysql `SET GLOBAL/SESSION`)\n- **세션** — 활성 세션 조회, 쿼리 취소/세션 종료\n- **유지보수** — VACUUM/ANALYZE/REINDEX (mysql ANALYZE/OPTIMIZE)\n- **SQL 콘솔** — 구조화 도구로 안 되는 작업을 위한 임의 권한 SQL 실행(확인 체크 필요, 원문 감사)\n\n> 삭제(사용자/DB)와 SQL 콘솔은 되돌릴 수 없습니다. 최소 권한 원칙에 따라 DBA 계정 권한을 필요한 만큼만 부여하세요.',
    stats: '## 통계\n\nMCP 활동량과 SQL 유효율, 실행 상태, 최근 추이를 확인합니다.\n\n- 관리자는 **전체 사용자** 집계와 사용자별 호출량을 볼 수 있습니다.\n- 유효율이 낮으면 **내 이력**에서 invalid 생성을 확인하고, 재질문 학습 제안에 따라 지표·용어 사전을 보강하세요.\n- 서버 운영 지표는 `/metrics`의 Prometheus 형식으로도 제공됩니다.',
    history: '## 내 이력\n\n프롬프트·SQL·프로파일·도구명으로 과거 활동을 검색합니다.\n\n- **질의로 다시 사용**: 과거 프롬프트를 질의 화면에 채웁니다.\n- **DB 콘솔로 보내기**: 과거 SQL과 프로파일을 DB 콘솔로 전달합니다.\n- 관리자는 **전체 사용자** 범위로 전환할 수 있습니다.\n\n> 실행 실패와 invalid 생성을 검색해 반복 오류를 찾고, 통계 화면의 품질 신호와 함께 확인하세요.',
    users: '## 사용자 (관리자)\n\n로컬 계정·역할을 관리합니다. admin 역할이 전체 관리 권한을 가집니다.',
    keys: '## MCP 키\n\nMCP 클라이언트 인증용 API 키(ssk_...)를 발급·회전·폐기합니다.',
    fleet: '## 운영 현황\n\n등록된 모든 DB의 연결·수집 상태를 위험도 순으로 보여주는 첫 화면입니다.\n\n- **요약 카드**: 전체·정상·주의·위험·수집 실패 인스턴스 수와 평균 건강점수.\n- **위험 순위 표**: 위험 점수가 높은 DB가 위에 옵니다. 상태(정상/주의/위험/수집 실패), 등급, 응답 시간, 수집 시각과 **근거·제한**(왜 그 점수인지)을 함께 봅니다.\n- **지원 기능**: 엔진이 제공하는 관측 기능 수와 미지원 기능. 미지원 항목은 해당 화면에서 "지원하지 않음"으로 표시됩니다.\n- 위험한 DB를 찾으면 왼쪽 메뉴의 **예방 경보 · 세션 · 워크로드 · 예방 점검**으로 이어서 원인을 확인하세요.\n\n> 외부 백업 도구(pgBackRest·XtraBackup 등)의 잡 상태는 DB 서버가 보고하지 않으므로 위험도에 포함되지 않습니다.',
    alerts: '## 예방 경보\n\n디스크가 차서 DB가 멈추기 **전에** 신호를 잡아 알립니다. 1분마다 수집된 관측 결과를 평가합니다.\n\n- **원인 추정**: 함께 발생한 경보를 원인→결과로 묶어 고칠 것 하나를 보여줍니다(예: 버려진 복제 슬롯 → pg_wal 과다 → 고갈 예측).\n- **저장공간 예측**: 사용률, 6시간·7일 추세, 가득 차는 시점과 **증가 원인**(어떤 테이블·WAL이 늘었는지). 한도가 없으면 DB 연결 화면에서 "저장공간 한도"를 입력하세요.\n- **발생 중 경보**: 확인(ack)하면 재알림과 당직 호출이 멈춥니다. 상세에서 승인 게이트를 거치는 **수정안**을 만들 수 있습니다.\n- **무음**: 볼륨 증설·마이그레이션 같은 계획 작업 동안 DB·규칙 단위로 알림만 멈춥니다.\n- **스키마 변경 이력**: 변경계획·배포 마이그레이션(Flyway 등)과 대조해 계획 외 DDL을 표시합니다.\n- **설정**(관리자): 알림 채널, 당직 호출, 생존 신호, Mattermost 버튼을 재시작 없이 바꿉니다.\n\n> 상단 사이드바의 예방 경보 배지는 아직 아무도 확인하지 않은 긴급·경고 경보 수입니다.',
    maintenance: '## 예방 점검\n\n오류 없이 잠복하다 장애를 일으키는 위험을 미리 찾습니다 — 트랜잭션 ID **wraparound**, 테이블 **블로트**, WAL을 붙잡는 **복제 슬롯**, 실패하는 **WAL 아카이브**, VACUUM을 막는 **장기 트랜잭션** 등.\n\n- 위험은 치명적 → 경고 순으로 정렬됩니다. 탭으로 걸러 보세요.\n- 같은 점검 결과가 **예방 경보**로 이어져 알림이 갑니다.\n- 조치는 반드시 **변경 관리**의 승인 흐름으로 수행합니다. 이 화면은 읽기 전용입니다.',
    compliance: '## 컴플라이언스\n\n읽기 전용 진단 결과를 ISMS-P · PCI-DSS · 개인정보보호법 통제 항목에 매핑한 **참고 리포트**입니다. 공식 인증 심사를 대체하지 않습니다.\n\n- 항목마다 통과 / 미준수 / 수동 확인으로 표시하고 근거를 함께 보여줍니다.\n- **리포트 저장/인쇄**로 감사 자료를 만들 수 있습니다.\n- 미준수 항목의 조치는 변경 관리로 수행하세요.',
    settings: '## 서버 설정 (관리자)\n\n마스터 토큰·허용 Origin·Keycloak SSO를 메타 DB에 저장하고 즉시 적용합니다.',
    menus: '## 메뉴 관리 (관리자)\n\n콘솔 메뉴를 켜고 끄거나, 메뉴를 볼 수 있는 역할을 좁힙니다. 저장하면 재시작 없이 바로 적용됩니다.\n\n- **끈 메뉴**는 모든 사람의 사이드바와 빠른 이동(Ctrl+K)에서 사라지고, 화면 주소로 열어도 열 수 있는 첫 화면으로 돌아가며 이유를 안내합니다.\n- **역할**(로그인 모드): 체크한 역할에게만 보입니다. 메뉴의 기본 권한보다 넓힐 수는 없습니다(예: 사용자 관리는 관리자 전용).\n- **메뉴 관리**는 끌 수 없습니다 — 되돌릴 길이 사라지지 않도록.\n- 위쪽 숫자는 저장 전에도 각 역할이 보게 될 메뉴 수를 미리 보여줍니다.\n\n> 메뉴 설정은 콘솔 화면만 바꿉니다. REST API와 MCP 도구 권한은 역할과 MCP 키로 통제됩니다.\n\n설정은 `<data>/operations/console/menus.json` 에 저장되고 변경은 감사 로그(`admin:console_menus_update`)에 남습니다.',
  };

  // ---------------------------------------------------------------- utils
  var esc = function (s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); };
  var store = {
    get: function (k, d) { try { var v = localStorage.getItem(k); return v == null ? d : v; } catch (e) { return d; } },
    set: function (k, v) { try { localStorage.setItem(k, v); } catch (e) { /* private mode */ } },
  };
  var root = document.documentElement;
  function el(tag, cls, html) { var n = document.createElement(tag); if (cls) n.className = cls; if (html != null) n.innerHTML = html; return n; }
  function typing(e) {
    var t = e.target;
    return t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
  }

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

  // ---------------------------------------------------------------- modals
  // Every shell dialog: scrim click and Esc close it, focus moves in and
  // returns to whatever opened it.
  function openDialog(host, onClose) {
    var before = document.activeElement;
    document.body.appendChild(host);
    var close = function () {
      host.remove();
      document.removeEventListener('keydown', onKey, true);
      if (onClose) onClose();
      if (before && before.focus) before.focus();
    };
    var onKey = function (e) { if (e.key === 'Escape') { e.preventDefault(); close(); } };
    document.addEventListener('keydown', onKey, true);
    host.addEventListener('mousedown', function (e) { if (e.target === host) close(); });
    requestAnimationFrame(function () { host.classList.add('open'); });
    return close;
  }

  function guideModal(title, md) {
    var host = el('div', 'jmodal', '<div class="box guidebox" role="dialog" aria-modal="true" aria-label="' + esc(title) + '">' +
      '<div class="ghead"><h3>' + esc(title) + '</h3><button type="button" class="jb icon-only" data-x aria-label="닫기">' + icon('x') + '</button></div>' +
      '<div class="gbody">' + mdToHtml(md) + '</div></div>');
    var close = openDialog(host);
    host.querySelector('[data-x]').onclick = close;
    host.querySelector('[data-x]').focus();
    return host;
  }

  function openOnboarding() {
    var host = guideModal('sqlon 온보딩 가이드', '불러오는 중…');
    fetch('/admin/onboarding.md').then(function (r) { return r.text(); }).then(function (md) {
      host.querySelector('.gbody').innerHTML = mdToHtml(md);
    }).catch(function () {
      host.querySelector('.gbody').innerHTML = '<p>온보딩 문서를 불러오지 못했습니다.</p>';
    });
  }

  function tokenHeaders(json) {
    var h = {};
    if (json) h['Content-Type'] = 'application/json';
    var tok = document.getElementById('adminToken');
    if (tok && tok.value) h['X-Admin-Token'] = tok.value;
    return h;
  }

  function modalShell(title, sub, bodyHTML, onSubmit) {
    var host = el('div', 'jmodal',
      '<div class="box" role="dialog" aria-modal="true" aria-label="' + esc(title) + '"><h3>' + esc(title) + '</h3><p class="sub">' + esc(sub) + '</p>' +
      '<form>' + bodyHTML + '<div class="msg" role="status"></div>' +
      '<div class="acts"><button type="button" class="jb" data-x>취소</button>' +
      '<button type="submit" class="jb primary">저장</button></div></form></div>');
    var close = openDialog(host);
    host.querySelector('[data-x]').onclick = close;
    var msg = host.querySelector('.msg');
    host.querySelector('form').onsubmit = function (e) {
      e.preventDefault();
      onSubmit(host, function (ok, text) {
        msg.className = 'msg ' + (ok ? 'ok' : 'bad'); msg.textContent = text || '';
        if (ok) setTimeout(close, 800);
      });
    };
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

  // ---------------------------------------------------------------- theme
  function themePref() { return store.get('sqlon-theme', 'system'); }
  function applyTheme() {
    var pref = themePref();
    var dark = pref === 'dark' || (pref === 'system' && window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches);
    root.setAttribute('data-theme', dark ? 'dark' : 'light');
    document.querySelectorAll('[data-theme-label]').forEach(function (n) {
      n.innerHTML = icon(pref === 'dark' ? 'moon' : pref === 'light' ? 'sun' : 'monitor') + '<span class="lbl">' + ({ dark: '다크', light: '라이트', system: '시스템' })[pref] + ' 테마</span>';
    });
    try { window.dispatchEvent(new CustomEvent('sqlon:theme', { detail: { dark: dark } })); } catch (e) { /* old browsers */ }
  }
  function cycleTheme() {
    var order = ['system', 'light', 'dark'];
    store.set('sqlon-theme', order[(order.indexOf(themePref()) + 1) % order.length]);
    applyTheme();
  }
  if (window.matchMedia) {
    var mq = matchMedia('(prefers-color-scheme: dark)');
    var onScheme = function () { if (themePref() === 'system') applyTheme(); };
    if (mq.addEventListener) mq.addEventListener('change', onScheme); else if (mq.addListener) mq.addListener(onScheme);
  }

  // ---------------------------------------------------------------- sidebar
  var visible = []; // flattened items the user may open, for the palette
  var shellMe = null;
  function canShow(rule, me) {
    var authed = !!(me && me.auth_enabled);
    var role = (authed && me.authenticated && me.user && me.user.role) || '';
    if (rule === 'dba') return role === 'admin' || role === 'dba' || !authed;
    if (rule === 'manage') return role === 'admin' || !authed;
    return rule === 'always' || (rule === 'auth' && authed) || (rule === 'admin' && role === 'admin');
  }
  // switched off in 메뉴 관리 (for everyone, or for this user's role)
  function isHidden(key, me) { return ((me && me.hidden_menus) || []).indexOf(key) >= 0; }
  function menuLabel(key) {
    for (var i = 0; i < GROUPS.length; i++) {
      for (var j = 0; j < GROUPS[i].items.length; j++) {
        if (GROUPS[i].items[j].key === key) return GROUPS[i].items[j].label;
      }
    }
    return key;
  }
  function currentGroup() {
    for (var i = 0; i < GROUPS.length; i++) {
      for (var j = 0; j < GROUPS[i].items.length; j++) {
        if (GROUPS[i].items[j].key === window.JASQL.page) return GROUPS[i];
      }
    }
    return null;
  }

  function buildSidebar(me) {
    var cur = window.JASQL.page;
    // Groups the user opened or closed are remembered; otherwise only
    // monitoring and the current page's group start open, so the menu fits.
    var openSet = (store.get('sqlon-nav-open', 'monitor') || '').split(',').filter(Boolean);
    var curGroup = currentGroup();
    var aside = el('aside', 'jsb');
    aside.setAttribute('aria-label', '주 메뉴');
    var html = '<div class="jsb-top"><a class="jbrand" href="/" title="운영 현황"><img src="/admin/logo-transparent.png" alt="" width="26" height="26"><span class="lbl">sqlon</span></a>' +
      '<button type="button" class="jsb-mini" data-mini title="사이드바 접기/펼치기" aria-label="사이드바 접기/펼치기">' + icon('panel') + '</button></div>' +
      '<button type="button" class="jsb-search" data-palette title="빠른 이동 (Ctrl+K)">' + icon('search') + '<span class="lbl">빠른 이동</span><kbd class="lbl">Ctrl K</kbd></button>' +
      '<nav>';
    visible = [];
    GROUPS.forEach(function (g) {
      var items = g.items.filter(function (it) { return canShow(it.show, me) && !isHidden(it.key, me); });
      if (!items.length) return;
      var open = (curGroup && curGroup.id === g.id) || openSet.indexOf(g.id) >= 0;
      html += '<div class="jgrp' + (open ? '' : ' closed') + '" data-grp="' + g.id + '">' +
        '<button type="button" class="jgrp-h" aria-expanded="' + open + '">' + '<span>' + esc(g.title) + '</span>' + icon('chevron', 'ic chev') + '</button><div class="jgrp-items">';
      items.forEach(function (it) {
        visible.push({ key: it.key, href: it.href, label: it.label, icon: it.icon, group: g.title, hint: it.hint || '' });
        var active = it.key === cur;
        html += '<a class="jlink' + (active ? ' active' : '') + '" href="' + it.href + '"' + (active ? ' aria-current="page"' : '') + ' title="' + esc(it.label) + '">' +
          icon(it.icon) + '<span class="lbl">' + esc(it.label) + '</span>' + (it.badge ? '<span class="jbadge" data-badge="' + it.badge + '" hidden></span>' : '') + '</a>';
      });
      html += '</div></div>';
    });
    html += '</nav><div class="jsb-foot">' +
      '<a class="jlink" href="#" data-onboarding title="온보딩 가이드">' + icon('book') + '<span class="lbl">온보딩 가이드</span></a>' +
      '<a class="jlink" href="/docs" target="_blank" rel="noopener" title="API 문서">' + icon('code') + '<span class="lbl">API 문서</span>' + icon('external', 'ic ext') + '</a>' +
      '<button type="button" class="jlink" data-theme-toggle data-theme-label title="테마 바꾸기"></button>' +
      '<div class="jver lbl">sqlon ' + esc(me && me.version ? 'v' + me.version : '') + (me && me.auth_enabled ? '' : ' · 단독 모드') + '</div>' +
      '</div>';
    aside.innerHTML = html;
    document.body.insertBefore(aside, document.body.firstChild);
    var main = document.querySelector('main');
    if (main) {
      if (!main.id) main.id = 'main';
      if (!main.hasAttribute('tabindex')) main.setAttribute('tabindex', '-1');
      var skip = el('a', 'ui-skip', '본문으로 건너뛰기');
      skip.href = '#' + main.id;
      document.body.insertBefore(skip, aside);
    }

    visible.push({ key: '_onboarding', label: '온보딩 가이드', icon: 'book', group: '문서', hint: '처음 쓰는 분을 위한 안내', run: openOnboarding });
    visible.push({ key: '_docs', href: '/docs', label: 'API 문서', icon: 'code', group: '문서', hint: 'REST·MCP 레퍼런스', target: '_blank' });
    visible.push({ key: '_theme', label: '테마 바꾸기', icon: 'moon', group: '화면', hint: '시스템 → 라이트 → 다크', run: cycleTheme });

    aside.querySelectorAll('.jgrp-h').forEach(function (b) {
      b.onclick = function () {
        var g = b.parentNode;
        var nowClosed = !g.classList.contains('closed');
        g.classList.toggle('closed', nowClosed);
        b.setAttribute('aria-expanded', String(!nowClosed));
        var list = (store.get('sqlon-nav-open', 'monitor') || '').split(',').filter(Boolean).filter(function (x) { return x !== g.dataset.grp; });
        if (!nowClosed) list.push(g.dataset.grp);
        store.set('sqlon-nav-open', list.join(','));
      };
    });
    aside.querySelector('[data-mini]').onclick = function () {
      var mini = !root.classList.contains('sb-mini');
      root.classList.toggle('sb-mini', mini);
      store.set('sqlon-sidebar', mini ? 'mini' : 'full');
    };
    aside.querySelector('[data-palette]').onclick = openPalette;
    aside.querySelector('[data-theme-toggle]').onclick = cycleTheme;
    aside.querySelector('[data-onboarding]').onclick = function (e) { e.preventDefault(); closeMobile(); openOnboarding(); };
    var act = aside.querySelector('.jlink.active');
    if (act) requestAnimationFrame(function () {
      var nav = aside.querySelector('nav');
      var top = act.offsetTop - nav.offsetTop;
      if (top + act.offsetHeight > nav.scrollTop + nav.clientHeight || top < nav.scrollTop) nav.scrollTop = Math.max(0, top - nav.clientHeight / 2);
    });

    // small screens: a menu button and a scrim
    var toggle = el('button', 'jsb-toggle', icon('menu'));
    toggle.type = 'button';
    toggle.setAttribute('aria-label', '메뉴 열기');
    toggle.onclick = function () { document.body.classList.toggle('jsb-open'); };
    document.body.appendChild(toggle);
    var scrim = el('div', 'jsb-scrim');
    scrim.onclick = closeMobile;
    document.body.appendChild(scrim);
    applyTheme();
  }
  function closeMobile() { document.body.classList.remove('jsb-open'); }

  // Redraw the sidebar after the menu switches change (메뉴 관리 saves).
  function rebuildNav(hidden) {
    if (!shellMe) return;
    shellMe.hidden_menus = hidden || [];
    document.querySelectorAll('body > .jsb, body > .ui-skip, body > .jsb-toggle, body > .jsb-scrim').forEach(function (n) { n.remove(); });
    buildSidebar(shellMe);
    refreshBadges();
  }

  // A hidden page sends the viewer to the first page they can open with
  // ?menu_off=<key>; say which page was refused and why, then tidy the URL.
  function menuOffNotice() {
    var m = location.search.match(/[?&]menu_off=([^&#]+)/);
    if (!m) return;
    var label = menuLabel(decodeURIComponent(m[1]));
    var main = document.querySelector('main');
    if (main) {
      var box = el('div', 'ui-notice', icon('toggle') + '<div><b>‘' + esc(label) + '’ 화면은 꺼져 있습니다.</b>' +
        '<span>관리자가 메뉴 관리에서 이 메뉴를 껐거나 볼 수 있는 역할에서 뺐습니다. 열 수 있는 첫 화면으로 이동했습니다.</span></div>' +
        '<button type="button" class="ui-notice-x" aria-label="안내 닫기">' + icon('x') + '</button>');
      box.setAttribute('role', 'status');
      box.querySelector('button').onclick = function () { box.remove(); };
      main.insertBefore(box, main.firstChild);
    }
    try {
      var u = new URL(location.href);
      u.searchParams.delete('menu_off');
      history.replaceState(history.state, '', u.pathname + u.search + u.hash);
    } catch (e) { /* old browsers keep the query */ }
  }

  // ------------------------------------------------------------ badges
  var baseTitle = '';
  function refreshBadges() {
    fetch('/api/console/summary', { headers: tokenHeaders(false) }).then(function (r) { return r.ok ? r.json() : null; }).then(function (d) {
      if (!d) return;
      var a = d.alerts || {};
      if (!baseTitle) baseTitle = document.title.replace(/^\(긴급 \d+\) /, '');
      document.title = (a.critical ? '(긴급 ' + a.critical + ') ' : '') + baseTitle;
      setBadge('alerts', a.critical ? a.critical : a.warning, a.critical ? 'bad' : 'warn',
        (a.critical ? '긴급 ' + a.critical + '건' : '') + (a.critical && a.warning ? ' · ' : '') + (a.warning ? '경고 ' + a.warning + '건' : '') + ' — 확인되지 않음');
      var c = d.changes || {};
      setBadge('changes', c.awaiting_approval, 'info', '승인 대기 ' + (c.awaiting_approval || 0) + '건');
    }).catch(function () { /* badges are a hint; never break the page */ });
  }
  function setBadge(name, n, tone, title) {
    document.querySelectorAll('[data-badge="' + name + '"]').forEach(function (b) {
      if (!n) { b.hidden = true; return; }
      b.hidden = false;
      b.textContent = n > 99 ? '99+' : String(n);
      b.className = 'jbadge ' + tone;
      b.title = title;
      b.parentNode.setAttribute('aria-label', b.parentNode.textContent.replace(b.textContent, '').trim() + ', ' + title);
    });
  }

  // --------------------------------------------------- header decoration
  function headerEl() { return document.querySelector('body > header, main > header, header'); }
  function ensureGrow(header) {
    var grow = header.querySelector(':scope > .grow');
    if (!grow) { grow = el('span', 'grow'); header.appendChild(grow); }
    return grow;
  }
  function decorateHeader(me) {
    var header = headerEl();
    if (!header) return;
    header.classList.add('ui-header');
    // section label above the title: where am I?
    var h1 = header.querySelector('h1');
    var g = currentGroup();
    if (h1 && g && !header.querySelector('.ui-crumb') && h1.textContent.trim() !== g.title) {
      var crumb = el('div', 'ui-crumb', esc(g.title));
      if (h1.parentNode === header) {
        // h1 sits directly in the flex row: give it a title block
        var box = el('div', 'ui-title');
        header.insertBefore(box, h1);
        box.appendChild(crumb);
        box.appendChild(h1);
      } else {
        h1.parentNode.insertBefore(crumb, h1);
      }
    }
    ensureGrow(header);
    var tools = el('div', 'ui-tools');
    header.appendChild(tools);
    var search = el('button', 'ui-tool', icon('search') + '<span class="lbl">검색</span><kbd>Ctrl K</kbd>');
    search.type = 'button'; search.title = '빠른 이동 (Ctrl+K)';
    search.onclick = openPalette;
    tools.appendChild(search);
    var md = GUIDES[window.JASQL.page];
    if (md) {
      var help = el('button', 'ui-tool', icon('help') + '<span class="lbl">가이드</span>');
      help.type = 'button'; help.title = '이 화면 사용법 (?)';
      help.onclick = function () { guideModal((window.JASQL.pageLabel || (h1 ? h1.textContent : '') || '') + ' 가이드', md); };
      tools.appendChild(help);
    }
    handleTokenBox(header, tools, !!me.auth_enabled);
    if (me.auth_enabled && me.authenticated) buildProfileMenu(tools, me);
  }

  function handleTokenBox(header, tools, authed) {
    var tok = document.getElementById('adminToken');
    if (!tok) return;
    tok.hidden = true;
    if (authed) return; // the session replaces the token in auth mode
    var b = el('button', 'ui-tool' + (tok.value ? ' set' : ''), icon('token') + '<span class="lbl">관리 토큰</span>');
    b.type = 'button'; b.title = '관리 토큰 입력 (단독 모드에서 변경 작업에 필요)';
    b.onclick = function () {
      tok.hidden = !tok.hidden;
      if (!tok.hidden) tok.focus();
    };
    tok.addEventListener('change', function () { b.classList.toggle('set', !!tok.value); });
    tools.insertBefore(b, tools.firstChild);
  }

  function buildProfileMenu(tools, me) {
    var u = me.user;
    var name = u.display_name || u.username;
    var initial = (name || '?').trim().charAt(0).toUpperCase();
    var isLocal = u.provider === 'local' || !u.provider;
    var wrap = el('div', 'jprofile',
      '<button class="javatar" type="button" aria-haspopup="menu" aria-expanded="false"><span class="jav">' + esc(initial) + '</span><span class="lbl">' + esc(name) + '</span>' + icon('chevron', 'ic chev') + '</button>' +
      '<div class="jmenu" role="menu">' +
        '<div class="jmhead"><b>' + esc(name) + '</b><div class="jmsub">' + esc(u.email || u.username) + ' · ' + esc(u.role) + '</div></div>' +
        '<button class="jmitem" role="menuitem" data-act="profile">' + icon('user') + '개인정보 변경</button>' +
        (isLocal ? '<button class="jmitem" role="menuitem" data-act="password">' + icon('lock') + '비밀번호 변경</button>' : '') +
        (isHidden('keys', me) ? '' : '<button class="jmitem" role="menuitem" data-act="keys">' + icon('key') + 'MCP 키 관리</button>') +
        '<button class="jmitem" role="menuitem" data-act="theme" data-theme-label></button>' +
        '<div class="jmsep"></div>' +
        '<button class="jmitem danger" role="menuitem" data-act="logout">' + icon('logout') + '로그아웃</button>' +
        '<div class="jmfoot">sqlon v' + esc(me.version || '') + '</div>' +
      '</div>');
    tools.appendChild(wrap);
    var btn = wrap.querySelector('.javatar');
    var setOpen = function (open) { wrap.classList.toggle('open', open); btn.setAttribute('aria-expanded', String(open)); };
    btn.onclick = function (e) { e.stopPropagation(); setOpen(!wrap.classList.contains('open')); };
    document.addEventListener('click', function () { setOpen(false); });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && wrap.classList.contains('open')) { setOpen(false); btn.focus(); } });
    wrap.querySelector('.jmenu').addEventListener('click', function (e) { e.stopPropagation(); });
    wrap.querySelectorAll('.jmitem').forEach(function (item) {
      item.onclick = function () {
        var act = item.getAttribute('data-act');
        if (act === 'theme') { cycleTheme(); return; }
        setOpen(false);
        if (act === 'logout') { fetch('/auth/logout', { method: 'POST' }).then(function () { location.href = '/auth/login'; }); }
        else if (act === 'keys') { location.href = '/admin/keys'; }
        else if (act === 'profile') { openProfileModal(u); }
        else if (act === 'password') { openPasswordModal(); }
      };
    });
    applyTheme();
  }

  // ------------------------------------------------------ command palette
  // recently opened pages lead the palette when nothing is typed
  function recentKeys() { return (store.get('sqlon-recent', '') || '').split(',').filter(Boolean); }
  function rememberVisit(key) {
    if (!key) return;
    var list = recentKeys().filter(function (k) { return k !== key; });
    list.unshift(key);
    store.set('sqlon-recent', list.slice(0, 6).join(','));
  }

  function openPalette() {
    if (document.querySelector('.jpalette')) return;
    closeMobile();
    var host = el('div', 'jmodal jpalette',
      '<div class="box" role="dialog" aria-modal="true" aria-label="빠른 이동">' +
      '<div class="jp-search">' + icon('search') + '<input type="text" placeholder="화면 이름이나 기능으로 찾기 — 예: 경보, 세션, 권한" aria-label="검색어" autocomplete="off" spellcheck="false"><kbd>Esc</kbd></div>' +
      '<div class="jp-list" role="listbox"></div>' +
      '<div class="jp-foot"><span><kbd>↑</kbd><kbd>↓</kbd> 이동</span><span><kbd>Enter</kbd> 열기</span><span><kbd>Ctrl</kbd><kbd>K</kbd> 언제든 열기</span></div></div>');
    var close = openDialog(host);
    var input = host.querySelector('input');
    var list = host.querySelector('.jp-list');
    var sel = 0, shown = [];
    var norm = function (s) { return String(s || '').toLowerCase().replace(/[\s·]/g, ''); };
    function render() {
      var q = norm(input.value);
      if (!q) {
        // recent first (not the page we are on), then everything else
        var byKey = {};
        visible.forEach(function (it) { byKey[it.key] = it; });
        var recent = recentKeys().filter(function (k) { return byKey[k] && k !== window.JASQL.page; }).slice(0, 5)
          .map(function (k) { var it = Object.assign({}, byKey[k]); it.recent = true; return it; });
        var rk = {};
        recent.forEach(function (it) { rk[it.key] = 1; });
        shown = recent.concat(visible.filter(function (it) { return !rk[it.key]; }));
      } else {
        shown = visible.filter(function (it) { return norm(it.label + it.group + it.hint + it.key).indexOf(q) >= 0; });
      }
      if (sel >= shown.length) sel = Math.max(0, shown.length - 1);
      list.innerHTML = shown.length ? shown.map(function (it, i) {
        return '<div class="jp-item' + (i === sel ? ' sel' : '') + (it.key === window.JASQL.page ? ' here' : '') + '" role="option" aria-selected="' + (i === sel) + '" data-i="' + i + '">' +
          icon(it.icon) + '<span class="jp-label">' + esc(it.label) + '</span><span class="jp-hint">' + esc(it.hint) + '</span><span class="jp-group">' + esc(it.recent ? '최근' : it.group) + '</span></div>';
      }).join('') : '<div class="jp-empty">"' + esc(input.value) + '"에 맞는 화면이 없습니다.</div>';
      var cur = list.querySelector('.sel');
      if (cur && cur.scrollIntoView) cur.scrollIntoView({ block: 'nearest' });
    }
    function go(it) {
      if (!it) return;
      close();
      if (it.run) { it.run(); return; }
      if (it.target) { window.open(it.href, it.target); return; }
      location.href = it.href;
    }
    input.addEventListener('input', function () { sel = 0; render(); });
    input.addEventListener('keydown', function (e) {
      if (e.key === 'ArrowDown') { e.preventDefault(); sel = Math.min(shown.length - 1, sel + 1); render(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); sel = Math.max(0, sel - 1); render(); }
      else if (e.key === 'Enter') { e.preventDefault(); go(shown[sel]); }
    });
    list.addEventListener('mousemove', function (e) {
      var row = e.target.closest('.jp-item');
      if (row && +row.dataset.i !== sel) { sel = +row.dataset.i; render(); }
    });
    list.addEventListener('click', function (e) {
      var row = e.target.closest('.jp-item');
      if (row) go(shown[+row.dataset.i]);
    });
    render();
    input.focus();
  }

  // ------------------------------------------------- wide tables scroll
  // A table wider than the screen should scroll inside its card, not push
  // the whole page sideways. Pages render tables at any time, so watch.
  function scrollsX(node) {
    var ov = getComputedStyle(node).overflowX;
    return ov === 'auto' || ov === 'scroll' || ov === 'hidden';
  }
  function wrapTables(scope) {
    (scope || document).querySelectorAll('table').forEach(function (t) {
      if (t.closest('.ui-scroll, .jmodal, .jsb') || t.dataset.noWrap != null) return;
      for (var n = t.parentElement; n && n !== document.body; n = n.parentElement) {
        if (scrollsX(n)) return;
        if (n.tagName === 'MAIN' || n.classList.contains('card') || n.classList.contains('panel')) break;
      }
      var w = el('div', 'ui-scroll');
      t.parentNode.insertBefore(w, t);
      w.appendChild(t);
      // on a phone, many columns need room to scroll rather than squeeze
      var row = t.tHead && t.tHead.rows[0] || t.rows[0];
      var cols = row ? row.cells.length : 0;
      if (cols >= 6) t.classList.add('ui-wide');
      else if (cols >= 4) t.classList.add('ui-mid');
    });
  }
  var pending = false;
  function watchTables() {
    wrapTables();
    new MutationObserver(function (muts) {
      if (pending) return;
      for (var i = 0; i < muts.length; i++) {
        if (muts[i].addedNodes.length) {
          pending = true;
          requestAnimationFrame(function () { pending = false; wrapTables(); hideOffLinks(); });
          return;
        }
      }
    }).observe(document.body, { childList: true, subtree: true });
  }

  // ------------------------------------------ links to switched-off pages
  // Pages link to each other (a "SQL Lab" shortcut, row shortcuts). A
  // shortcut to a page switched off for this viewer is dropped; a link inside
  // a sentence stays, and opening it explains why the page is off.
  function switchedOffPath(path) {
    var known = false, open = false;
    GROUPS.forEach(function (g) {
      g.items.forEach(function (it) {
        if (it.href.split('#')[0] !== path) return;
        known = true;
        if (!isHidden(it.key, shellMe)) open = true;
      });
    });
    return known && !open;
  }
  function hideOffLinks() {
    if (!shellMe || !(shellMe.hidden_menus || []).length) return;
    document.querySelectorAll('main a[href], header a[href]').forEach(function (a) {
      if (a.dataset.menuOff) return;
      var u;
      try { u = new URL(a.getAttribute('href'), location.href); } catch (e) { return; }
      if (u.origin !== location.origin || !switchedOffPath(u.pathname)) return;
      var rest = a.parentElement.cloneNode(true);
      rest.querySelectorAll('a').forEach(function (x) { x.remove(); });
      if (/[^\s·|,/]/.test(rest.textContent)) return; // part of a sentence
      a.dataset.menuOff = '1';
      a.style.display = 'none';
    });
  }

  // ------------------------------------------------------ first run
  // Ops pages are empty until a database is registered; say so, and where.
  var NEEDS_DB = { fleet: 1, alerts: 1, sessions: 1, workload: 1, capacity: 1, availability: 1, maintenance: 1, dba: 1, security: 1, compliance: 1, 'dba-console': 1, ask: 1 };
  function firstRunHint() {
    if (!NEEDS_DB[window.JASQL.page]) return;
    fetch('/api/db-profiles', { headers: tokenHeaders(false) }).then(function (r) { return r.ok ? r.json() : null; }).then(function (d) {
      if (!d || (d.profiles || []).length) return;
      var main = document.querySelector('main');
      if (!main || main.querySelector('.ui-firstrun')) return;
      var box = isHidden('db', shellMe)
        ? el('div', 'ui-firstrun', icon('database') + '<div><b>아직 등록된 데이터베이스가 없습니다.</b><span>관리자에게 DB 등록을 요청하세요. 등록되면 이 화면이 채워집니다.</span></div>')
        : el('div', 'ui-firstrun', icon('database') + '<div><b>아직 등록된 데이터베이스가 없습니다.</b><span>DB 연결에서 읽기 전용 계정으로 첫 DB를 등록하면 이 화면이 채워집니다.</span></div><a class="btn primary" href="/admin/db">DB 연결하기</a>');
      main.insertBefore(box, main.firstChild);
    }).catch(function () { /* hint only */ });
  }

  // -------------------------------------------------- intro banners
  // The big intro banner helps on the first visit and costs space every
  // visit after. Let people fold it to its title line, per page.
  function foldableHeroes() {
    var key = 'sqlon-hero-folded';
    var folded = (store.get(key, '') || '').split(',').filter(Boolean);
    document.querySelectorAll('main .hero, main .profile-hero').forEach(function (h, i) {
      var id = (window.JASQL.page || '') + (i ? ':' + i : '');
      var b = el('button', 'ui-fold', '');
      b.type = 'button';
      var apply = function (on) {
        h.classList.toggle('ui-folded', on);
        b.innerHTML = icon('chevron', 'ic' + (on ? '' : ' up')) + '<span>' + (on ? '안내 펼치기' : '안내 접기') + '</span>';
        b.setAttribute('aria-expanded', String(!on));
      };
      b.onclick = function () {
        var on = !h.classList.contains('ui-folded');
        apply(on);
        var list = (store.get(key, '') || '').split(',').filter(function (x) { return x && x !== id; });
        if (on) list.push(id);
        store.set(key, list.join(','));
      };
      apply(folded.indexOf(id) >= 0);
      h.appendChild(b);
    });
  }

  // ------------------------------------------------- freshness labels
  // Pages write "최신 수집 <time> · <trace id>". The id helps support, not
  // the reader: keep it in the tooltip and show only the time.
  function tidyFresh() {
    var n = document.querySelector('header #fresh, header .fresh');
    if (!n) return;
    var tidy = function () {
      var t = n.textContent;
      var m = t.match(/^(.*?)\s*·\s*([A-Za-z][\w-]*-[0-9a-f]{6,}[\w-]*|trace 없음)\s*$/);
      if (!m) return;
      n.title = t;
      n.textContent = m[1];
    };
    tidy();
    new MutationObserver(tidy).observe(n, { childList: true, characterData: true, subtree: true });
  }

  // ---------------------------------------------------------- shortcuts
  document.addEventListener('keydown', function (e) {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && (e.key === 'k' || e.key === 'K')) {
      e.preventDefault();
      openPalette();
      return;
    }
    if (typing(e) || e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key === '?' && GUIDES[window.JASQL.page] && !document.querySelector('.jmodal')) {
      e.preventDefault();
      var h1 = document.querySelector('header h1');
      guideModal((window.JASQL.pageLabel || (h1 ? h1.textContent : '')) + ' 가이드', GUIDES[window.JASQL.page]);
    }
  });

  function ensureStyles() {
    if (document.querySelector('link[href="/admin/ui.css"]')) return;
    var l = document.createElement('link');
    l.rel = 'stylesheet'; l.href = '/admin/ui.css';
    document.head.appendChild(l);
  }

  window.JASQL = {
    page: null,
    guide: guideModal,
    onboarding: openOnboarding,
    palette: openPalette,
    icon: icon,
    refreshBadges: refreshBadges,
    menu: GROUPS,
    rebuildNav: rebuildNav,
    async mount(opts) {
      opts = opts || {};
      this.page = opts.page || null;
      this.pageLabel = opts.label || null;
      if (opts.guide) GUIDES[this.page] = opts.guide; // page-supplied override
      ensureStyles();
      root.classList.add('shell');
      if (this.page) document.body.setAttribute('data-page', this.page);
      var me = { auth_enabled: false };
      try { me = await (await fetch('/auth/me')).json(); } catch (e) { /* standalone fallback */ }
      if (me.auth_enabled && !me.authenticated) {
        location.href = '/auth/login?next=' + encodeURIComponent(location.pathname);
        return;
      }
      window.AUTH = me.auth_enabled && me.authenticated ? me : null;
      shellMe = me;
      buildSidebar(me);
      decorateHeader(me);
      watchTables();
      tidyFresh();
      firstRunHint();
      menuOffNotice();
      hideOffLinks();
      foldableHeroes();
      rememberVisit(this.page);
      refreshBadges();
      setInterval(refreshBadges, 60000);
      if (typeof opts.onReady === 'function') {
        try { opts.onReady(me); } catch (e) { console.error('onReady', e); }
      }
    },
  };
})();
