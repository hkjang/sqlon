# 워크로드·용량 수집

SQLON은 대상 데이터베이스의 누적 시스템 카운터, 대기 이벤트, Top SQL 통계와
용량을 고정된 읽기 전용 엔진 Provider로 수집합니다. 기존 `workload_report`가
SQLON 자체 쿼리 감사 로그를 요약하는 기능인 반면, 이 수집기는 대상 DB가 직접
제공하는 운영 통계를 저장합니다.

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
| `POST /api/collector/run` | 권한 범위 프로파일 즉시 일괄 수집 |
| MCP `get_workload_summary` | 워크로드 요약 |
| MCP `get_top_sql` | Top SQL |
| MCP `get_storage_status` | 저장공간 상태 |

조회 기본값은 저장소 데이터이며 대상 DB를 암묵적으로 재조회하지 않습니다.
`fresh=true`일 때만 새 시스템 조회를 수행하고 저장합니다. REST와 MCP 모두 같은
프로파일 ACL, Provider, 저장소 서비스를 사용합니다.
