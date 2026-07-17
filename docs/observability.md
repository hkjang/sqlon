# 워크로드·용량 수집

SQLON은 대상 데이터베이스의 누적 시스템 카운터, 대기 이벤트, Top SQL 통계와
용량을 고정된 읽기 전용 엔진 Provider로 수집합니다. 기존 `workload_report`가
SQLON 자체 쿼리 감사 로그를 요약하는 기능인 반면, 이 수집기는 대상 DB가 직접
제공하는 운영 통계를 저장합니다.

엔진별 Provider 구현(고정 시스템 쿼리와 정규화)은 `internal/engine/<엔진>`
패키지에 있으며 `internal/engine/adapters`가 단일 배선 지점으로 등록합니다.
자세한 구조는 [architecture.md](architecture.md)의 "엔진 어댑터 계층"을
참고하세요.

## 수집 흐름

```text
프로파일 목록 → Capability 확인 → 엔진 Provider 시스템 조회
→ 공통 Snapshot 정규화 → 이전 Snapshot과 rate 계산
→ evidence/limitation 생성 → append-only 운영 저장소
```

- 기본 수집 주기는 1분이며 프로파일별 작업은 격리·동시 실행됩니다.
- 느리거나 실패한 DB는 다른 프로파일의 결과를 제거하지 않습니다.
- 누적 카운터 두 점의 시간 차이로 QPS/TPS와 초당 I/O를 계산합니다.
- 용량 두 점의 차이로 객체별 일간 증가량을 계산합니다.
- 현재 증가율이 계속된다고 가정해 30일 이내 고갈 예상 대상을 evidence로 생성합니다.
- 첫 스냅숏, 카운터 리셋, 시간 역전은 임의로 보간하지 않고 limitation을 반환합니다.
- 사용률 80% 이상은 warning, 90% 이상은 critical evidence입니다.

## 엔진별 기본 원천

| 엔진 | 워크로드·대기·Top SQL | 용량 |
| --- | --- | --- |
| PostgreSQL | `pg_stat_database`, `pg_stat_activity`, 선택적 `pg_stat_statements` | `pg_database_size`, `pg_total_relation_size` |
| MySQL | Performance Schema global status, waits, statement digest | `information_schema.tables` |
| MariaDB | MySQL 계열 Performance Schema | `information_schema.tables` |
| Oracle | `V$SYSSTAT`, `GV$SESSION`, `V$SYSTEM_EVENT`, `GV$SQL` | `DBA_TABLESPACE_USAGE_METRICS` |

Top SQL은 SQL 원문과 bind 값을 저장하지 않고 fingerprint/SQL ID, plan hash와
누적 통계만 저장합니다. PostgreSQL `pg_stat_statements`나 Performance Schema가
비활성인 경우 전체 수집을 성공으로 위장하지 않고 Top SQL limitation을 남깁니다.

Oracle 기본 Provider는 AWR, ASH, ADDM, `DBA_HIST_*`,
`DBMS_WORKLOAD_REPOSITORY`, SQL Tuning Advisor를 사용하지 않습니다. 모든 쿼리는
추가로 프로파일 `license_policy` 검사를 통과해야 합니다.

## 복제 상태

`get_replication_status`(MCP) / `GET /api/observability/replication`(REST)은
복제 역할(primary/replica/standby/standalone)과 구성 요소별 상태를
반환합니다.

| 엔진 | 원천 | 노드 종류 |
| --- | --- | --- |
| PostgreSQL | `pg_is_in_recovery`, `pg_stat_replication`, `pg_replication_slots`, `pg_stat_wal_receiver`, `pg_last_xact_replay_timestamp` | standby, slot, receiver |
| MySQL | `SHOW REPLICA STATUS`(구버전 `SHOW SLAVE STATUS` 폴백), PROCESSLIST의 Binlog Dump 스레드 | channel, primary_feed |
| MariaDB | `SHOW ALL SLAVES STATUS`(멀티 소스) 폴백 포함 | channel, primary_feed |
| Oracle | `V$DATABASE`, `V$DATAGUARD_STATS`(transport/apply lag), `V$ARCHIVE_DEST_STATUS` | dataguard_lag, archive_dest |

동작 원칙:

- 측정할 수 없는 지연은 `lag_seconds = -1`로 반환하고 limitation을 남깁니다 —
  0으로 위장하지 않습니다 (No Silent Failure).
- 비정상 구성 요소(IO/SQL 스레드 중단, WAL 재생 일시정지, 비활성 복제 슬롯,
  archive destination 오류)가 하나라도 있으면 응답 상태가 `critical`이 되고
  `REPLICATION_BROKEN` evidence가 생성됩니다.
- 측정된 지연이 300초 이상이면 `warning` / `REPLICATION_LAG_HIGH`.
- `SHOW REPLICA/SLAVE STATUS`는 읽기 전용 명령이며, 연결 세션 자체가 읽기
  전용 DSN으로 열립니다. Oracle은 base 라이선스 뷰만 사용합니다.

## 백업 상태

`get_backup_status`(MCP) / `GET /api/observability/backup`(REST)은 DB 서버가
**스스로 보고할 수 있는** 백업·아카이브 상태를 반환합니다. SQLON은 백업
엔진이 아니며, 외부 백업 도구(pgBackRest·Barman·XtraBackup 등)의 잡 상태는
추측하지 않고 limitation으로 명시합니다.

| 엔진 | 원천 | 항목 종류 |
| --- | --- | --- |
| PostgreSQL | `archive_mode`, `pg_stat_archiver`(성공/실패 시각) | wal_archiver |
| MySQL / MariaDB | `@@log_bin`, `SHOW BINARY LOG STATUS`(구버전 `SHOW MASTER STATUS`) | binlog |
| Oracle | `V$DATABASE`(log_mode), `V$RMAN_BACKUP_JOB_DETAILS`(최근 5건), `V$RECOVERY_FILE_DEST`(FRA 사용률) | rman_job, fra |

- `archiving`은 시점 복구(PITR)의 기반인 지속 아카이빙 상태입니다:
  비활성이면 `warning` / `BACKUP_PITR_DISABLED`.
- 아카이버 실패, FAILED RMAN 작업, FRA 사용률 90% 초과는 `critical` /
  `BACKUP_FAILURE_DETECTED`.

## 운영 저장소와 보존

단독 모드는 `<data>/operations/snapshots/YYYYMMDD.jsonl`에 append-only JSONL로
저장합니다. 디렉터리는 `0700`, 파일은 `0600`으로 생성합니다. 손상된 행은
건너뛰되 경고 없이 숨기지 않습니다. 기본 보존기간은 30일이며 날짜 단위 파일을
정리합니다.

```sh
sqlon \
  -observe-interval 1m \
  -observe-retention-days 30
```

환경변수 `SQLON_OBSERVE_INTERVAL`, `SQLON_OBSERVE_RETENTION_DAYS`도 지원하며
기존 `JAMYPG_*` 이름은 호환 별칭입니다. `-observe-interval 0`은 백그라운드
수집을 끕니다. 수동 수집은 명시적인 `fresh=true` 또는 화면의 **지금 수집**만
대상 DB를 조회합니다.

## API와 MCP

| 인터페이스 | 내용 |
| --- | --- |
| `GET /api/observability/workload?profile=...` | 최신 저장 카운터·rate·대기 |
| `GET /api/observability/top-sql?profile=...` | 원문 없는 Top SQL 통계 |
| `GET /api/observability/capacity?profile=...` | 용량·사용률·일간 증가량 |
| `GET /api/observability/history?profile=...&hours=24` | 저장 시계열 |
| `GET /api/observability/replication?profile=...` | 복제 역할·토폴로지·지연 |
| `GET /api/observability/backup?profile=...` | 백업·아카이브 상태 |
| `POST /api/collector/run` | 권한 범위 프로파일 즉시 일괄 수집 |
| MCP `get_workload_summary` | 워크로드 요약 |
| MCP `get_top_sql` | Top SQL |
| MCP `get_storage_status` | 저장공간 상태 |
| MCP `get_replication_status` | 복제 상태 |
| MCP `get_backup_status` | 백업·아카이브 상태 |

조회 기본값은 저장소 데이터이며 대상 DB를 암묵적으로 재조회하지 않습니다.
`fresh=true`일 때만 새 시스템 조회를 수행하고 저장합니다. REST와 MCP 모두 같은
프로파일 ACL, Provider, 저장소 서비스를 사용합니다.
