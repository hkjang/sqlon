# DB 플릿 운영 현황

SQLON의 첫 화면(`/`)은 사용자가 접근할 수 있는 DB 프로파일을 대상으로 연결
상태와 운영 컨텍스트를 수집하고 위험도가 높은 순서로 보여줍니다. 자연어 질의나
SQL 실행 화면과 분리된 **관찰 우선** 진입점입니다.

## 프로파일 운영 컨텍스트

`/admin/db`에서 다음 값을 등록합니다. 이 값은 비밀번호나 접속 문자열과 달리
플릿 분류와 위험도 산정에 쓰이는 운영 메타데이터입니다.

| 필드 | 허용값·용도 |
| --- | --- |
| `service_name` | DB를 사용하는 업무 서비스 |
| `environment` | `production`, `staging`, `development`, `dr`, `test`, `unspecified` |
| `criticality` | `critical`, `high`, `medium`, `low` |
| `role` | `primary`, `standby`, `replica`, `cdb`, `pdb`, `unspecified` |
| `owner_team`, `location` | 담당 조직과 리전·데이터센터 |
| `maintenance_window` | 변경이 허용되는 운영 시간 |
| `tags` | 업무·보안·등급 분류용 문자열 목록 |

운영 환경 프로파일에는 `plain:` 비밀번호 참조를 저장할 수 없습니다. 조회 계정과
DBA 계정 모두 `env:` 또는 `file:` Secret 참조를 사용해야 합니다.

## 조회 인터페이스

| 인터페이스 | 목적 |
| --- | --- |
| `GET /api/fleet/instances` | 권한 범위의 플릿 인벤토리. DB 접속은 수행하지 않음 |
| `GET /api/fleet/health` | 프로파일별 독립 Ping과 구성 위험 판정 |
| MCP `list_database_instances` | REST 인벤토리와 같은 서비스 계층 사용 |
| MCP `get_fleet_health` | REST 상태 조회와 같은 서비스 계층 사용 |
| `GET /api/observability/sessions?profile=...` | 세션·SQL 실행시간·트랜잭션·대기·보호 상태 |
| `GET /api/observability/locks?profile=...` | 블로킹 관계, 루트 블로커와 영향 세션 수 |
| MCP `list_sessions`, `get_lock_tree` | REST 관찰 API와 동일한 Provider·권한 경로 사용 |

인증 모드에서는 소유·grant·shared 프로파일만 반환합니다. 관리자는 전체를 볼 수
있습니다. 단독 모드의 MCP 상태 조회는 대상 DB에 접속하므로 관리자 토큰이
필요합니다.

응답은 `status`, `data`, `warnings`, `limitations`, `collected_at`, `trace_id`를
포함합니다. 각 인스턴스는 `collection_status`, `risk_level`, `risk_score`,
`evidence`, `capabilities`, `driver_available`을 제공하므로 UI나 AI가 상태를
추측할 필요가 없습니다.

## 현재 위험 판정

- 엔진 어댑터나 배포판 드라이버 미지원은 정상 데이터가 아닌 `unsupported`로
  구분합니다. 표준판에서 Oracle 프로파일을 조회하면 `sqlon-oracle` 배포판이
  필요하다는 제한을 반환합니다.
- DNS·인증·네트워크·timeout 등 접속 실패는 `failed`이며 오류 범주와 해결
  단서를 증거에 포함합니다.
- 운영 환경과 중요도가 높은 프로파일의 연결 실패는 더 높은 위험 점수를 가집니다.
- 기존 데이터에 남아 있는 `plain:` 자격증명은 구성 위험으로 표시합니다. 신규
  운영 프로파일 저장은 검증 단계에서 차단됩니다.
- 프로파일별 probe는 제한된 동시성으로 격리되어 느린 DB 하나가 전체 응답을
  순차적으로 막지 않습니다.

## 데이터 해석 제한

플릿 상태는 프로파일 구성, **현재 연결 가능성**, 가장 최근의 워크로드·용량
스냅숏, **실시간 복제 상태**를 결합합니다. 스냅숏이 없거나 설정된 수집 주기의
2배보다 오래되면 정상으로 간주하지 않고 `operational_status`와 evidence로
표시합니다. 용량 80/90% 초과와 30일 이내 고갈 예측도 위험도에 반영합니다.

복제 상태는 헬스 점검 시 라이브로 관찰합니다: 복제 구성 요소가 비정상이면
`REPLICATION_BROKEN`(critical, 점수 88), 측정된 지연이 임계값을 넘으면
`REPLICATION_LAG_HIGH`(high, 점수 55)로 승격되고, 프로파일에 역할이 선언되지
않았으면 관찰된 역할(primary/replica/standby/standalone)로 보강됩니다. 복제
상태를 확인할 수 없는 경우(권한 부족 등)는 `REPLICATION_STATUS_UNAVAILABLE`
evidence로 가시화합니다. 세션·잠금은 전용 화면에서 실시간 조회하며, 백업
위험은 아직 플릿 점수에 포함되지 않으므로 연결 성공만으로 데이터베이스 전체가
정상이라고 해석해서는 안 됩니다.

## 세션·잠금 스냅숏

`/admin/sessions`는 PostgreSQL `pg_stat_activity`·`pg_blocking_pids`, MySQL
Performance Schema, MariaDB `INNODB_LOCK_WAITS`, Oracle `GV$SESSION`·
`GV$TRANSACTION`을 엔진 Provider 내부에서만 조회합니다. 외부 입력 SQL을 이
경로로 전달할 수 없습니다.

- SQL 실행 지속시간과 트랜잭션 지속시간을 별도 필드로 반환합니다.
- Oracle의 세션 키는 RAC에서도 안전한 `INST_ID:SID:SERIAL#`입니다.
- SYS, SYSTEM, background, replication 세션은 보호 대상으로 표시합니다.
- SQL 본문과 bind 값은 개인정보·Secret 노출을 막기 위해 반환하지 않습니다.
- 조회 권한 부족, 배포판 드라이버 미지원, 라이선스 정책 차단, 일반 수집 실패를
  서로 다른 `status`와 evidence code로 반환합니다.
- 블로킹 그래프는 전이 관계를 따라 루트 블로커와 전체 영향 세션 수를 계산합니다.
  화면은 관찰 전용이며 세션 취소·종료는 수행하지 않습니다.
- 단일 스냅숏은 엔진 쿼리 단계에서 10,000행으로 제한합니다. 상한에 도달하면
  정상 전체 데이터처럼 처리하지 않고 `*_SNAPSHOT_TRUNCATED` 근거와 limitation을
  반환합니다.

### 최소 조회 권한

| 엔진 | Provider가 조회하는 주요 뷰 | 운영 계정 점검 |
| --- | --- | --- |
| PostgreSQL | `pg_stat_activity`, `pg_blocking_pids` | 다른 세션 상세가 필요하면 `pg_monitor` 등 최소 모니터링 권한 검토 |
| MySQL | `information_schema.PROCESSLIST`, `INNODB_TRX`, Performance Schema lock 뷰 | `PROCESS`와 Performance Schema 조회 범위 검토 |
| MariaDB | `PROCESSLIST`, `INNODB_TRX`, `INNODB_LOCK_WAITS`, `INNODB_LOCKS` | `PROCESS` 및 InnoDB 상태 조회 권한 검토 |
| Oracle | `GV$SESSION`, `GV$TRANSACTION` | 필요한 `V_$`/`GV_$` 객체에 대한 직접 조회 권한을 최소 범위로 부여 |

Oracle Provider의 기본 쿼리는 AWR, ASH, ADDM, `DBA_HIST_*`, SQL Tuning
Advisor를 사용하지 않습니다. SQLON의 라이선스 정책 가드는 모든 시스템 쿼리에
추가 적용되며, `CONTROL_MANAGEMENT_PACK_ACCESS`만으로 계약상 라이선스를
보유했다고 판단하지 않습니다.
