package mcp

// openAPISpec documents the REST management API served under /api. It is
// rendered by the embedded Swagger UI at /docs and downloadable at
// /openapi.json. Keep it in sync with registerAdmin.
var openAPISpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "SQLON AI Database Operations API",
    "version": "` + Version + `",
    "description": "SQLON 관리·운영 REST API.\n\n인증 모드(meta DB 활성): 로그인 세션 쿠키, MCP 키(Authorization: Bearer ssk_... 또는 X-MCP-Key), 또는 마스터 토큰(X-Admin-Token)으로 인증합니다. admin 역할이 전체 관리 권한을 가지며, 일반 사용자는 본인 소유·grant·shared 프로파일과 본인 키만 다룹니다.\n단독 모드(meta DB 미설정): 로그인이 없고 -admin-token(또는 SQLON_ADMIN_TOKEN)으로 변경 API를 보호합니다.\n\n- 데이터셋 변경은 자동 백업 후 적용되고 카탈로그를 재기동 없이 핫스왑하며, 실패 시 자동 롤백됩니다.\n- 웹 콘솔: / (플릿 운영 현황), /admin (데이터셋), /admin/db (DB 연결·SQL Lab), /admin/users (사용자), /admin/keys (MCP 키), /auth/login (로그인)"
  },
  "servers": [{ "url": "/" }],
  "tags": [
    { "name": "auth", "description": "로그인/로그아웃/현재 사용자/비밀번호/Keycloak SSO (meta DB 활성 시)" },
    { "name": "users", "description": "사용자 관리 — admin 전용 (meta DB 활성 시)" },
    { "name": "mcp-keys", "description": "MCP API 키 라이프사이클: 발급/조회/회전/폐기" },
    { "name": "grants", "description": "DB 프로파일 사용자 권한(use/manage) 부여·회수" },
    { "name": "settings", "description": "런타임 설정(마스터 토큰·허용 Origin·Keycloak SSO)을 메타 DB에 저장·즉시 적용 — admin 전용" },
    { "name": "datasets", "description": "데이터셋 조회/교체/제거/복원" },
    { "name": "catalog", "description": "카탈로그 상태와 리로드" },
    { "name": "fleet", "description": "권한 범위의 DB 플릿 인벤토리와 근거 기반 연결·구성 위험 상태" },
    { "name": "changes", "description": "승인 기반 변경 통제: 변경계획 생성·제출·승인·실행·롤백·취소 (DBA 권한 필요)" },
    { "name": "db-profiles", "description": "DB 접속 프로파일(PostgreSQL/MySQL/MariaDB/Oracle) CRUD와 접속 테스트 (/admin/db 화면과 동일 기능)" },
    { "name": "query", "description": "Read-Only 쿼리 실행: 검증/실행/미리보기/실행계획/메타데이터/이력/취소" },
    { "name": "activity", "description": "MCP 호출 이력·통계 (개인화): 본인 기본, admin은 all/user 필터" }
  ],
  "components": {
    "securitySchemes": {
      "AdminToken": { "type": "apiKey", "in": "header", "name": "X-Admin-Token", "description": "마스터 토큰(-admin-token). 인증 모드에서는 합성 admin으로 동작." },
      "SessionCookie": { "type": "apiKey", "in": "cookie", "name": "sqlon_session", "description": "로그인 세션 쿠키 (POST /auth/login으로 발급)." },
      "MCPKey": { "type": "http", "scheme": "bearer", "bearerFormat": "ssk", "description": "MCP 키(ssk_...). Authorization: Bearer 또는 X-MCP-Key 헤더." }
    },
    "schemas": {
      "DatasetStatus": {
        "type": "object",
        "properties": {
          "name": { "type": "string", "example": "metrics" },
          "file": { "type": "string", "example": "metrics.json" },
          "required": { "type": "boolean", "description": "true면 서버 기동에 필수라 제거 불가" },
          "editable": { "type": "boolean", "description": "false면 시스템 관리 대상(feedback/audit)이라 교체/제거 불가" },
          "format": { "type": "string", "enum": ["json-array", "json-object", "jsonl-dir"] },
          "description": { "type": "string" },
          "schema": { "type": "string", "description": "기대하는 JSON 구조 요약" },
          "used_by": { "type": "array", "items": { "type": "string" }, "description": "이 데이터를 사용하는 MCP 도구" },
          "present": { "type": "boolean" },
          "size_bytes": { "type": "integer" },
          "modified": { "type": "string" },
          "loaded": { "description": "카탈로그에 로드된 건수" },
          "issues": { "type": "array", "description": "이 파일에 귀속된 로드 이슈" }
        }
      },
      "MutationResult": {
        "type": "object",
        "properties": {
          "applied": { "type": "boolean", "description": "false면 자동 롤백됨(reason/issues 참조)" },
          "dataset": { "type": "string" },
          "backup": { "type": "string", "description": "적용 전 파일의 백업 경로" },
          "loaded": { "type": "object", "description": "핫스왑된 카탈로그 요약" },
          "reason": { "type": "string" },
          "issues": { "type": "array" }
        }
      },
      "Error": { "type": "object", "properties": { "error": { "type": "string" } } }
    }
  },
  "paths": {
    "/auth/login": {
      "post": {
        "tags": ["auth"], "summary": "로그인 (세션 쿠키 발급)",
        "description": "로컬 계정 아이디/비밀번호로 로그인. 성공 시 HttpOnly 세션 쿠키(sqlon_session)를 설정합니다. meta DB 미설정 시 503.",
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object", "required": ["username","password"], "properties": { "username": {"type":"string"}, "password": {"type":"string"} } } } } },
        "responses": { "200": {"description":"ok + user"}, "401": {"description":"자격 증명 오류"}, "503": {"description":"인증 비활성(단독 모드)"} }
      }
    },
    "/auth/logout": {
      "post": { "tags": ["auth"], "summary": "로그아웃 (세션 폐기)", "responses": { "200": {"description":"ok"} } }
    },
    "/auth/me": {
      "get": { "tags": ["auth"], "summary": "현재 인증 상태/사용자",
        "responses": { "200": {"description":"{auth_enabled, authenticated, user, sso_enabled}"} } }
    },
    "/auth/password": {
      "put": { "tags": ["auth"], "summary": "본인 비밀번호 변경 (로컬 계정)",
        "security": [{"SessionCookie":[]}],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type":"object", "required":["old_password","new_password"], "properties": { "old_password":{"type":"string"}, "new_password":{"type":"string"} } } } } },
        "responses": { "200": {"description":"ok"}, "401": {"description":"현재 비밀번호 불일치"} } }
    },
    "/auth/sso/login": {
      "get": { "tags": ["auth"], "summary": "Keycloak SSO 로그인 시작 (302 리다이렉트)",
        "responses": { "302": {"description":"authorize 엔드포인트로 리다이렉트"}, "503": {"description":"SSO 미설정"} } }
    },
    "/auth/sso/callback": {
      "get": { "tags": ["auth"], "summary": "OIDC 콜백 (code 교환 → 세션 발급 → /admin)",
        "parameters": [ {"name":"code","in":"query","schema":{"type":"string"}}, {"name":"state","in":"query","schema":{"type":"string"}} ],
        "responses": { "302": {"description":"로그인 후 /admin으로"}, "400": {"description":"state 불일치/코드 누락"} } }
    },
    "/api/users": {
      "get": { "tags": ["users"], "summary": "사용자 목록 (admin)", "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "responses": { "200": {"description":"{users[]}"}, "403": {"description":"admin 아님"} } },
      "post": { "tags": ["users"], "summary": "로컬 사용자 생성 (admin)", "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type":"object", "required":["username","password"], "properties": { "username":{"type":"string"}, "password":{"type":"string","minLength":8}, "role":{"type":"string","enum":["admin","user"]}, "display_name":{"type":"string"}, "email":{"type":"string"} } } } } },
        "responses": { "200": {"description":"ok + user"}, "400": {"description":"검증 실패/중복"}, "403": {"description":"admin 아님"} } }
    },
    "/api/users/{id}": {
      "put": { "tags": ["users"], "summary": "사용자 수정: 역할/활성화/PW재설정 (admin)", "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "description": "role/is_active/display_name/email/password. 마지막 admin은 강등·비활성화 불가. 비활성화 시 모든 세션 즉시 폐기.",
        "parameters": [ {"name":"id","in":"path","required":true,"schema":{"type":"string"}} ],
        "requestBody": { "content": { "application/json": { "schema": {"type":"object"} } } },
        "responses": { "200": {"description":"ok"}, "400": {"description":"마지막 admin 등 제약"}, "403": {"description":"admin 아님"} } }
    },
    "/api/mcp-keys": {
      "get": { "tags": ["mcp-keys"], "summary": "MCP 키 목록 (본인; ?all=true는 admin 전용)",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [ {"name":"all","in":"query","schema":{"type":"boolean"}}, {"name":"user_id","in":"query","schema":{"type":"string"}} ],
        "responses": { "200": {"description":"{keys[]} (status: active|expired|revoked)"} } },
      "post": { "tags": ["mcp-keys"], "summary": "MCP 키 발급 (원본은 1회만 반환)",
        "security": [{"SessionCookie":[]},{"MCPKey":[]}],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type":"object", "properties": { "name":{"type":"string"}, "ttl_hours":{"type":"integer","description":"0=무기한"}, "user_id":{"type":"string","description":"admin이 타 사용자 키 발급 시"} } } } } },
        "responses": { "200": {"description":"{key(원본, 1회), key_info}"} } }
    },
    "/api/mcp-keys/{id}/rotate": {
      "post": { "tags": ["mcp-keys"], "summary": "키 회전 (구 키 즉시 폐기 + 신 키 발급)",
        "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "parameters": [ {"name":"id","in":"path","required":true,"schema":{"type":"string"}} ],
        "responses": { "200": {"description":"{key(원본, 1회), key_info}"}, "403": {"description":"타인 키"} } }
    },
    "/api/mcp-keys/{id}": {
      "delete": { "tags": ["mcp-keys"], "summary": "키 폐기",
        "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "parameters": [ {"name":"id","in":"path","required":true,"schema":{"type":"string"}} ],
        "responses": { "200": {"description":"revoked"}, "403": {"description":"타인 키"} } }
    },
    "/api/db-profiles/{id}/grants": {
      "get": { "tags": ["grants"], "summary": "프로파일 권한 목록 (manage 권한 필요)",
        "security": [{"SessionCookie":[]}],
        "parameters": [ {"name":"id","in":"path","required":true,"schema":{"type":"string"}} ],
        "responses": { "200": {"description":"{owner_id, visibility, grants[]}"}, "403": {"description":"manage 권한 없음"} } },
      "put": { "tags": ["grants"], "summary": "권한 부여/변경 (use|manage)",
        "security": [{"SessionCookie":[]}],
        "parameters": [ {"name":"id","in":"path","required":true,"schema":{"type":"string"}} ],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type":"object", "required":["permission"], "properties": { "username":{"type":"string"}, "user_id":{"type":"string"}, "permission":{"type":"string","enum":["use","manage"]} } } } } },
        "responses": { "200": {"description":"ok"}, "403": {"description":"manage 권한 없음"} } }
    },
    "/api/db-profiles/{id}/grants/{userID}": {
      "delete": { "tags": ["grants"], "summary": "권한 회수",
        "security": [{"SessionCookie":[]}],
        "parameters": [ {"name":"id","in":"path","required":true,"schema":{"type":"string"}}, {"name":"userID","in":"path","required":true,"schema":{"type":"string"}} ],
        "responses": { "200": {"description":"ok"}, "403": {"description":"manage 권한 없음"} } }
    },
    "/api/mcp-activity": {
      "get": { "tags": ["activity"], "summary": "MCP 활동 이력 (본인 기본; admin은 all=true/user=<id>)",
        "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "parameters": [
          {"name":"limit","in":"query","schema":{"type":"integer"}},
          {"name":"all","in":"query","schema":{"type":"boolean"},"description":"admin 전용 — 전체 사용자"},
          {"name":"user","in":"query","schema":{"type":"string"},"description":"admin 전용 — 특정 사용자 id"}
        ],
        "responses": { "200": {"description":"{activity[], is_admin, scope}"} } }
    },
    "/api/clarification-suggestions": {
      "get": { "tags": ["activity"], "summary": "반복 재질문 집계 → 사전 승격 후보 (admin)",
        "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "responses": { "200": {"description":"{suggestions[]: clarification_id, occurrences, top_answer, suggestion}"}, "403": {"description":"admin 아님"} } }
    },
    "/api/mcp-stats": {
      "get": { "tags": ["activity"], "summary": "MCP 호출 통계 (본인 기본; admin은 all=true로 사용자별 분포)",
        "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "parameters": [
          {"name":"all","in":"query","schema":{"type":"boolean"}},
          {"name":"user","in":"query","schema":{"type":"string"}}
        ],
        "responses": { "200": {"description":"{total, prompts, generated, executions, total_rows, by_tool, by_exec_status, timeline[], per_user, users[]}"} } }
    },
    "/api/db-profiles/{id}/visibility": {
      "put": { "tags": ["grants"], "summary": "전체 공개 토글 (shared|private) — 소유자·admin만, 정의는 유지",
        "security": [{"SessionCookie":[]}],
        "parameters": [ {"name":"id","in":"path","required":true,"schema":{"type":"string"}} ],
        "requestBody": {"required":true,"content":{"application/json":{"schema":{"type":"object","properties":{"visibility":{"type":"string","enum":["shared","private"]}},"required":["visibility"]}}}},
        "responses": { "200": {"description":"{ok, id, visibility}"}, "403": {"description":"소유자·admin 아님"} } }
    },
    "/api/settings": {
      "get": {
        "tags": ["settings"], "summary": "런타임 설정 조회 (admin) — 시크릿은 마스킹",
        "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "responses": { "200": {"description":"{settings[], sso_enabled, boot_only}"}, "403": {"description":"admin 아님"} } },
      "put": {
        "tags": ["settings"], "summary": "설정 저장·즉시 적용 (admin)",
        "description": "본문은 key→value 맵. 값=설정, \"\"=빈값, null=삭제(플래그/env 기본값으로 복귀). 마스터 토큰·허용 Origin·OIDC(SSO)는 재기동 없이 적용됩니다.",
        "security": [{"SessionCookie":[]},{"AdminToken":[]}],
        "requestBody": { "required": true, "content": { "application/json": { "schema": {"type":"object","additionalProperties":{"type":["string","null"]}},
          "examples": { "oidc": { "value": { "oidc_issuer":"https://kc/realms/x","oidc_client_id":"sqlon","oidc_client_secret":"...","oidc_redirect_url":"https://host:9797/auth/sso/callback" } } } } } },
        "responses": { "200": {"description":"{ok, settings[], note}"}, "403": {"description":"admin 아님"} } }
    },
    "/api/health": {
      "get": {
        "tags": ["catalog"], "summary": "카탈로그 헬스",
        "description": "컴파일 상태, 오류/경고 목록, 커버리지 갭, PII 컬럼, 사전 크기.",
        "responses": { "200": { "description": "health report" } }
      }
    },
    "/api/fleet/instances": {
      "get": {
        "tags": ["fleet"], "summary": "DB 플릿 인벤토리",
        "description": "대상 DB에 연결하지 않고 현재 사용자가 접근 가능한 인스턴스의 환경, 업무서비스, 중요도, 엔진, 역할, 담당팀과 Capability를 반환합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "responses": { "200": {"description":"{status,data,summary,warnings,limitations,collected_at,trace_id}"}, "401": {"description":"인증 필요"} }
      }
    },
    "/api/fleet/health": {
      "get": {
        "tags": ["fleet"], "summary": "DB 플릿 연결·구성 위험 상태",
        "description": "접근 가능한 프로파일을 독립적으로 병렬 점검합니다. 부분 실패도 HTTP 200의 degraded 응답으로 반환하며 각 인스턴스에 수집 시각, 위험 점수, 근거와 구조화된 실패 원인을 포함합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "responses": { "200": {"description":"fleet health envelope"}, "401": {"description":"인증 필요"} }
      }
    },
    "/api/observability/sessions": {
      "get": {
        "tags": ["fleet"], "summary": "DB 세션 스냅숏",
        "description": "고정된 엔진 시스템 뷰를 읽기 전용으로 조회합니다. SQL 실행시간과 트랜잭션 지속시간, 대기 이벤트와 보호 세션을 구분하며 SQL 본문·bind 값은 반환하지 않습니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"evidence-bearing session snapshot"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/observability/locks": {
      "get": {
        "tags": ["fleet"], "summary": "DB 블로킹 트리",
        "description": "blocker→blocked 관계, 루트 블로커와 전체 영향 세션 수를 반환하는 관찰 전용 API입니다. 세션을 취소하거나 종료하지 않습니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"evidence-bearing lock tree"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/observability/replication": {
      "get": {
        "tags": ["fleet"], "summary": "DB 복제 상태와 토폴로지",
        "description": "복제 역할(primary/replica/standby/standalone)과 구성 요소별 상태·지연을 반환합니다. PostgreSQL은 standby·slot·WAL receiver, MySQL/MariaDB는 채널별 IO/SQL 스레드, Oracle은 Data Guard lag과 archive destination(base 라이선스 뷰만)입니다. 측정 불가한 지연은 lag_seconds=-1로 구분합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"evidence-bearing replication status"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/observability/backup": {
      "get": {
        "tags": ["fleet"], "summary": "DB 백업·아카이브 상태",
        "description": "DB 서버가 스스로 보고할 수 있는 백업 상태를 반환합니다: PostgreSQL WAL 아카이버, MySQL/MariaDB binlog(PITR 기반), Oracle ARCHIVELOG·RMAN 이력·FRA 사용률(base 뷰만). 외부 백업 도구의 잡 상태는 limitation으로 명시합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"evidence-bearing backup status"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/observability/security": {
      "get": {
        "tags": ["fleet"], "summary": "DB 사용자·권한 진단",
        "description": "권한 과다 항목을 근거·심각도와 함께 반환합니다: 로그인 가능한 비기본 SUPERUSER(PostgreSQL), 위험 권한·와일드카드 호스트(MySQL/MariaDB USER_PRIVILEGES), DBA 역할·위험 시스템 권한(Oracle DBA_* 뷰). 읽기 전용 진단입니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"evidence-bearing security posture"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/observability/workload": {
      "get": {
        "tags": ["fleet"], "summary": "저장된 DB 워크로드 요약",
        "description": "누적 시스템 카운터, 이전 스냅숏 기반 QPS/TPS와 대기 이벤트를 반환합니다. fresh=true일 때만 대상 DB를 새로 조회하고 저장합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}},{"name":"fresh","in":"query","schema":{"type":"boolean","default":false}}],
        "responses": {"200":{"description":"workload evidence envelope"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/observability/top-sql": {
      "get": {
        "tags": ["fleet"], "summary": "원문 없는 Top SQL 통계",
        "description": "fingerprint/SQL ID별 calls, elapsed, CPU, reads, rows와 plan hash를 반환합니다. SQL 원문과 bind 값은 저장하지 않습니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}},{"name":"fresh","in":"query","schema":{"type":"boolean","default":false}}],
        "responses": {"200":{"description":"top SQL evidence envelope"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/observability/capacity": {
      "get": {
        "tags": ["fleet"], "summary": "DB·객체·tablespace 용량",
        "description": "사용량, 사용률과 이전 스냅숏 대비 일간 증가량을 반환합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}},{"name":"fresh","in":"query","schema":{"type":"boolean","default":false}}],
        "responses": {"200":{"description":"capacity evidence envelope"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/observability/history": {
      "get": {
        "tags": ["fleet"], "summary": "운영 스냅숏 시계열",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"profile","in":"query","required":true,"schema":{"type":"string"}},{"name":"hours","in":"query","schema":{"type":"integer","default":24,"maximum":2160}},{"name":"limit","in":"query","schema":{"type":"integer","default":1000,"maximum":10000}}],
        "responses": {"200":{"description":"stored operational snapshots"},"401":{"description":"인증 필요"},"404":{"description":"프로파일 없음 또는 접근 불가"}}
      }
    },
    "/api/collector/run": {
      "post": {
        "tags": ["fleet"], "summary": "권한 범위 DB 즉시 수집",
        "description": "각 프로파일을 격리해 고정 읽기 전용 Provider 쿼리를 실행하고 스냅숏을 저장합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "responses": {"200":{"description":"partial-failure-safe batch result"},"401":{"description":"인증 필요"}}
      }
    },
    "/api/changes": {
      "get": {
        "tags": ["changes"], "summary": "변경계획 목록 (최신순)",
        "description": "저장된 모든 변경계획을 상태·위험도·승인 이력과 함께 반환합니다. 계획은 재시작 후에도 유지됩니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "responses": {"200":{"description":"{status,data:[Plan],collected_at}"},"401":{"description":"인증 필요"},"403":{"description":"DBA 권한 필요"}}
      },
      "post": {
        "tags": ["changes"], "summary": "변경계획 생성 (draft)",
        "description": "대상·사유·위험도와 단계(command/verification/compensation)를 검증해 초안을 생성합니다. 필요 승인 수는 서버 정책이 결정하며 이 호출은 DB를 변경하지 않습니다. Idempotency-Key 헤더를 지원합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "responses": {"201":{"description":"created plan"},"400":{"description":"검증 실패"},"401":{"description":"인증 필요"},"403":{"description":"DBA 권한 필요"}}
      }
    },
    "/api/changes/template": {
      "post": {
        "tags": ["changes"], "summary": "구조화된 액션 → 변경계획 단계 생성",
        "description": "create_user/create_database/grant/revoke 같은 되돌릴 수 있는 권한 작업을 방언별 안전 인용된 command·verification·compensation 단계로 생성합니다. DB를 변경하지 않으며, 되돌릴 수 없는 작업과 비밀번호는 거부합니다. 생성된 단계를 검토 후 POST /api/changes의 steps에 포함하세요.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "responses": {"200":{"description":"generated change step"},"400":{"description":"지원하지 않는 액션·잘못된 인자·비밀번호 포함"},"401":{"description":"인증 필요"},"403":{"description":"DBA 권한 필요"}}
      }
    },
    "/api/changes/{id}": {
      "get": {
        "tags": ["changes"], "summary": "변경계획 상세",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"plan"},"404":{"description":"없음"},"401":{"description":"인증 필요"},"403":{"description":"DBA 권한 필요"}}
      }
    },
    "/api/changes/{id}/submit": {
      "post": {
        "tags": ["changes"], "summary": "변경계획 제출 (검토/승인 대기)",
        "description": "초안을 동결합니다. low 위험만 승인 없이 실행 가능 상태가 되며 그 외에는 review_required로 전이합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"plan"},"400":{"description":"상태 오류"},"401":{"description":"인증 필요"},"403":{"description":"DBA 권한 필요"}}
      }
    },
    "/api/changes/{id}/approve": {
      "post": {
        "tags": ["changes"], "summary": "변경계획 승인",
        "description": "현재 사용자의 자격으로 승인을 기록합니다. critical은 서로 다른 2인의 승인이 필요하며 동일 승인자의 중복 승인은 거부됩니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"plan(+approval id)"},"400":{"description":"상태/중복 오류"},"401":{"description":"인증 필요"},"403":{"description":"DBA 권한 필요"}}
      }
    },
    "/api/changes/{id}/execute": {
      "post": {
        "tags": ["changes"], "summary": "승인된 변경 실행",
        "description": "실행 직전 재검증 후 승인된 불변 단계만 실행하고 각 단계를 검증합니다. 승인 필요 위험도에는 X-Approval-ID 헤더가 필수입니다. 실패 시 rollback_required로 전이합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"id","in":"path","required":true,"schema":{"type":"string"}},{"name":"X-Approval-ID","in":"header","schema":{"type":"string"}}],
        "responses": {"200":{"description":"completed plan"},"400":{"description":"실행/검증 실패"},"401":{"description":"인증 필요"},"403":{"description":"승인 ID 누락/무효"},"404":{"description":"없음"}}
      }
    },
    "/api/changes/{id}/rollback": {
      "post": {
        "tags": ["changes"], "summary": "보상 작업 실행 (롤백)",
        "description": "rollback_required 상태의 변경에 대해 승인된 계획의 보상 작업을 역순 실행합니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"rolled_back plan"},"400":{"description":"상태 오류/보상 실패"},"401":{"description":"인증 필요"},"403":{"description":"DBA 권한 필요"}}
      }
    },
    "/api/changes/{id}/cancel": {
      "post": {
        "tags": ["changes"], "summary": "변경계획 취소",
        "description": "실행 전 단계(초안·검토·승인·예약)의 변경을 취소합니다. 취소된 계획도 이력으로 보존됩니다.",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
        "responses": {"200":{"description":"cancelled plan"},"400":{"description":"상태 오류"},"401":{"description":"인증 필요"},"403":{"description":"DBA 권한 필요"}}
      }
    },
    "/api/datasets": {
      "get": {
        "tags": ["datasets"], "summary": "데이터셋 목록 + 라이브 상태",
        "description": "레지스트리의 18개 데이터셋 전부: 용도, 스키마, 사용 도구, 존재/크기/로드 건수/이슈.",
        "responses": { "200": { "description": "dataset list", "content": { "application/json": { "schema": { "type": "object", "properties": { "data_dir": { "type": "string" }, "datasets": { "type": "array", "items": { "$ref": "#/components/schemas/DatasetStatus" } } } } } } } }
      }
    },
    "/api/datasets/{name}": {
      "get": {
        "tags": ["datasets"], "summary": "데이터셋 상세 + 내용 샘플",
        "parameters": [
          { "name": "name", "in": "path", "required": true, "schema": { "type": "string" }, "example": "glossary" },
          { "name": "sample_rows", "in": "query", "schema": { "type": "integer", "default": 5, "maximum": 50 } }
        ],
        "responses": { "200": { "description": "detail with sample" }, "404": { "description": "unknown dataset", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } } }
      },
      "put": {
        "tags": ["datasets"], "summary": "데이터셋 교체 (백업→검증→핫스왑, 실패 시 자동 롤백)",
        "description": "요청 본문이 파일의 새 전체 내용이 됩니다(부분 병합 아님). 형식(json-array/json-object) 검증 후 기존 파일을 backups/에 백업하고 카탈로그를 재컴파일합니다. 컴파일 실패 또는 신규 로드 오류 시 원본 복원 + 이전 카탈로그 유지. force=true면 오류가 있어도 적용.",
        "security": [{ "AdminToken": [] }],
        "parameters": [
          { "name": "name", "in": "path", "required": true, "schema": { "type": "string" } },
          { "name": "force", "in": "query", "schema": { "type": "boolean", "default": false } }
        ],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "description": "데이터셋 스키마에 맞는 전체 JSON (array 또는 object)" }, "examples": { "glossary": { "summary": "glossary 예시", "value": { "entries": [{ "term": "고객", "synonyms": ["회원", "cust_no"], "category": "entity" }] } } } } } },
        "responses": {
          "200": { "description": "applied=true(적용) 또는 applied=false(롤백)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/MutationResult" } } } },
          "400": { "description": "잘못된 이름/형식/시스템 관리 대상", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } },
          "401": { "description": "admin token 필요" }
        }
      },
      "delete": {
        "tags": ["datasets"], "summary": "데이터셋 제거 (백업 후 삭제 + 핫스왑)",
        "description": "선택(optional) + 편집 가능 데이터셋만 제거할 수 있습니다. required=true(물리/논리 모델)와 editable=false(feedback/audit)는 거부됩니다.",
        "security": [{ "AdminToken": [] }],
        "parameters": [{ "name": "name", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": {
          "200": { "description": "removed", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/MutationResult" } } } },
          "400": { "description": "required/system-managed/미존재", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } },
          "401": { "description": "admin token 필요" }
        }
      }
    },
    "/api/datasets/{name}/content": {
      "get": {
        "tags": ["datasets"], "summary": "데이터셋 원본 전체 내용",
        "description": "편집기용 원본 파일 그대로 반환. 파일이 없으면 204.",
        "parameters": [{ "name": "name", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "raw JSON content" }, "204": { "description": "file not present" }, "400": { "description": "jsonl-dir 등 단일 문서가 아님" } }
      }
    },
    "/api/datasets/{name}/backups": {
      "get": {
        "tags": ["datasets"], "summary": "백업 목록 (최신순)",
        "parameters": [{ "name": "name", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "backups" } }
      }
    },
    "/api/datasets/{name}/restore": {
      "post": {
        "tags": ["datasets"], "summary": "백업으로 복원 (복원 전 현재 파일도 백업)",
        "security": [{ "AdminToken": [] }],
        "parameters": [{ "name": "name", "in": "path", "required": true, "schema": { "type": "string" } }],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object", "required": ["backup"], "properties": { "backup": { "type": "string", "example": "metrics.json.20260703T101500" } } } } } },
        "responses": { "200": { "description": "restored + hot-swapped" }, "400": { "description": "invalid backup name" }, "401": { "description": "admin token 필요" } }
      }
    },
    "/api/reload": {
      "post": {
        "tags": ["catalog"], "summary": "디스크에서 카탈로그 재컴파일 + 핫스왑",
        "description": "볼륨 마운트 등으로 파일을 직접 수정한 뒤 호출하세요. 컴파일 실패 시 이전 카탈로그가 유지됩니다.",
        "security": [{ "AdminToken": [] }],
        "responses": { "200": { "description": "reloaded" }, "401": { "description": "admin token 필요" }, "500": { "description": "reload failed; previous catalog stays active" } }
      }
    },
    "/api/db-profiles": {
      "get": {
        "tags": ["db-profiles"], "summary": "프로파일 목록 (비밀번호 마스킹)",
        "description": "프로파일 요약 목록. postgres/mysql/mariadb 드라이버가 기본 내장되어 실행·테스트가 바로 가능합니다.",
        "responses": { "200": { "description": "profiles + driver 상태" } }
      },
      "post": {
        "tags": ["db-profiles"], "summary": "프로파일 생성",
        "description": "connect_string은 Easy Connect(host:port/service?connect_timeout=5&expire_time=2), VIP 주소, (DESCRIPTION=...) TNS, tcps://를 지원합니다. password_ref는 env:NAME | file:PATH | plain:VALUE (plain은 비권장). 저장 전 자동 백업.",
        "security": [{ "AdminToken": [] }],
        "requestBody": { "required": true, "content": { "application/json": { "examples": { "pg": { "summary": "PostgreSQL + 풀/정책", "value": { "id": "pg-prod-01", "name": "운영 PostgreSQL", "type": "postgres", "connect_string": "db.example.com:5432/appdb", "username": "app_readonly", "password_ref": "env:PG_PROD_PW", "pool": { "max_open_conns": 10, "max_idle_conns": 2, "conn_max_lifetime_seconds": 1800, "conn_max_idle_time_seconds": 600 }, "policy": { "query_timeout_seconds": 30, "connection_test_timeout_seconds": 5, "default_max_rows": 100, "max_rows": 1000 } } } } } } },
        "responses": { "200": { "description": "saved" }, "400": { "description": "검증 실패(id 형식, password_ref 스킴, readonly=false 등)" }, "401": { "description": "admin token 필요" } }
      }
    },
    "/api/db-profiles/{id}": {
      "put": {
        "tags": ["db-profiles"], "summary": "프로파일 수정 (풀은 자동 재생성)",
        "security": [{ "AdminToken": [] }],
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "requestBody": { "required": true, "content": { "application/json": {} } },
        "responses": { "200": { "description": "saved" }, "400": { "description": "not found / invalid" }, "401": { "description": "admin token 필요" } }
      },
      "delete": {
        "tags": ["db-profiles"], "summary": "프로파일 삭제 (백업 후)",
        "security": [{ "AdminToken": [] }],
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "removed" }, "400": { "description": "not found" }, "401": { "description": "admin token 필요" } }
      }
    },
    "/api/db-profiles/{id}/test": {
      "post": {
        "tags": ["db-profiles"], "summary": "접속 테스트 (PingContext + connection_test_timeout)",
        "description": "성공 시 ok=true와 소요 ms. 실패 시 ORA 코드/정제 메시지. 연속 3회 실패 시 서킷브레이커가 30초간 열립니다(CIRCUIT_OPEN).",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "PingResult {ok, elapsed_ms, error, error_code}" } }
      }
    },
    "/api/query/validate": {
      "post": {
        "tags": ["query"], "summary": "SQL 안전성 검증 (카탈로그 33종 룰 + 커넥터 가드)",
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object", "required": ["sql"], "properties": { "sql": { "type": "string" }, "profile_id": { "type": "string", "description": "프로파일 추가 차단 키워드 반영" }, "metrics": { "type": "array", "items": { "type": "string" } }, "expected_outputs": { "type": "array", "items": { "type": "string" } } } } } } },
        "responses": { "200": { "description": "{catalog: ValidationResult, connector: {valid, error}}" } }
      }
    },
    "/api/query/execute": {
      "post": {
        "tags": ["query"], "summary": "Read-Only 실행 (검증 통과 SQL만, maxRows+1로 truncated 감지)",
        "description": "카탈로그 검증 실패 시 executed=false + validation 반환(실행 안 함). 성공 시 컬럼 메타, 행(JSON), row_count, elapsed_ms, truncated. 바인드 변수는 binds 배열(:1, :2 ...). 모든 실행은 audit/query-*.jsonl에 기록.",
        "security": [{ "AdminToken": [] }],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object", "required": ["profile_id", "sql"], "properties": { "profile_id": { "type": "string" }, "sql": { "type": "string" }, "max_rows": { "type": "integer" }, "timeout_seconds": { "type": "integer" }, "binds": { "type": "array" }, "user": { "type": "string" }, "trace_id": { "type": "string" } } } } } },
        "responses": { "200": { "description": "{executed, result | error, validation}" }, "401": { "description": "admin token 필요" } }
      }
    },
    "/api/query/preview": {
      "post": {
        "tags": ["query"], "summary": "미리보기 (프로파일 default_max_rows 강제)",
        "security": [{ "AdminToken": [] }],
        "responses": { "200": { "description": "execute와 동일 형식" }, "401": { "description": "admin token 필요" } }
      }
    },
    "/api/query/explain": {
      "post": {
        "tags": ["query"], "summary": "실행계획 분석 (정적 + 실측 EXPLAIN PLAN)",
        "description": "static은 항상 반환. profile_id가 있고 드라이버가 있으면 실제 EXPLAIN PLAN을 수행해 PLAN_TABLE을 분석합니다: full table scan, MERGE JOIN CARTESIAN, nested loops 비효율, 대형 sort/hash aggregate, 예상 row 과다(100만+), 고비용(cost 10만+). risk=high면 실행하지 말고 suggestions를 반영해 재생성하세요. DBMS_ 패키지는 사용하지 않습니다(PLAN_TABLE 직접 조회).",
        "security": [{ "AdminToken": [] }],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object", "required": ["sql"], "properties": { "profile_id": { "type": "string" }, "sql": { "type": "string" } } } } } },
        "responses": { "200": { "description": "{static, live_plan{steps, total_cost, max_cardinality, risk, risk_factors, suggestions} | live_plan_error}" }, "401": { "description": "admin token 필요" } }
      }
    },
    "/api/query/metadata": {
      "post": {
        "tags": ["query"], "summary": "컬럼 메타데이터만 조회 (ROWNUM <= 0 실행)",
        "security": [{ "AdminToken": [] }],
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object", "required": ["profile_id", "sql"], "properties": { "profile_id": { "type": "string" }, "sql": { "type": "string" } } } } } },
        "responses": { "200": { "description": "{ok, columns[{name,type}] | error}" }, "401": { "description": "admin token 필요" } }
      }
    },
    "/api/query/history": {
      "get": {
        "tags": ["query"], "summary": "실행 이력(최신순, 메모리 200건) + 실행 중 목록",
        "parameters": [{ "name": "limit", "in": "query", "schema": { "type": "integer", "default": 50 } }],
        "responses": { "200": { "description": "{history[], running[]}" } }
      }
    },
    "/api/query/submit": {
      "post": { "tags": ["query"], "summary": "비동기 쿼리 제출 — 즉시 job_id 반환, 백그라운드 실행 (사용자당 5개, 결과 10분 보관)",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "requestBody": {"required":true,"content":{"application/json":{"schema":{"type":"object","required":["profile_id","sql"],"properties":{"profile_id":{"type":"string"},"sql":{"type":"string"},"max_rows":{"type":"integer"},"timeout_seconds":{"type":"integer"}}}}}},
        "responses": { "202": {"description":"{submitted, job_id, poll}"}, "200": {"description":"검증 실패 시 {submitted:false, validation}"}, "429": {"description":"동시 실행 한도"} } }
    },
    "/api/query/job/{jobId}": {
      "get": { "tags": ["query"], "summary": "비동기 잡 상태/결과 조회 (본인 또는 admin)",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"jobId","in":"path","required":true,"schema":{"type":"string"}}],
        "responses": { "200": {"description":"{status: running|done|failed, result?, result_diagnosis?, masked_columns?}"}, "404": {"description":"만료/없음"} } }
    },
    "/api/query/job/{jobId}/cancel": {
      "post": { "tags": ["query"], "summary": "비동기 잡 취소 (본인 또는 admin)",
        "security": [{"SessionCookie":[]},{"MCPKey":[]},{"AdminToken":[]}],
        "parameters": [{"name":"jobId","in":"path","required":true,"schema":{"type":"string"}}],
        "responses": { "200": {"description":"canceled"}, "404": {"description":"없음/권한 없음"} } }
    },
    "/api/query/cancel/{executionId}": {
      "post": {
        "tags": ["query"], "summary": "실행 중인 쿼리 취소 (context cancel)",
        "security": [{ "AdminToken": [] }],
        "parameters": [{ "name": "executionId", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "canceled" }, "404": { "description": "이미 종료됨" }, "401": { "description": "admin token 필요" } }
      }
    },
    "/api/metrics": {
      "get": {
        "tags": ["query"], "summary": "커넥터 메트릭 (카운터 + 프로파일별 커넥션 풀 db.Stats + 브레이커 상태)",
        "responses": { "200": { "description": "snapshot JSON; Prometheus 텍스트는 GET /metrics" } }
      }
    }
  }
}`
