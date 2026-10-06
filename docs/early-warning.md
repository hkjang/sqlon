# 예방 경보 (Early Warning)

디스크가 가득 차서 DB가 멈추는 장애는 대부분 **몇 시간에서 며칠 전부터 신호가 있습니다**.
WAL 아카이브가 실패하기 시작했거나, 소비자가 사라진 복제 슬롯이 WAL을 붙잡거나,
배치가 테이블을 평소의 열 배로 키우는 순간부터입니다. 예방 경보는 SQLON이 이미
읽기 전용으로 수집하는 관측 결과를 1분마다 평가해, 그런 신호를 **장애 전에** 웹훅
(Mattermost·Slack)과 콘솔(`/admin/alerts`)로 알립니다.

DB에 새 쿼리를 보내지 않습니다. 입력은 모두 기존 수집기(워크로드·용량 스냅숏),
예방 점검(`get_maintenance_health`), 메타데이터 수집기(물리 스키마)가 이미 고정된
읽기 전용 쿼리로 모은 것입니다.

## 3단계 설정

1. **모니터링 계정에 `pg_monitor` 부여** (PostgreSQL)

   ```sql
   GRANT pg_monitor TO sqlon_mon;   -- 읽기 전용 모니터링 내장 역할
   ```

   없으면 데이터베이스 크기만으로 점유량을 추정하고, WAL·임시파일·로그 크기와
   아카이브 적체는 보지 못합니다. 이 경우 콘솔에 정보 항목
   "모니터링 권한 부족"이 표시됩니다. 권한이 없는 함수는 **호출하지 않습니다**
   (권한 오류가 프로파일의 회로 차단기를 열지 않도록 먼저 권한을 조회합니다).

2. **프로파일에 저장공간 한도 선언** — `/admin/db` 의 "저장공간 한도" 또는 JSON:

   ```json
   "capacity": { "storage_limit": "500GiB", "warn_days": 14, "critical_days": 3 }
   ```

   PostgreSQL·MySQL은 SQL로 디스크 여유 공간을 알려주지 않습니다. PGDATA·WAL이
   있는 볼륨 크기(`df -h` 의 Size)를 넣어야 "며칠 후 가득 차는지"를 계산합니다.
   `500GiB`·`2TB`·`750G`(df 표기)·바이트 수를 받으며 GB/TB는 1000, GiB/TiB와 G/T는
   1024 단위입니다. 비워 두면 증가 추세·급증만 감시합니다.

   | 키 | 기본값 | 의미 |
   | --- | --- | --- |
   | `storage_limit` | (없음) | 볼륨 크기 |
   | `warn_percent` / `critical_percent` | 80 / 90 | 사용률 경고·긴급 |
   | `warn_days` / `critical_days` | 14 / 3 | 고갈 예측 경고·긴급(일) |

3. **알림 웹훅 지정**

   ```sh
   SQLON_ALERT_WEBHOOK=https://mattermost.example.com/hooks/xxxxxxxx
   SQLON_ALERT_CONSOLE_URL=https://sqlon.example.com/admin/alerts   # 메시지의 콘솔 링크
   ```

   Mattermost·Slack incoming webhook에 그대로 쓸 수 있도록 `text`(Markdown)와
   구조화된 `notifications` 배열을 함께 보냅니다. `/admin/alerts` 의 **알림 테스트**
   버튼(또는 `POST /api/early-warning/test-notification`)으로 경로를 확인하세요.
   웹훅이 없으면 경보는 콘솔에만 표시됩니다.

관측 수집기(`-observe-interval`, 기본 1분)가 켜져 있어야 평가가 돕니다.

**MySQL·MariaDB**: 점유량은 모든 스키마 + binlog 입니다. binlog 크기를 보려면
`GRANT REPLICATION CLIENT ON *.* TO <계정>;` (MariaDB 10.5+ 는 `BINLOG MONITOR`)
가 필요합니다. 권한이 없으면 `SHOW BINARY LOGS` 를 실행하지 않고 안내만 표시합니다.

## 4단계(권장): DB 서버 디스크 보고

SQL로는 볼륨의 **다른 파일**(덤프 백업·외부 로그·코어 파일·다른 프로그램)과 볼륨의
실제 크기를 알 수 없습니다. DB 서버에서 `scripts/sqlon-disk-report.sh` 를 1분마다
실행하면 `df` 결과가 SQLON으로 들어와 디스크 자체를 예측합니다.

```sh
# DB 서버 crontab (curl 또는 BusyBox wget 만 있으면 됩니다)
* * * * * SQLON_URL=https://sqlon.example.com SQLON_TOKEN=<관리 토큰 또는 MCP 키> \
  SQLON_PROFILE=orders-prod /usr/local/bin/sqlon-disk-report.sh /var/lib/postgresql /pgwal
```

- 인자로 데이터·WAL·백업 디렉터리를 주면 그 디렉터리가 있는 볼륨을 보고합니다(같은 볼륨은 한 번).
- 한도는 `사용 + 여유` 입니다. ext4 의 root 예약 블록은 DB가 쓸 수 없으므로 빼고 계산합니다.
- 경보에 "DB 점유량 41 GiB, DB 밖 파일 약 329 GiB" 처럼 DB 밖에서 차지하는 양을 함께 보여줍니다.
- DB에 접속할 수 없어도 디스크 경보는 계속 평가합니다(디스크가 차서 DB가 멈춘 경우).
- 보고가 15분 넘게 끊기면 "호스트 디스크 보고 중단" 경고가 납니다.
- 디스크 보고가 있으면 "용량 한도 미선언" 안내는 사라집니다(선언 한도 없이도 예측).

요청 형식(다른 에이전트에서 보낼 때): `POST /api/early-warning/disk`,
`Authorization: Bearer <토큰>`,
`{"profile":"orders-prod","host":"db01","volumes":[{"mount":"/var/lib/postgresql","total_bytes":…,"used_bytes":…,"avail_bytes":…}]}`
(`profiles` 배열로 한 서버의 여러 DB에 같은 보고를 붙일 수 있습니다).

## 무엇을 감지하나

| 규칙 | 조건 | 심각도 |
| --- | --- | --- |
| `capacity_forecast` | 최소제곱 회귀로 계산한 고갈까지 남은 일수 ≤ `warn_days`/`critical_days`. 최근 6시간 추세(급증 포착, R² ≥ 0.8일 때만)와 7일 추세 중 빠른 쪽 | 경고 / 긴급 |
| `capacity_usage` | 선언 한도 대비 사용률 ≥ 80% / 90% | 경고 / 긴급 |
| `capacity_growth_surge` | 6시간 증가 속도가 7일 추세의 3배 이상이고 하루 1GiB·사용량 2% 이상 (한도 없어도 동작) | 경고 |
| `temp_spill` | 임시 파일이 5GiB 또는 한도의 5% 이상 | 경고 |
| `maint_replication_slot` | 비활성 슬롯이 WAL 1GiB/8GiB 이상 보존, 연결된 소비자가 8GiB 이상 뒤처짐, 13+ 에서 `wal_status` lost·unreserved | 경고 / 긴급 |
| `maint_wal_archive` | `archive_command` 실패 중(10분 넘게 성공 없음이면 긴급), 아카이브 대기 WAL 1GiB/8GiB 이상, `archive_mode=on` 인데 명령이 비어 있음 | 경고 / 긴급 |
| `maint_wal_retention` | `pg_wal` 이 `max_wal_size + wal_keep_size` 의 2배·4배 이상 | 경고 / 긴급 |
| `maint_vacuum_blocker` | 1시간/6시간 넘게 열린 트랜잭션, 30분/6시간 넘은 prepared 트랜잭션 (VACUUM 이 공간을 회수하지 못함) | 경고 / 긴급 |
| `maint_wraparound` · `maint_bloat` | 기존 예방 점검 결과 | 기존과 동일 |
| `maint_config_risk` | 슬롯이 있는데 `max_slot_wal_keep_size=-1`, `autovacuum=off` | 정보 / 경고 |
| `table_growth_surge` | 테이블이 24시간 동안 평소 하루 증가량의 3배 이상(최소 1GiB) 증가 | 경고 |
| `table_shrink` | 24시간 안에 크기가 절반 이하로(최소 1GiB) 감소 — TRUNCATE·대량 삭제 의심 | 경고 |
| `schema_change` | 물리 스키마 변경(컬럼·테이블 삭제, 타입·키 변경, 테이블 추가 …). 실행된 변경계획에 없는 변경을 "계획 외"로 표시 | 아래 참조 |
| `capacity_usage` · `capacity_forecast` (`volume:<마운트>`) | DB 서버 디스크 보고 기준 사용률·고갈 예측 | 경고 / 긴급 |
| `host_disk_stale` | 디스크 보고가 15분 넘게 끊김 | 경고 |
| `collection_down` | 3회 연속 수집 실패 — 이 DB는 지금 예방 경보가 동작하지 않음 (디스크가 차서 DB가 멈춘 경우 포함) | 운영: 긴급, 그 외: 경고 |
| `capacity_limit_undeclared` · `monitor_privilege` | 한도 미선언, `pg_monitor` 없음 | 정보 |

**스키마 변경 심각도**: 실행된 변경계획(대상·단계 명령)에 테이블 이름이 있으면
정보. 계획 외라면 삭제는 긴급, 타입·키·뷰 변경은 경고, 그 밖의 변경은 운영 환경
(`environment=production` 또는 `criticality=critical`)에서 경고·그 외 정보입니다.
운영이 아닌 프로파일은 경고가 상한입니다. 마이그레이션 도구가 같은 시각에 기록한 변경은 배포로 봅니다
(아래 "배포 마이그레이션 인식"). 첫 점검은 기준선만 만들고, 부분 수집된
스냅숏은 비교하지 않습니다(사라진 테이블로 오인하지 않도록). 주석 변경은 무시합니다.

## 경보 수명주기

- **발생**: 처음 감지되면 한 번 알립니다 (기본 경고 이상, `SQLON_ALERT_MIN_SEVERITY`).
- **격상**: 더 심각해지면 즉시 다시 알리고 이전 확인(ack)을 해제합니다.
- **지속**: 확인하지 않은 경보는 6시간마다 다시 알립니다 (`SQLON_ALERT_RENOTIFY`, 0 = 끔).
- **해소**: 그 경보를 만든 점검이 **실제로 다시 실행되어** 더 이상 보고하지 않을 때만
  해소합니다. DB에 접속할 수 없어 점검이 돌지 못한 주기에는 아무 경보도 지우지 않습니다.
  알렸던 경보는 해소도 알립니다.
- **확인(ack)**: 콘솔이나 API로 확인하면 지속 알림이 멈춥니다. 스키마 변경 같은 이벤트는
  확인 시 종료되고, 확인하지 않아도 24시간 뒤 닫힙니다.
- **무음(silence)**: 계획 작업(볼륨 증설·마이그레이션·페일오버 훈련) 동안 DB·규칙 단위로
  알림만 멈춥니다(최대 7일, 사유 필수, 감사 로그 기록). 경보는 콘솔에 계속 표시되고, 무음이
  끝날 때 여전히 유효한 것은 그때 보냅니다. 규칙은 `capacity_*` 처럼 접두어로도 지정합니다.
  모든 DB 대상 무음은 관리자만 만들 수 있습니다.
- 한 주기의 알림은 메시지 하나로 묶어 보냅니다. 전달 실패는 1·2·4…32분 간격으로 재시도하며
  콘솔에 실패 상태를 표시합니다. 경보 상태는 `<data>/operations/earlywarning/` 에 저장되어
  재시작해도 같은 경보를 다시 보내지 않습니다.

## DB별 알림 채널

프로파일의 `alerting` 으로 DB마다 다른 채널과 최소 위험도를 지정합니다.

```json
"alerting": { "webhook_ref": "env:TEAM_ORDERS_WEBHOOK", "escalation_ref": "env:ORDERS_ONCALL_WEBHOOK", "min_severity": "warning" }
```

- `webhook_ref` 는 비밀번호와 같은 참조(`env:`·`file:`·`plain:`)입니다. 웹훅 URL은 경로에 비밀이
  있으므로 `plain:` 값은 API·화면에서 `plain:****` 로 가려지고, 그대로 다시 저장하면 기존 값이 유지됩니다.
- 채널을 지정한 DB의 경보는 **그 채널로만** 갑니다(서버 기본 채널로는 가지 않습니다). 전체 현황은
  콘솔과 일일 리포트로 봅니다. 채널을 해석할 수 없으면(환경변수 없음 등) 경보를 잃지 않도록 기본
  채널로 보내고 콘솔에 오류를 표시합니다. 채널마다 전달 상태·재시도가 따로 관리됩니다.
- `min_severity` 로 개발 DB는 `critical` 만 보내는 식으로 소음을 줄입니다.
- `/admin/alerts` 의 알림 테스트는 `POST /api/early-warning/test-notification?profile=<id>` 로 DB별 채널도 시험합니다.

## 일일 용량 리포트

매일 `SQLON_ALERT_DIGEST_AT`(기본 09:00, 표시 시간대 기준)에 기본 채널로 DB별 사용률·7일 추세·
가득 차는 날짜·한도 미선언 DB·수집 실패 DB·전달 실패 채널을 한 메시지로 보냅니다. 경보가 "지금
조치"라면 리포트는 60일 뒤 고갈 같은 느린 추세를 놓치지 않게 합니다. 그 시각부터 2시간 안에만
보내므로, 서버가 그 시간에 꺼져 있었으면 그날은 건너뜁니다. `off` 로 끕니다.

## 당직 호출 (확인되지 않은 긴급 경보)

팀 채널은 음소거되어 있거나 새벽에는 아무도 보지 않습니다. 밤새 차오르는 디스크가 바로 그 경우라서,
**긴급(critical) 경보가 일정 시간 확인(ack)되지 않으면** 별도의 당직 채널로 한 번 더, 더 크게 알립니다.

```sh
SQLON_ALERT_ESCALATION_WEBHOOK=https://mattermost.example.com/hooks/oncall-xxxx   # 당직 채널
SQLON_ALERT_ESCALATE_AFTER=30m    # 기본 30m · 0 = 즉시 · off = 끔
```

- 기준 시각은 경보가 **긴급이 된 순간**입니다(경고에서 격상된 경우 격상 시각). 긴급에서 내려가면 초기화됩니다.
- 경보 하나당 한 번만 호출하고, 호출한 경보가 해소되면 당직 채널에 **호출 해소**를 보냅니다.
- 확인(ack)하거나 무음을 건 경보는 호출하지 않습니다 — 누군가 보고 있다는 뜻이기 때문입니다.
- DB마다 다른 당직 채널은 프로파일의 `alerting.escalation_ref` 로 지정합니다(`webhook_ref` 와 같은 참조 형식).
  없으면 서버의 당직 채널을 씁니다.
- 실패한 호출은 다른 채널과 같은 간격으로 재시도하며 콘솔의 채널 상태에 "📟 당직 호출" 로 표시됩니다.
- `test_alert_channel`(MCP, `channel=escalation`)이나 `POST /api/early-warning/test-notification?channel=escalation` 으로 경로를 시험합니다.

## SQLON 자신의 생존 신호 (heartbeat)

예방 경보가 DB를 지켜보는 동안, SQLON 자신이 멈추면 아무 경보도 오지 않고 그 침묵은 "이상 없음" 과
구별되지 않습니다. 외부 감시 서비스(healthchecks.io, Uptime Kuma push, Cronitor 등)의 핑 URL을 주면
**평가 주기마다** GET 으로 호출하고, 핑이 끊기면 그 서비스가 알려줍니다(dead man's switch).

```sh
SQLON_HEARTBEAT_URL=https://hc-ping.com/<uuid>     # 감시 서비스에서 주기 1분·유예 5분 정도로 설정
```

핑은 **SQLON이 실제로 경고할 수 있을 때만** 보냅니다. 평가가 실패했거나 기본 알림 채널 전달이 실패
중이면 핑을 보류합니다 — 경보가 나가지 못하는 상태도 감시 서비스가 잡아냅니다. 보류 사유와 마지막 핑
시각은 콘솔의 채널 상태(💓)에 표시됩니다. URL의 경로는 비밀로 취급해 화면·오류 메시지에서 가립니다.

## Mattermost 버튼 (확인·무음·수정안)

`SQLON_ALERT_CHAT_ACTIONS=mattermost` 면 알림 메시지의 경보마다 버튼이 붙습니다.

| 버튼 | 동작 |
| --- | --- |
| ✅ 확인 | 경보를 ack — 기록되는 확인자는 `mattermost:<사용자명>` |
| 🔕 2시간 무음 | 그 DB·규칙의 알림을 2시간 멈춤 (경보는 콘솔에 계속 표시) |
| 🛠 수정안 만들기 | 자동 수정이 있는 경보(슬롯·VACUUM 차단·블로트·설정 위험·모니터링 권한)만. **변경계획 초안(draft)만** 만들고 실행하지 않습니다. 같은 경보에 진행 중인 계획이 있으면 새로 만들지 않고 그 계획을 알려줍니다 |

설정:

1. Mattermost가 SQLON에 닿는 주소를 지정합니다. 기본값은 `SQLON_ALERT_CONSOLE_URL` 의 origin 이고,
   다르면 `SQLON_ALERT_ACTION_URL=https://sqlon.internal:6767` 처럼 줍니다. 버튼은
   `POST <주소>/api/early-warning/chat-action` 을 호출합니다.
2. SQLON이 사설 IP라면 Mattermost 시스템 콘솔의 **Allow untrusted internal connections to**
   (`ServiceSettings.AllowedUntrustedInternalConnections`)에 SQLON 호스트를 추가해야 버튼이 동작합니다.

콜백은 Mattermost가 보내므로 SQLON 자격 증명이 없습니다. 대신 버튼마다 **서명된 토큰**(HMAC-SHA256,
7일 만료)이 들어 있어, 그 토큰은 경보 하나에 대한 동작 하나만 허용합니다. 서명 키는 버튼을 켤 때
`<data>/operations/earlywarning/action.key`(권한 0600)에 만들어지며, 이 파일을 지우면 이미 게시된 모든
버튼이 무효가 됩니다. 버튼을 끄면(`off`) 게시된 버튼도 즉시 거부됩니다. 위조·만료 토큰은 403 으로
거부하고 감사 로그(`early_warning_chat_action_rejected`)에 남깁니다. Slack incoming webhook은 버튼을
지원하지 않으므로 Slack에는 켜지 마세요.

## 원인 추정과 흔들림 억제

함께 발생한 경보는 알려진 원인→결과 관계로 묶어 **사건(incident)** 으로 보여줍니다. 예를 들어
"WAL 아카이브 실패 → pg_wal 과다 → 고갈 예측 → 사용률 임계 초과" 는 하나의 사건이고, 고칠 것은
맨 앞의 원인 하나입니다. 증상 경보의 알림에는 `🔗 원인 추정: …` 줄이 붙고, 콘솔 맨 위와 헤드라인에
사건이 표시됩니다. 설정 위험은 그 설정이 실제로 만드는 결과로만 연결합니다(`autovacuum=off` →
블로트, 무제한 `max_slot_wal_keep_size` 는 원인이 아니라 잠재 위험). 디스크가 실제로 가득 찬
경우(사용률 ≥ 98% 또는 남은 일수 0)만 "DB 정지(관측 중단)" 의 원인으로 봅니다.

사용률이 임계값 근처에서 오르내리면 같은 경보가 발생·해소를 반복합니다. 1시간 안에 3번 이상
발생한 경보는 **흔들림(flapping)** 으로 보고 발생·해소·지속 알림을 보류합니다(격상은 보냄). 안정되면
여전히 유효한 경보를 그때 보냅니다. 콘솔에는 "흔들림 — 알림 보류" 로 표시됩니다.

## 무엇이 늘었나 (증가 원인)

"사흘 뒤 가득 찬다" 다음 질문은 "무엇이 늘고 있나" 입니다. DB 점유량과 디스크 볼륨의 최근 7일(이력이
짧으면 있는 만큼, 최소 1시간) 증가분을 SQLON이 이미 추적하는 부분으로 나눕니다 — 크기 상위 테이블,
WAL·binlog, 임시파일, 로그. 디스크 볼륨은 먼저 **DB 점유량** 과 **DB 밖 파일**(덤프 백업·외부 로그)로
나눕니다. 이번 주에 새로 생긴 테이블(예: `CREATE TABLE … AS` 백업 사본)은 0부터 자란 것으로 셉니다.

```
최근 7.0일 증가 +42.0 GiB 중 public.events +34.5 GiB(82%), WAL +5.1 GiB(12%), …
```

저장공간 경보의 상세, 콘솔 예측 표의 "증가 원인", 용량 계획, `explain_early_warning`, 일일 리포트("증가
1위")에 표시됩니다. 테이블 크기는 인덱스·TOAST를 포함하며, 비율의 합이 100%가 안 되면 나머지는 추적하지 않는 작은 테이블과
시스템 카탈로그 등입니다.

## 배포 마이그레이션 인식

스키마 변경을 감지하면 수집된 스키마에 있는 마이그레이션 도구의 이력 테이블을 읽기 전용으로 조회합니다.

| 도구 | 이력 테이블 |
| --- | --- |
| Flyway | `flyway_schema_history` (실패한 행 제외) |
| Liquibase | `databasechangelog` |
| Django | `django_migrations` |
| Prisma | `_prisma_migrations` |
| Knex | `knex_migrations` |

직전 점검 이후 **새로 기록된** 마이그레이션이 있으면 그 변경은 배포입니다: 제목에 "(배포 마이그레이션 N건)",
상세에 적용된 마이그레이션(도구·버전·설명·실행자·시각)을 붙이고, 추가 위주의 변경은 정보로 낮춥니다.
단 **테이블·컬럼 삭제는 배포여도 경고**입니다 — 잘못된 마이그레이션이 데이터를 지운 경우가 바로 봐야 할
이상 징후이기 때문입니다. 직전 점검에 이미 있던 마이그레이션은 이후의 변경을 설명하지 못하므로, 배포
직후의 수동 `ALTER` 는 그대로 "계획 외" 로 남습니다. PostgreSQL·MySQL·MariaDB에서 동작하며, 이력
테이블이 수집 범위 밖의 스키마에 있으면 보지 못합니다.

## 용량 계획

`plan_capacity`(MCP)·`GET /api/early-warning/capacity-plan?profile=&days=`·콘솔의 "용량 계획" 버튼은
현재 추세로 N일(기본 90)을 버티려면 필요한 볼륨 크기(경고 임계 아래로 유지), 부족분, 경고 임계
도달일·가득 차는 날짜, 추세 1·2·3배 시나리오를 계산합니다. 이미 경고 임계를 넘은 자산은 추세 이력이
없어도 증설 필요로 판단합니다.

## 경보에서 수정까지 (승인 게이트)

`propose_early_warning_fix`(DBA)·`POST /api/early-warning/alerts/{id}/fix`·콘솔 "상세 → 수정 변경계획
초안 만들기" 는 경보를 고치는 **초안(draft) 변경계획** 을 만듭니다. 실행은 기존 변경 관리 흐름
(submit → approve → execute, 감사 기록, 보상 단계)을 그대로 거칩니다.

| 경보 | 수정안 | 위험도 |
| --- | --- | --- |
| 비활성·무효화된 복제 슬롯 | `pg_drop_replication_slot` (보상: 같은 이름·종류로 빈 슬롯 재생성 — WAL은 되돌릴 수 없음) | high |
| VACUUM 차단 세션 | `pg_terminate_backend(pid, 5000)` (14+), prepared 는 `ROLLBACK PREPARED` | medium / high |
| 테이블 블로트 | `VACUUM (ANALYZE) 테이블` | low |
| `max_slot_wal_keep_size=-1` | `ALTER SYSTEM SET max_slot_wal_keep_size` = 볼륨의 20%(또는 `value_mb`) + reload, 보상 RESET | medium |
| `autovacuum=off` | `ALTER SYSTEM SET autovacuum = on` + reload | medium |
| 모니터링 권한 부족 | `GRANT pg_monitor TO <모니터링 계정>` (보상 REVOKE) | low |

변경 실행기는 검증 쿼리가 오류 없이 실행되면 통과로 보므로, 이 수정안의 검증은 조치가 적용되지
않았으면 `verification failed: …` 오류를 냅니다. 연결된 소비자가 뒤처진 슬롯, WAL 아카이브 대상 장애,
저장공간 경보처럼 SQL 한 줄로 고칠 수 없는 경보는 무엇을 해야 하는지 안내만 합니다.

## MCP 로 운영하기 (에이전트·관리자)

모든 기능을 MCP 도구로 쓸 수 있습니다. 단독 HTTP 모드에서는 모두 관리 토큰이 필요하고, 메타 DB(로그인)
모드에서는 사용자가 권한을 가진 DB의 경보만 보입니다(다른 DB의 경보 ID는 없는 것처럼 응답).

| 도구 | 권한 | 용도 |
| --- | --- | --- |
| `get_early_warnings` | 프로파일 | 현황 + 사건(incidents) + 우선순위별 `next_actions` |
| `explain_early_warning` | 프로파일 | 원인 사슬·과거 이력·예측·시계열·다음 도구 |
| `plan_capacity` | 프로파일 | 증설 크기·시점 계산 |
| `acknowledge_early_warning` | 프로파일 | 확인(ack) |
| `manage_early_warning_silences` | 프로파일(전체 무음은 관리자) | 무음 list·create·end |
| `report_host_disk` | 프로파일 | 디스크(df) 보고 |
| `configure_early_warning` | 관리자 | 서버 설정 get·set·reset (재시작 없이) — 당직 호출·생존 신호·버튼 포함 |
| `configure_profile_alerting` | 관리자 | DB별 용량 한도·채널·당직 채널·최소 위험도 |
| `run_early_warning_check` | 관리자 | 즉시 평가, 새로 생긴/해소된 경보 |
| `test_alert_channel` | 관리자 | 채널 테스트 (`channel=escalation` 은 당직 채널) |
| `propose_early_warning_fix` | DBA | 승인 대기 수정안 생성 |

**전략적으로 쓰는 법**: MCP 프롬프트 `early_warning_triage` 가 순서를 안내합니다 —
① `get_early_warnings` 의 `next_actions` 를 위에서부터(원인 경보 우선) ② `explain_early_warning` 으로
원인 사슬 확인 ③ 고칠 수 있으면 `propose_early_warning_fix` → 사람의 승인(`submit_change` →
`approve_change` → `execute_approved_change`) ④ 고칠 수 없는 저장공간 문제는 `plan_capacity` ⑤ 계획
작업은 무음, 확인만 할 것은 ack ⑥ `run_early_warning_check` 의 `resolved_ids` 로 효과 확인 ⑦ 감시 공백
(`configure_profile_alerting`·`configure_early_warning` 제안)은 관리자에게 보고. `next_actions` 의 모든
항목은 그대로 호출할 수 있는 도구 이름과 인자입니다.

## 운영 설정

관리자는 아래 설정을 콘솔 "예방 경보 설정", `PUT /api/early-warning/settings`, MCP
`configure_early_warning` 으로 **재시작 없이** 바꿀 수 있습니다(런타임 값은 `settings.json` 에 저장되어
재시작 후에도 유지되고, 초기화하면 아래 기본값으로 돌아갑니다). 기본 채널의 `webhook_ref` 는
`env:`·`file:`·`plain:` 참조이며 `plain:` 값은 가려서 보여줍니다.


| 플래그 | 환경변수 | 기본값 |
| --- | --- | --- |
| `-early-warning` | `SQLON_EARLY_WARNING` (`off` 로 끔) | 켜짐 |
| `-alert-webhook` | `SQLON_ALERT_WEBHOOK` | `-digest-webhook` 값 |
| `-alert-console-url` | `SQLON_ALERT_CONSOLE_URL` | (없음) |
| `-alert-min-severity` | `SQLON_ALERT_MIN_SEVERITY` | `warning` |
| `-alert-renotify` | `SQLON_ALERT_RENOTIFY` | `6h` |
| `-alert-timezone` | `SQLON_ALERT_TZ` | `Asia/Seoul` |
| `-alert-digest-at` | `SQLON_ALERT_DIGEST_AT` (`off` 로 끔) | `09:00` |
| `-maintenance-interval` | `SQLON_MAINTENANCE_INTERVAL` | `5m` |
| `-schema-watch-interval` | `SQLON_SCHEMA_WATCH_INTERVAL` (0 = 끔) | `15m` |
| `-alert-escalation-webhook` | `SQLON_ALERT_ESCALATION_WEBHOOK` | (없음) |
| `-alert-escalate-after` | `SQLON_ALERT_ESCALATE_AFTER` (`0` = 즉시, `off` = 끔) | `30m` |
| `-alert-heartbeat-url` | `SQLON_HEARTBEAT_URL` | (없음) |
| `-alert-chat-actions` | `SQLON_ALERT_CHAT_ACTIONS` (`mattermost`·`off`) | `off` |
| `-alert-action-url` | `SQLON_ALERT_ACTION_URL` | 콘솔 링크의 origin |

런타임 설정 이름은 `escalation_ref`·`escalate_after`·`heartbeat_ref`·`chat_actions`·`action_url` 입니다.

## 조회 경로

- 콘솔: `/admin/alerts` — 저장공간 예측 표(DB 점유량과 디스크 볼륨, 증가 원인), 발생 중 경보(확인 버튼·무음·당직 호출 표시), 무음 관리, 스키마 변경 이력, 해소 이력, 기본·DB별·당직 채널 상태와 생존 신호·일일 리포트 상태, **지금 평가**·**알림 테스트** 버튼
- MCP 도구 11종(위 표)과 프롬프트 `early_warning_triage`
- REST: `GET /api/early-warning`, `GET /api/early-warning/alerts/{id}`, `POST /api/early-warning/alerts/{id}/ack`, `POST /api/early-warning/alerts/{id}/fix`, `GET /api/early-warning/capacity-plan`, `GET·PUT /api/early-warning/settings`, `POST /api/early-warning/silences`, `DELETE /api/early-warning/silences/{id}`, `POST /api/early-warning/disk`, `POST /api/early-warning/evaluate`, `POST /api/early-warning/test-notification`, `POST /api/early-warning/chat-action`(Mattermost 버튼 콜백, 서명 토큰으로 인가)
- Prometheus (`/metrics`): `sqlon_storage_used_bytes`, `sqlon_storage_limit_bytes`,
  `sqlon_storage_growth_bytes_per_day{window="6h|7d"}`, `sqlon_storage_days_to_full`,
  `sqlon_early_warning_alerts_firing{severity}`, `sqlon_early_warning_notifications_total{outcome}`

  기존 Alertmanager를 쓴다면 같은 예측으로 규칙을 걸 수 있습니다:

  ```yaml
  - alert: DatabaseVolumeFillsSoon
    expr: sqlon_storage_days_to_full < 3
    labels: { severity: critical }
  ```

## 저장공간 점유량(PostgreSQL)

`storage:footprint` = 인스턴스의 **모든** 데이터베이스 크기 + `pg_wal` + `pgsql_tmp`
+ 로그 디렉터리(`logging_collector=on` 일 때). 메시지와 콘솔은 이 구성을 함께 보여주므로
"WAL 180GiB" 처럼 원인이 바로 드러납니다. 선언한 한도는 이 값에 적용됩니다.
MySQL·MariaDB는 모든 스키마 + binlog 점유량에, Oracle은 엔진이 보고하는 테이블스페이스
최대 크기에 적용됩니다. DB 서버 디스크 보고가 있으면 볼륨마다 `df` 의 실제 크기로 따로 예측합니다.

## 한계

- **디스크 보고 없이는 볼륨의 다른 파일이 보이지 않습니다.** 같은 디스크에 덤프 백업·외부
  로그·다른 프로그램이 있다면 4단계의 디스크 보고를 설치하세요.
- 별도 볼륨에 둔 테이블스페이스도 SQL 점유량에서는 하나로 합산됩니다(디스크 보고는 볼륨별).
- MySQL·MariaDB의 relay log, 역할(role)로만 받은 binlog 권한은 감지하지 않습니다.
- 추세에는 이력이 필요합니다: 6시간 추세는 2시간·표본 6개, 7일 추세는 24시간·표본 12개
  이상부터 계산합니다. 처음 켤 때는 이미 저장된 수집 스냅숏(최근 8일)으로 이력을 복원합니다.
- 테이블 급증·급감과 증가 원인은 크기 상위 100개 테이블만 봅니다.
- 설정 수정안(`autovacuum`·`max_slot_wal_keep_size`)의 검증 단계는 값을 단정하지 않고 표시만 합니다.
  `pg_reload_conf()` 의 새 값은 같은 세션에서 바로 보이지 않기 때문입니다 — 실행 후 다음 예방 점검에서
  경보가 해소되는지로 확인하세요.
