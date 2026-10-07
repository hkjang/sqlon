# Changelog

## Unreleased

## v0.7.1 — 2026-10-08

### 메뉴 관리 다듬기 — 여럿이 써도, 파일을 고쳐도 믿을 수 있게

- **동시 편집 보호**: 저장은 편집을 시작한 시점의 revision 을 함께 보냅니다. 그 사이 다른 관리자가 저장했다면
  거부하고(409), 화면이 최신 설정을 불러와 내 변경을 그 위에 다시 얹은 뒤 같은 메뉴를 둘 다 바꿨는지 알려줍니다.
  이전에는 나중에 저장한 사람이 먼저 저장한 사람의 변경을 소리 없이 지웠습니다.
- **최근 변경**: 저장마다 무엇이 어떻게 바뀌었는지("OpenMetadata 끔, SQL Lab 관리자·DBA만")와 누가·언제를 화면 아래에
  최근 30건 보여줍니다. 감사 로그도 결과가 아니라 변경 내용(`rev 3: openmetadata on→off; ask roles all→admin+dba`)을 남깁니다.
- **역할로 보기**: 위쪽 역할 숫자를 누르면 그 역할에게 안 보이는 메뉴를 표시합니다. 어떤 역할이 열 수 있는 화면이 0이 되면
  저장 전에 경고합니다.
- **메뉴 찾기**(이름·설명·주소), **Ctrl+S** 저장, 바뀐 게 없는 저장은 새 revision 을 만들지 않음.
- **설정 파일**: 직접 고치거나 백업에서 복구하면 재시작 없이 반영됩니다. 규칙에 맞지 않는 파일(예: 역할 이름 오타)은
  그 메뉴를 모두에게서 숨기는 대신 모든 메뉴를 켠 채 이유를 표시하고, 화면에서 한 번 저장하면 복구됩니다.

### 수정

- 예방 경보 메뉴를 꺼도 브라우저 탭 제목에 "(긴급 N)" 이 계속 붙던 것(변경 관리 배지도 같음).
- 첫 화면(운영 현황)을 끄면 로그인할 때마다 "꺼져 있습니다" 안내가 뜨던 것 — 아무도 연 적 없는 기본 화면이므로 조용히 넘어갑니다.
- 단독 모드에서 메뉴 설정 API 가 서버의 실제 파일 경로를 알려주던 것.
- 꺼진 스위치의 테두리 대비가 1.5:1 이던 것을 3:1 이상으로(WCAG 1.4.11), 일부만 켜진 그룹 스위치가 꺼짐처럼 보이고
  화면낭독기에 스위치로 읽히던 것을 반쯤 켜진 모양 + "혼합" 상태로.
- 메뉴 관리 화면의 소개·최근 변경 상자에 여백과 테두리가 없던 것.

### 검증

- 콘솔 브라우저 검사 `test/ui/menus.cjs` 를 저장소에 넣었습니다(로그인·단독 모드 62항목, 실행법은 docs/development.md).

## v0.7.0 — 2026-10-07

### 메뉴 관리 — 관리자가 콘솔 메뉴를 켜고 끕니다

- **메뉴 관리**(`/admin/menus`, 설정 그룹): 메뉴별·그룹별로 켜고 끄고, 로그인 모드에서는 메뉴를 볼 수 있는 역할(관리자·DBA·사용자)을
  좁힙니다. 저장하면 재시작 없이 모든 사용자에게 바로 적용되고, 저장 전에도 역할별로 보게 될 메뉴 수를 미리 보여줍니다.
- **주소로 열어도 막힙니다**: 끈 메뉴는 사이드바·빠른 이동·다른 화면의 바로가기에서 사라지고, 화면 주소로 열면 서버가 그 사람이 열 수 있는
  첫 화면으로 돌려보내며 "꺼져 있습니다" 안내를 띄웁니다. 열 수 있는 화면이 없으면 안내 페이지(403)를 보여줍니다.
- 역할은 메뉴의 기본 권한보다 넓힐 수 없고(예: 사용자 관리는 관리자 전용), **메뉴 관리 자체는 끌 수 없습니다**.
- 단독 모드에서도 동작합니다(켜기·끄기만, 저장에 관리 토큰 필요).
- 설정은 `<data>/operations/console/menus.json` 에 저장되고 변경은 감사 로그(`admin:console_menus_update`)에 남습니다.
  API: `GET/PUT /api/console/menus`. 메뉴 설정은 콘솔 화면만 바꾸며 REST·MCP 권한은 역할·키로 통제됩니다.
- 관리자 전용 화면에 들어온 비관리자는 전처럼 DB 연결로, DB 연결이 꺼져 있으면 열 수 있는 첫 화면으로 보냅니다.
- 공통 컴포넌트: 켜기·끄기 스위치(`.ui-switch`), 안내 상자(`.ui-notice`).

## v0.6.1 — 2026-10-07

### 배포 이미지 정리 — `sqlon:v0.6.1` 하나

- 릴리즈 Docker 이미지를 하나로 합쳤습니다. 이미지 이름은 `sqlon:vX.Y.Z`, 이미지 파일은
  `sqlon-vX.Y.Z.tar.gz` 와 `.sha256` 입니다. Oracle Instant Client·godror 와 PostgreSQL/MySQL/MariaDB
  드라이버가 모두 들어 있어 어떤 DB든 이 이미지 하나로 운영합니다. `sqlon-eval`·`sqlon-goldgen` 도 포함합니다.
  (이전: `sqlon/sqlon:vX`·`sqlon/sqlon-oracle:vX`, `sqlon-vX-docker.tar.gz`·`sqlon-oracle-vX-docker.tar.gz`)
- 릴리즈 스크립트가 저장한 이미지 파일을 다시 `docker load` 해서 검사한 뒤에야 끝납니다
  (`scripts/verify-image.sh`, 항목과 이유는 `docs/RELEASE_CHECKLIST.md`). 이번 릴리즈는 실제 Oracle
  AI Database 26ai Free(23.26.3)에 연결해 확인했습니다.

### 수정

- **빈 호스트 디렉터리를 데이터로 마운트하면 기동하지 못하던 문제**: 관리자 가이드대로 `mkdir` 한
  디렉터리를 `/app/data/sqlon` 에 붙이면 `load SQLON catalog: ... no such file` 로 바로 종료했습니다.
  이제 비어 있으면 첫 기동 때 기본 메타데이터로 채우고, 쓸 수 없으면 `chown -R 10001:10001` 을 안내합니다.
- **Oracle 연결 실패 안내**: 비밀번호가 틀려도(ORA-01017) "TLS/인증서 설정이 맞지 않습니다" 로
  안내하던 것을 인증 실패로 바로잡았습니다. ORA-28000(계정 잠김)·ORA-12514(서비스 없음)·ORA-12541(리스너
  없음)·ORA-12170(시간 초과)도 각각 분류하고, 오류 코드를 `INTERNAL` 대신 `ORA-01017` 처럼 돌려줍니다.
- 단독 바이너리에서 Oracle 을 고르면 나오는 안내가 이제 `sqlon` Docker 이미지를 가리킵니다.

## v0.6.0 — 2026-10-06

### 콘솔 디자인·사용성 전면 개편

- **하나의 디자인 시스템**: 화면마다 따로 쓰던 225가지 색을 약 30개 토큰으로 정리하고(`/admin/ui.css`),
  두 세대로 갈라져 있던 화면(어두운 헤더·큰 배너 / 밝은 헤더)을 같은 헤더·버튼·입력·표·카드로 맞췄습니다.
- **다크 테마**: 시스템 · 라이트 · 다크를 메뉴 아래 버튼이나 프로필 메뉴에서 바꿉니다. 첫 화면부터 깜빡임 없이
  적용됩니다. 두 테마 모두 25개 화면의 모든 글자가 명도 대비 4.5:1(WCAG AA) 이상입니다.
- **메뉴 재구성**: 28개 메뉴를 업무 단위(모니터링 · 진단·점검 · 변경·작업 · SQL Lab · 메타데이터 · 설정)로 묶고,
  그룹 접기(상태 기억), 일관된 선 아이콘, 아이콘만 보이는 좁은 메뉴를 넣었습니다. 메뉴 이름과 화면 제목·탭 제목을
  일치시켰습니다(예: "DB 플릿 설정"·"DB 연결 관리 · 쿼리 실행" → "DB 연결").
- **빠른 이동(Ctrl+K)**: 어느 화면에서든 화면 이름·기능으로 찾아 엽니다. 검색어가 없으면 최근 방문 화면이 먼저
  나옵니다. **?** 키로 그 화면의 가이드를 엽니다. 운영 현황·예방 경보·예방 점검·컴플라이언스 가이드를 추가했습니다.
- **상태 배지**: 예방 경보의 미확인 긴급·경고 건수와 승인 대기 변경계획 수를 메뉴 배지로 보여주고, 긴급 경보가 있으면
  브라우저 탭 제목에 "(긴급 N)"을 붙입니다(`GET /api/console/summary`).
- **DB 선택 유지**: 한 화면에서 고른 DB가 세션·워크로드·점검·진단·DBA 콘솔·SQL Lab 등 모든 화면에서 유지됩니다.
  운영 현황 표의 **바로가기**로 그 DB의 세션·워크로드·점검·경보를 바로 엽니다.
- **휴대폰**: 14개 화면에서 생기던 가로 넘침(최대 465px)과 글자 단위로 깨지던 제목을 없앴습니다. 넓은 표는 카드
  안에서 가로 스크롤되고, 헤더는 제목과 도구를 첫 줄에 둡니다.
- **로그인·소개 화면**을 새로 만들었습니다(비밀번호 표시·Caps Lock 안내, 제품 소개). 로그인 후 기본 이동 화면은
  운영 현황입니다.
- 등록된 DB가 없으면 운영 화면에 **첫 DB 연결** 안내를 띄우고, 화면 맨 위 소개 배너는 접어 둘 수 있습니다(화면별 기억).
- 키보드: 본문 바로가기 링크, 모든 대화상자의 Esc 닫기·포커스 복귀, 현재 화면 `aria-current` 표시.

### 수정

- 운영 현황의 "지원 기능" 칩이 행 높이를 늘리고 "근거·제한" 열이 잘리던 것 — "9/11 지원"과 미지원 목록으로 요약하고
  근거는 줄바꿈합니다.
- 소개 화면의 "운영 현황" 카드가 인시던트 화면으로 가던 링크, 로그인 사용자 이름을 이스케이프 없이 넣던 부분.
- 헤더의 수집 시각 옆에 붙던 추적 ID는 툴팁으로 옮겼습니다.

## v0.5.0 — 2026-10-06

### 예방 경보가 확실히 사람에게 닿도록

- **당직 호출**: 긴급 경보가 `SQLON_ALERT_ESCALATE_AFTER`(기본 30분) 동안 확인되지 않으면
  당직 채널(`SQLON_ALERT_ESCALATION_WEBHOOK`, DB별 `alerting.escalation_ref`)로 한 번 더 알리고,
  해소되면 "호출 해소" 를 보냅니다. 확인(ack)·무음이 걸린 경보는 호출하지 않습니다.
- **SQLON 생존 신호**: `SQLON_HEARTBEAT_URL`(healthchecks.io·Uptime Kuma 등)을 평가 주기마다
  호출합니다. 평가가 실패하거나 기본 채널 전달이 실패 중이면 핑을 보류해, SQLON이 멈추거나 경고를
  내보내지 못하는 상태를 외부 감시가 잡아냅니다.
- **Mattermost 버튼**: `SQLON_ALERT_CHAT_ACTIONS=mattermost` 면 알림마다 ✅ 확인 · 🔕 2시간 무음 ·
  🛠 수정안 만들기 버튼이 붙습니다. 콜백(`POST /api/early-warning/chat-action`)은 경보 하나·동작
  하나만 허용하는 서명 토큰(HMAC-SHA256, 7일 만료)으로 인가하고, 수정안은 변경계획 초안만 만듭니다
  (같은 경보에 진행 중인 계획이 있으면 재사용, 무음도 이미 걸려 있으면 겹쳐 만들지 않음). 버튼을 끄면
  게시된 버튼도 무효가 됩니다.

### 이상 징후를 더 정확하게

- **증가 원인**: 저장공간 경보·예측·용량 계획·일일 리포트가 최근 증가분을 테이블·WAL·임시파일·로그,
  볼륨이면 DB 점유량·DB 밖 파일로 나눠 "무엇이 늘었나" 를 보여줍니다. 이번 주 새로 생긴 테이블은 0부터
  자란 것으로 셉니다.
- **배포 마이그레이션 인식**: 스키마 변경과 함께 Flyway·Liquibase·Django·Prisma·Knex 이력 테이블에
  새 마이그레이션이 기록됐으면 배포로 표시하고(정보, 마이그레이션 목록 첨부) 계획 외 변경과 구분합니다.
  배포여도 테이블·컬럼 삭제는 경고로 남기고, 직전 점검에 이미 있던 마이그레이션은 이후의 수동 `ALTER`
  를 설명하지 못합니다.

### 관리

- MCP `configure_early_warning` 에 `escalation_ref`·`escalate_after`·`heartbeat_ref`·`chat_actions`·
  `action_url`, `configure_profile_alerting` 에 `alerting.escalation_ref`, `test_alert_channel` 에
  `channel=escalation` 을 추가했습니다. 콘솔 설정 패널·채널 상태(📟 당직, 💓 생존 신호)·경보 표의 당직
  호출 표시·예측 표의 증가 원인, DB 프로파일의 당직 채널 입력도 같이 바뀌었습니다.

### 수정

- 웹훅·핑 전송 오류 메시지가 URL 경로(웹훅의 비밀)를 그대로 담아 콘솔·상태·로그에 노출하던 문제를
  고쳤습니다. 이제 `https://host/…` 로 가립니다.
- SIGTERM·Ctrl+C(docker stop, systemd, Kubernetes)에 요청을 마무리하고 예방 경보 상태·시계열을 디스크에
  기록한 뒤 종료합니다. 이전에는 재시작·배포 때마다 마지막 저장 이후 최대 10분의 시계열이 사라졌습니다.
- 스키마 변경이 배포로 분류되면 "권한 오남용일 수 있습니다" 대신 배포에 맞는 조치 안내를 붙입니다.
- 설정 값의 기간을 `6h0m0s` 대신 `6h` 처럼 입력한 모양 그대로 보여줍니다.

## v0.4.0 — 2026-10-06

### MCP 로 예방 경보 전체 운영

- 예방 경보의 모든 기능을 MCP 도구 11종으로 쓸 수 있습니다: 조회(`get_early_warnings`,
  `explain_early_warning`, `plan_capacity`), 조치(`acknowledge_early_warning`,
  `manage_early_warning_silences`, `report_host_disk`), 관리자(`configure_early_warning`,
  `configure_profile_alerting`, `run_early_warning_check`, `test_alert_channel`), DBA
  (`propose_early_warning_fix`). 단독 모드는 관리 토큰, 로그인 모드는 사용자 권한 범위로 동작하며
  모든 변경은 감사 로그에 남습니다.
- **전략적 사용**: `get_early_warnings` 가 원인 경보 → 긴급 → 경고 순으로 정렬된 `next_actions`
  (그대로 호출할 도구와 인자)를 돌려주고, MCP 프롬프트 `early_warning_triage` 가 확인 → 원인 분석 →
  승인 게이트 수정 → 재평가 순서를 안내합니다.
- **경보 → 수정 변경계획**: 버려진 복제 슬롯 제거, VACUUM 차단 세션 종료·prepared 롤백, 블로트
  VACUUM, `max_slot_wal_keep_size` 상한, `autovacuum` 켜기, `pg_monitor` 부여를 승인 대기 초안으로
  만듭니다. 검증 단계는 조치가 적용되지 않으면 실패합니다.
- **원인 추정**: 함께 발생한 경보를 원인→결과로 묶어(예: 복제 슬롯 → pg_wal 과다 → 고갈 예측 →
  사용률 초과) 헤드라인·콘솔·알림(`🔗 원인 추정`)에 표시합니다.
- **흔들림 억제**: 1시간에 3번 이상 발생·해소를 반복하는 경보는 안정될 때까지 알림을 보류합니다.
- **용량 계획**: N일을 버티기 위한 볼륨 크기·부족분·시점과 추세 1·2·3배 시나리오.
- **런타임 설정**: 기본 알림 채널·최소 위험도·재알림·일일 리포트·점검 주기를 재시작 없이 바꾸고
  (`PUT /api/early-warning/settings`, 콘솔 설정 패널, MCP), 재시작 후에도 유지합니다.
- 콘솔: 원인 추정 패널, 경보 상세(수정안 만들기), 용량 계획, 설정 패널, 흔들림 표시.

## v0.3.0 — 2026-10-06

### 예방 경보 강화

- **DB 서버 디스크 실측 보고**: `scripts/sqlon-disk-report.sh`(POSIX sh + df, curl 또는
  BusyBox wget)를 DB 서버 cron 에서 돌리면 `POST /api/early-warning/disk` 로 볼륨 사용량이
  들어와, SQL로는 보이지 않는 DB 밖 파일(덤프 백업·외부 로그)과 볼륨의 실제 크기로 고갈을
  예측합니다. 경보에 "DB 밖 파일 약 N GiB" 를 함께 보여주고, DB에 접속할 수 없어도 평가하며,
  보고가 15분 넘게 끊기면 경고합니다. ext4 root 예약 블록은 한도에서 뺍니다.
- **MySQL·MariaDB 점유량**: 연결된 DB 하나가 아니라 모든 스키마 + binlog 를 수집합니다.
  binlog 권한(REPLICATION CLIENT·BINLOG MONITOR)은 `SHOW GRANTS` 로 먼저 확인해 권한
  오류가 회로 차단기를 열지 않게 합니다.
- **무음(silence)**: 계획 작업 동안 DB·규칙(접두어 가능) 단위로 알림만 멈춥니다. 경보는
  계속 표시되고 끝날 때 여전히 유효한 것을 보냅니다. 콘솔에서 추가·해제합니다.
- **DB별 알림 채널**: 프로파일 `alerting.webhook_ref`(env:/file:/plain:, 화면에서 가림)로 팀
  채널을 지정하고 `alerting.min_severity` 로 DB별 최소 위험도를 정합니다. 채널마다 전달 상태와
  재시도를 따로 관리하고, 해석할 수 없는 채널은 기본 채널로 대신 보냅니다.
- **일일 용량 리포트**: 매일 09:00(`SQLON_ALERT_DIGEST_AT`) 기본 채널로 DB별 사용률·추세·
  가득 차는 날짜를 보냅니다.

### 성능·정리

- 수집기가 매 주기 "직전 스냅숏"을 찾으려고 저장소 전체(보존 30일)를 읽던 것을 메모리
  캐시로 바꿨습니다. 7일치 저장소 기준 조회 1회 0.47초 → 메모리 조회이며, 그동안 저장소
  잠금을 잡지 않습니다. 플릿 상태·운영 화면 조회도 같은 경로를 씁니다.
- v0.1.0 직후 커밋에서 0바이트로 잘렸던 `docs/` 문서 28개를 복원하고 기본 포트 변경(6767)을
  반영했습니다.

## v0.2.0 — 2026-10-05

### 예방 경보 (Early Warning)

PostgreSQL 볼륨이 가득 차 DB가 멈추는 장애를 사전에 알리는 체계를 추가했습니다.
지금까지 PostgreSQL 용량 수집에는 한도가 없어 사용률·고갈 예측 경보가 구조적으로
한 번도 울릴 수 없었고, WAL을 붙잡는 슬롯·아카이브 점검은 주기적으로 돌지 않았습니다.

- **저장공간 점유량**: PostgreSQL에서 연결된 데이터베이스뿐 아니라 인스턴스의 모든
  데이터베이스 + `pg_wal` + 임시파일 + 로그 디렉터리를 `storage:footprint` 로
  수집합니다. `pg_ls_*` 실행 권한을 먼저 조회해, 권한이 없으면 호출하지 않고
  `pg_monitor` 부여를 안내합니다(권한 오류가 회로 차단기를 열지 않도록).
- **용량 한도 선언**: DB 프로파일에 `capacity.storage_limit`(예: `500GiB`)와
  사용률·남은 일수 임계값을 선언합니다. 화면(`/admin/db`)에서 입력할 수 있습니다.
- **고갈 예측**: 최근 6시간·7일 최소제곱 회귀로 증가율을 구해 가득 차는 시점을
  계산합니다. 6시간 추세는 R² ≥ 0.8일 때만 사용해 일회성 대량 적재로 오경보하지
  않습니다. 한도가 없어도 증가 속도 급증은 감지합니다.
- **디스크를 채우는 원인 점검**(예방 점검 확장, 5분 주기): 실패·적체된 WAL 아카이브,
  `archive_mode=on` 인데 비어 있는 아카이브 명령, `max_wal_size` 대비 과다한 `pg_wal`,
  연결됐지만 크게 뒤처진 복제 슬롯과 13+ `wal_status` lost/unreserved, VACUUM 을
  막는 장기·prepared 트랜잭션, `max_slot_wal_keep_size=-1`·`autovacuum=off`.
  스탠바이에서도 슬롯 조회가 실패하지 않도록 LSN 기준을 바꿨습니다.
- **테이블 급증·급감**: 테이블별 24시간 변화를 자기 평소 증가량과 비교합니다.
  다운샘플링이 대량 적재나 TRUNCATE 같은 급변을 지우지 않도록 큰 변화는 별도 점으로 보존합니다.
- **스키마 변경 감지**(15분 주기): 물리 스키마를 기준선과 비교하고, 실행된
  변경계획에 없는 변경을 "계획 외"로 표시합니다. 부분 수집된 스냅숏은 비교하지 않습니다.
- **관측 중단**: 3회 연속 수집 실패 시 경보합니다.
- **경보 수명주기**: 발생·격상·지속 재알림(6시간)·해소·확인(ack). 경보를 만든 점검이
  실제로 다시 실행됐을 때만 해소하고, 상태를 저장해 재시작해도 중복 알림이 없습니다.
- **알림**: Mattermost·Slack incoming webhook 호환 메시지(`SQLON_ALERT_WEBHOOK`).
  한 주기를 한 메시지로 묶고, 실패 시 지수 백오프로 재시도하며 전달 상태를 표시합니다.
  수집기의 기존 경보(쿼리 지연 회귀 등)도 같은 경로로 전달됩니다.
- **조회**: `/admin/alerts` 예방 경보 콘솔, MCP 도구 `get_early_warnings`,
  REST `/api/early-warning`(+ack·evaluate·test-notification), Prometheus
  `sqlon_storage_days_to_full` 등 게이지.
- 운영 스냅숏 조회가 `Since` 이전 날짜 파일을 읽지 않도록 했습니다.
- DB 프로파일 화면에서 프로파일을 수정하면 화면에 없는 `config_baseline` 이
  지워지던 문제를 고쳤습니다.

상세: [docs/early-warning.md](docs/early-warning.md)

## v0.1.5 — 2026-09-14

- PII 노출 리포트(`get_pii_exposure`)의 짧은 ASCII 단서(`pan`·`dob`·`ssn`·
  `card` 등 네 글자 이하)를 부분 문자열이 아닌 낱말 경계로 대조해
  `japan_code`가 신용카드로, `adobe_flag`가 생년월일로 잡히던 오탐을
  제거했습니다. 밑줄이 포함된 단서, 다섯 글자 이상 단서, 한글 단서는 기존
  부분 문자열 대조를 유지하며 `pan`·`dob`·`ssn`·`card_no`·`cust_addr`·
  `iban`·`rrn`은 그대로 탐지됩니다.
- 실행 중 생기는 로컬 데이터(`data/backups`, `data/sqlon`)와 로컬 데모
  스크립트를 `.gitignore`에 추가했습니다.
- GitHub Pages 랜딩 페이지에 한국어 기본·영어 전환, 브라우저 언어 자동
  감지, SEO/AEO 메타데이터와 Schema.org JSON-LD, 모바일 반응형 레이아웃,
  GitHub Sponsor 버튼을 추가했습니다.

## v0.1.0 — 2026-07-20

- 4개 엔진의 실제 누적 워크로드·대기·Top SQL·용량 통계를 읽기 전용 Provider로
  수집하고 append-only 운영 저장소에 보존하는 주기 수집기를 추가했습니다.
  이전 스냅숏에서 QPS/TPS와 일간 용량 증가량을 계산하며 REST/MCP와
  워크로드·용량 화면이 동일 서비스 계층을 사용합니다.
- PostgreSQL·MySQL·MariaDB·Oracle의 읽기 전용 시스템 뷰를 공통 Provider로
  정규화하는 세션·잠금 관찰 서비스를 추가했습니다. REST/MCP/UI에서 장기 SQL과
  장기 트랜잭션, 대기 이벤트, RAC 안전 세션 키, 보호 세션, 루트 블로커와 영향
  세션 수를 근거·수집 시각과 함께 확인할 수 있습니다.
- 권한 범위 내 PostgreSQL·MySQL·MariaDB·Oracle 프로파일을 공통 서비스에서
  병렬 점검하고 위험 순위, 수집 상태, 근거, 최신성, 엔진 Capability를 반환하는
  플릿 REST/MCP API와 첫 화면을 추가했습니다. DB 프로파일 UI에는 업무 서비스,
  환경, 중요도, 역할, 담당 조직과 Oracle 연결·Pack 정책 입력을 추가했습니다.
- 운영 환경 프로파일의 조회·DBA 자격증명은 `plain:` 저장을 거부하고 `env:` 또는
  `file:` Secret 참조를 강제합니다.
- 기본 시작 시 기존 `data/metadb`의 프로파일, 카탈로그와 감사 로그를 완전
  백업한 뒤 `data/sqlon`으로 충돌 없이 원자적 병합하는 마이그레이션을
  추가했습니다. 명시적인 데이터 경로는 이동하지 않으며 기존 환경변수 별칭을
  계속 지원합니다.
- PostgreSQL 메타 저장소를 `sqlon_meta` 스키마로 전환하고 기존
  `public.jamypg_*` 테이블과 데이터를 트랜잭션 안에서 자동 이동합니다. 신규
  인증 쿠키와 MCP 키는 `sqlon_session`, `ssk_`를 사용하며 기존 쿠키와
  `jsk_` 키는 한 릴리스 동안 계속 인증됩니다.
- Prometheus 제품 지표를 `sqlon_*`로 전환하고 기존 `jamypg_*` 이름을
  deprecated 별칭으로 제공합니다.

## v0.58.0 — 2026-07-15

- 데이터셋 관리 화면에 검색, 상태 필터, 요약 지표와 변경 영향 중심의 작업 흐름을 추가했습니다.
- 테이블 편집기에 데이터셋 요약, 단계 안내, 변경 상태 기반 저장과 단축키를 추가했습니다.
- 데이터셋 조회 실패를 빈 데이터로 오인하지 않도록 오류를 명확히 표시하고 편집을 차단합니다.
- 객체·배열 셀의 잘못된 JSON 입력을 저장 전에 검증해 데이터 타입 손상을 방지합니다.

## v0.57.0 — 2026-07-15

### OpenMetadata integration reliability

- Added connection testing before configuration save with URL validation and
  failure-stage diagnostics for authentication, DNS, network, timeout, and API
  path errors.
- Hardened table and glossary pagination against repeated cursors and exposed
  partial-fetch warnings instead of silently treating incomplete imports as
  successful.
- Split imports into preview, review-queue, and direct-apply workflows, and
  added lineage planning and publishing to the administration UI.

### Work area UX

- Natural-language queries can now select a DB profile and use its dedicated
  catalog workspace, with the effective catalog source shown in the result.
- History adds full-text filtering, activity summaries, prompt reuse, and SQL
  handoff to the DB console.
- Statistics now emphasize SQL validity rate, interpret quality signals, show
  refresh state, and link directly to relevant follow-up actions.
- Refreshed responsive layouts, guided empty states, example queries, and
  in-product help across query, history, statistics, and OpenMetadata screens.

## v0.56.0 — 2026-07-15

### Profile catalog reliability and workflow

- Added live schema discovery and selectable collection scope before building
  a profile catalog workspace.
- Profile catalog APIs now enforce profile access checks for direct workspace
  and dataset reads.
- Workspace load failures and per-profile batch build failures are surfaced
  with actionable detail instead of being hidden behind aggregate counts.
- The UI now reflects the actual activation model: standalone mode supports a
  temporary global switch, while meta-DB mode automatically uses the workspace
  selected by each request's profile.
- Redesigned the profile catalog screen around the select, discover, build,
  review, and enrich workflow with search, readiness state, guided empty
  states, responsive controls, and clearer dataset counts.

## v0.55.0 — 2026-07-15

### Database connection UX and diagnostics

- Added an engine-first DB connection wizard for PostgreSQL, MySQL, and
  MariaDB. The selected engine is now persisted explicitly instead of falling
  back to PostgreSQL.
- Profile saves now run an immediate connection test and show actionable
  diagnostics for DNS, network, authentication, database, TLS, secret-mount,
  and server compatibility failures.
- The DB screen now reports the drivers actually compiled into the binary;
  MySQL/MariaDB use the bundled pure-Go `go-sql-driver/mysql` and require no
  native client library in the runtime image.

### DBA console

- Redesigned the privileged DBA console around the operator workflow and added
  detailed PostgreSQL/MySQL/MariaDB capability guides.
- Improved profile context, status visibility, edit-from-list actions, safety
  guidance, and responsive presentation.

## v0.2.0 — 2026-07-11

### Security and DBA controls

- Standalone HTTP remains loopback-only by default. A non-loopback bind now
  requires either meta-DB authentication or both `-public-mcp` and an admin
  token.
- All profile-backed MCP operations (`run_sql_safely`, live `explain_sql`, and
  execution-based `run_evaluation`) share one authorization registry and
  standalone admin-token gate.
- Feedback is server-scoped and quarantined as `pending/untrusted`; only an
  administrator-approved record may influence few-shot examples, retrieval
  priors, or learned rules. Size/rate limits and duplicate suppression are
  included.
- Operator default filters support `enforcement: error` for execution-blocking
  policy, with dialect AST validation and query-block/alias-aware predicate
  checks.

### AI grounding

- Metric resolution now combines exact name/business name/alias priority with
  glossary synonyms and conservative token coverage/proximity scoring.
- Metric lookup, question analysis, and schema retrieval use the same resolver;
  responses include confidence and match evidence.

### MCP and engineering

- Added the administrator-only `review_feedback` tool (29 registered tools).
- Tool registry, dispatcher, and README drift is now detected by tests.
- OpenAPI and MCP server versions share one source of truth.
- Added GitHub Actions checks for module verification, vetting, tests, and all
  CLI builds.

### Upgrade notes

- Existing v1 feedback JSONL records remain audit data but are not trusted or
  learned automatically. Submit new feedback and approve it through
  `review_feedback`.
- `record_feedback` no longer changes retrieval or learning immediately.
- Existing externally bound standalone commands must add `-public-mcp` and
  configure `-admin-token`, or migrate to authenticated `-meta-db` mode.
- Default-filter behavior remains warning-only unless the entry explicitly sets
  `"enforcement": "error"`.
