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
| `collection_down` | 3회 연속 수집 실패 — 이 DB는 지금 예방 경보가 동작하지 않음 (디스크가 차서 DB가 멈춘 경우 포함) | 운영: 긴급, 그 외: 경고 |
| `capacity_limit_undeclared` · `monitor_privilege` | 한도 미선언, `pg_monitor` 없음 | 정보 |

**스키마 변경 심각도**: 실행된 변경계획(대상·단계 명령)에 테이블 이름이 있으면
정보. 계획 외라면 삭제는 긴급, 타입·키·뷰 변경은 경고, 그 밖의 변경은 운영 환경
(`environment=production` 또는 `criticality=critical`)에서 경고·그 외 정보입니다.
운영이 아닌 프로파일은 경고가 상한입니다. 첫 점검은 기준선만 만들고, 부분 수집된
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
- 한 주기의 알림은 메시지 하나로 묶어 보냅니다. 전달 실패는 1·2·4…32분 간격으로 재시도하며
  콘솔에 실패 상태를 표시합니다. 경보 상태는 `<data>/operations/earlywarning/` 에 저장되어
  재시작해도 같은 경보를 다시 보내지 않습니다.

## 운영 설정

| 플래그 | 환경변수 | 기본값 |
| --- | --- | --- |
| `-early-warning` | `SQLON_EARLY_WARNING` (`off` 로 끔) | 켜짐 |
| `-alert-webhook` | `SQLON_ALERT_WEBHOOK` | `-digest-webhook` 값 |
| `-alert-console-url` | `SQLON_ALERT_CONSOLE_URL` | (없음) |
| `-alert-min-severity` | `SQLON_ALERT_MIN_SEVERITY` | `warning` |
| `-alert-renotify` | `SQLON_ALERT_RENOTIFY` | `6h` |
| `-alert-timezone` | `SQLON_ALERT_TZ` | `Asia/Seoul` |
| `-maintenance-interval` | `SQLON_MAINTENANCE_INTERVAL` | `5m` |
| `-schema-watch-interval` | `SQLON_SCHEMA_WATCH_INTERVAL` (0 = 끔) | `15m` |

## 조회 경로

- 콘솔: `/admin/alerts` — 저장공간 예측 표, 발생 중 경보(확인 버튼), 스키마 변경 이력, 해소 이력, 알림 채널 상태, **지금 평가**·**알림 테스트** 버튼
- MCP 도구: `get_early_warnings` (`profile` 선택)
- REST: `GET /api/early-warning`, `POST /api/early-warning/alerts/{id}/ack`, `POST /api/early-warning/evaluate`, `POST /api/early-warning/test-notification`
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
MySQL·MariaDB는 연결된 데이터베이스 크기에, Oracle은 엔진이 보고하는 테이블스페이스
최대 크기에 적용됩니다.

## 한계

- **볼륨의 다른 파일은 보이지 않습니다.** 같은 디스크에 덤프 백업·외부 로그·다른
  프로그램이 있다면 SQL로는 알 수 없습니다. 한도를 그만큼 보수적으로 잡거나, 호스트
  디스크 감시(node_exporter 등)와 함께 쓰세요.
- 별도 볼륨에 둔 테이블스페이스도 하나의 점유량으로 합산됩니다.
- MySQL·MariaDB의 binlog·relay log 크기는 아직 점유량에 포함하지 않습니다.
- 추세에는 이력이 필요합니다: 6시간 추세는 2시간·표본 6개, 7일 추세는 24시간·표본 12개
  이상부터 계산합니다. 처음 켤 때는 이미 저장된 수집 스냅숏(최근 8일)으로 이력을 복원합니다.
- 테이블 급증·급감은 크기 상위 100개 테이블만 봅니다.
