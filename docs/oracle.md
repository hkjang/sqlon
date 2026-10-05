# Oracle 운영 지원

SQLON은 Oracle 실행 경로를 표준판과 분리합니다.

| 배포판 | 빌드 | Oracle |
| --- | --- | --- |
| `sqlon-standard` | `CGO_ENABLED=0` | 프로파일 검증과 정책만 제공, 연결 시 명확한 미지원 오류 |
| `sqlon-oracle` | `CGO_ENABLED=1 -tags oracle` | godror/OCI 연결 지원 |

godror는 `database/sql` 드라이버이며 CGO가 필요하고, 실행 환경에는 Oracle
Client 라이브러리가 필요합니다. 상세 요구사항은
[godror 설치 문서](https://godror.github.io/godror/doc/installation.html)를
기준으로 합니다.

```sh
sh scripts/build-oracle.sh
docker build -f Dockerfile.oracle -t sqlon/sqlon-oracle:dev .
```

## 프로파일

```json
{
  "id": "oracle-prod-01",
  "name": "운영 Oracle PDB",
  "type": "oracle",
  "connect_string": "dbhost:1521/PRODPDB",
  "username": "sqlon_monitor",
  "password_ref": "file:/run/secrets/oracle_monitor",
  "oracle": {
    "service_name": "PRODPDB",
    "connection_role": "normal",
    "cdb_scope": "pdb",
    "rac_enabled": true,
    "wallet_dir": "",
    "client_lib_dir": "/opt/oracle/instantclient"
  },
  "license_policy": {
    "diagnostics_pack": "disabled",
    "tuning_pack": "disabled",
    "source": "operator_declared"
  }
}
```

읽기 프로파일에는 `SYS`와 `SYSDBA` 연결을 허용하지 않습니다. Easy Connect,
TNS descriptor 및 OCI가 해석할 수 있는 Wallet/TCPS connect string을
`connect_string`에 지정할 수 있습니다.

## 라이선스 정책

기본값은 `base` 기능만 허용합니다.

- Diagnostics Pack 비활성: `DBA_HIST_*`, AWR, ADDM,
  `V$ACTIVE_SESSION_HISTORY` 및 `GV$ACTIVE_SESSION_HISTORY` 차단
- Tuning Pack 비활성: `DBMS_SQLTUNE`, `DBMS_SQLPA`, `DBMS_ADVISOR` 차단
- Tuning 기능은 두 Pack이 모두 `enabled`여야 허용
- `CONTROL_MANAGEMENT_PACK_ACCESS`는 참고값일 뿐 계약상 라이선스 보유의
  자동 증거로 사용하지 않음

정책은 일반 SQL, 내부 수집 쿼리, 승인된 변경 실행에 동일하게 적용되며 차단
시 `oracle_license_policy_denied` 오류와 감사 이벤트가 생성됩니다.

## 읽기 전용 제한

Oracle 일반 쿼리 경로는 단일 `SELECT` 또는 `WITH ... SELECT`만 허용합니다.
PL/SQL, DML/DDL/DCL, `FOR UPDATE`, DB link와 파일·관리 패키지는 차단합니다.
운영 계정 자체에도 SELECT 최소 권한만 부여해야 합니다.
