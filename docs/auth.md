# 인증·권한·MCP 키 관리

jamypg는 두 가지 실행 모드로 동작합니다.

| 모드 | 활성 조건 | 동작 |
| --- | --- | --- |
| **단독(standalone)** | `-meta-db` 미설정 | 기존과 동일 — 로그인 없음, `/mcp` 개방, 선택적 `-admin-token`으로 관리 API 보호, 프로파일은 `db_profiles.json` |
| **인증(auth)** | `-meta-db` 설정 | Postgres 메타 DB 기반 로그인·사용자·MCP 키·사용자별 프로파일·권한 |

메타 DB를 붙이면 나머지 기능이 자동으로 켜집니다. 기존 배포(단독 모드)는
플래그를 추가하지 않는 한 아무 영향도 받지 않습니다.

## 활성화

```sh
jamypg-mcp -transport http -addr 0.0.0.0:9797 \
  -meta-db 'postgres://jamypg:pw@pg-host:5432/jamypg?sslmode=require' \
  -bootstrap-admin 'admin:첫관리자비밀번호'        # 생략 시 랜덤 생성 후 로그 1회 출력
```

환경변수도 지원: `JAMYPG_META_DB`, `JAMYPG_BOOTSTRAP_ADMIN`,
`JAMYPG_ADMIN_TOKEN`, `JAMYPG_OIDC_*`.

- 메타 DB는 최초 접속 시 스키마를 **자동 마이그레이션**합니다(`jamypg_users`,
  `jamypg_sessions`, `jamypg_mcp_keys`, `jamypg_db_profiles`,
  `jamypg_profile_grants`). 재기동해도 idempotent합니다.
- 사용자가 0명이면 **부트스트랩 관리자**를 생성합니다. `-bootstrap-admin`이
  없으면 랜덤 비밀번호를 만들어 기동 로그에 한 번만 출력합니다.
- 드라이버는 순수 Go(pgx)라 CGO/외부 클라이언트가 필요 없습니다 — 대상 DB
  커넥터와 마찬가지로 기본 이미지에서 그대로 동작합니다.

## 로그인 방법

| 방법 | 용도 |
| --- | --- |
| 로컬 계정(아이디/비밀번호) | 웹 콘솔 로그인. bcrypt 해시 저장, 세션 쿠키(24h, 자동 연장) |
| Keycloak SSO(OIDC) | 조직 IdP 통합. authorization code + userinfo 검증 |
| MCP 키(`jsk_...`) | MCP 클라이언트/스크립트의 `/mcp`·`/api` 접근 |
| 마스터 토큰(`-admin-token`) | 비상용(break-glass) — 합성 admin으로 동작 |

### Keycloak SSO 설정

Keycloak에서 Confidential 클라이언트를 만들고 redirect URI에
`https://<host>:9797/auth/sso/callback`를 등록한 뒤:

```sh
-oidc-issuer https://kc.example.com/realms/myrealm \
-oidc-client-id jamypg \
-oidc-client-secret <secret> \
-oidc-redirect-url https://jamypg.example.com/auth/sso/callback
```

- 4개 값이 모두 있어야 SSO가 켜지고 로그인 페이지에 "Keycloak으로 로그인"
  버튼이 나타납니다.
- 표준 OIDC 흐름: authorize → code 교환 → **userinfo로 액세스 토큰 검증**
  (로컬 JWT/JWKS 처리 없음, state 파라미터로 CSRF 방지).
- 최초 SSO 사용자(=첫 사용자)는 admin으로, 이후는 user로 생성됩니다.
  역할 승격은 관리자가 `/admin/users`에서 수행합니다.

## 권한 모델

역할은 **admin**과 **user** 두 가지입니다.

| 대상 | admin | user |
| --- | --- | --- |
| 사용자 관리(생성/역할/활성화/PW재설정) | ✅ | ✗ |
| 데이터셋 편집·리로드 | ✅ | ✗ |
| 모든 DB 프로파일 조회/사용/관리/삭제 | ✅ | 소유·grant·shared 범위만 |
| 모든 MCP 키 조회/발급/폐기 | ✅ | 본인 키만 |
| 본인 프로파일 CRUD, 본인 키 관리 | ✅ | ✅ |

### DB 프로파일 권한

프로파일은 소유자(생성자)가 있고, 공개 범위와 개별 권한으로 접근을 제어합니다.

- **visibility**: `private`(소유자·grant·admin만) / `shared`(모든 로그인
  사용자가 **use** 가능)
- **grant**: 소유자·admin·manage 권한자가 다른 사용자에게 부여
  - `use` — 조회/실행/접속 테스트/실행계획
  - `manage` — 위 + 프로파일 정의 수정 + 권한 부여(단, 삭제·visibility 변경은
    소유자·admin만)
- 화면: `/admin/db`의 프로파일별 **[권한·공유]** 버튼

### 관리자가 DB를 사용자에게 공유하는 방법

`/admin/db`에서 프로파일의 **[권한·공유]**를 열면 두 가지 방식으로 접근을 나눠줄
수 있습니다(관리자는 소유하지 않은 프로파일에도 열 수 있습니다).

1. **특정 사용자에게만** — 사용자 아이디를 입력/선택하고 `use`(조회·실행) 또는
   `manage`(수정·재부여)를 부여. `PUT /api/db-profiles/{id}/grants`
   (`{username, permission}`). 관리자는 사용자 목록에서 바로 고를 수 있습니다.
2. **전체 사용자에게** — **[전체 사용자에게 공개 (shared)]** 토글을 켜면 모든 로그인
   사용자가 `use` 할 수 있습니다. `PUT /api/db-profiles/{id}/visibility`
   (`{visibility:"shared"|"private"}`) — 접속 정의는 그대로 유지되고 공개 범위만
   바뀝니다. 공개 전환은 **소유자·관리자만** 가능합니다.

개인 사용자는 자신이 만든 프로파일을 소유하며 위와 동일하게 다른 사람에게 공유할
수 있고, 관리자가 등록한 프로파일은 관리자 소유로 유지됩니다.

권한 검사는 REST(`/api/query/*`, `/api/db-profiles/*`)와 MCP 도구
(`run_sql_safely`, `explain_sql`, `run_evaluation`) 양쪽에서 동일하게
적용됩니다. 권한 없는 프로파일 실행은 `forbidden`으로 차단되고 감사됩니다.

## MCP 키 라이프사이클

MCP 클라이언트(Claude, qwen-code 등)가 HTTP `/mcp`에 접속할 때 사용합니다.
화면: `/admin/keys`.

```
발급 → (사용: last_used 기록) → 회전 또는 폐기
          └ 만료(TTL) → expired
```

- 발급 시 원본 키(`jsk_<64hex>`)는 **한 번만** 표시되고 서버에는 SHA-256
  해시만 저장됩니다.
- TTL: 무기한/24h/7d/30d/90d. 만료·폐기 키는 인증이 거부되며 목록에는
  감사 목적으로 남습니다.
- **회전**: 기존 키를 즉시 폐기하고 같은 이름의 새 키 발급(남은 유효기간
  승계). 유출 대응 시 사용.
- admin은 타 사용자 키를 발급·조회·폐기할 수 있습니다.

클라이언트 설정 예:

```json
{
  "mcpServers": {
    "jamypg": {
      "url": "https://jamypg.example.com/mcp",
      "headers": { "Authorization": "Bearer jsk_..." }
    }
  }
}
```

`X-MCP-Key: jsk_...` 헤더도 동일하게 동작합니다. stdio 전송은 로컬 신뢰
경계이므로 키가 필요 없습니다.

## 웹 콘솔 UX (좌측 사이드바 + 프로필 메뉴)

모든 관리 화면은 공통 앱셸을 사용합니다.

- **좌측 사이드바**: `작업`(질의·데이터셋·테이블 편집·DB/쿼리) / `관리`(사용자·서버
  설정·MCP 키, 권한에 따라 노출) / `문서`(API 문서) 그룹. 현재 페이지가 강조되고,
  좁은 화면에서는 햄버거로 접힙니다. 하단에 서버 버전(`v0.14.1`)이 표시됩니다.
- **우측 상단 프로필 메뉴**(로그인 시): 아바타 클릭 → 드롭다운.
  - **개인정보 변경** — 표시 이름·이메일 수정(`PUT /auth/profile`). 역할·비밀번호는
    바뀌지 않습니다.
  - **비밀번호 변경** — 현재 비밀번호 확인 후 변경(`PUT /auth/password`). SSO 계정은
    Keycloak에서 관리되므로 표시되지 않습니다.
  - **MCP 키 관리** → `/admin/keys`, **로그아웃**, 하단에 버전.
- **단독 모드**에서는 프로필 메뉴 대신 필요 시 관리 토큰 입력을 여는 🔧 토글만
  제공됩니다(기본 숨김).

### DB 프로파일 소유권 화면

`/admin/db`의 프로파일 목록은 소유권 기준으로 그룹화됩니다.

- **👤 내 프로파일** — 내가 소유(생성). 자유롭게 수정·삭제·권한부여.
- **🤝 공유 · 부여받음** — `shared`이거나 나에게 grant된 프로파일(use/manage).
- **🛡 전체(관리자 열람)** — admin에게만, 다른 사용자 소유 프로파일이 표시됩니다.

개인은 자신의 접속 정보를 직접 등록·관리하고, 관리자 소유 프로파일은 관리자 소유로
유지됩니다. 수정/삭제/권한 버튼은 권한(owner/manage/admin)에 따라 노출됩니다.

## MCP 활동 이력 · 호출 통계 (개인화)

MCP 키로 호출한 활동이 인증 모드에서 사용자별로 기록되어 두 화면으로 제공됩니다.

- **내 이력** `/admin/history` — 생성/실행한 쿼리, 그 쿼리를 만든 **사용자 프롬프트**
  (같은 세션의 최근 `prepare_sql_context`/`analyze_question` 질문을 자동 상관),
  프로파일·상태·행 수, 그리고 에이전트가 보낸 **파라미터(`_meta`, 예: temperature)**.
  API: `GET /api/mcp-activity?limit=&all=&user=`.
- **호출 통계** `/admin/stats` — 총 호출/프롬프트/생성(유효)/실행/반환행 KPI, 최근
  14일 추이, 도구별·실행상태 분포. API: `GET /api/mcp-stats`.

기록 대상은 의미 있는 이벤트만입니다: `prepare_sql_context`·`analyze_question`
(프롬프트), `validate_sql`·`explain_sql`(생성), `run_sql_safely`(실행). 단순
조회 도구는 남기지 않습니다.

**권한**: 일반 사용자는 **본인 활동만** 봅니다(`all`/`user` 파라미터를 줘도 본인으로
고정). **관리자**는 `?all=true`로 전체를, `?user=<id>`로 특정 사용자를 볼 수 있고
통계에 **사용자별 분포**가 추가됩니다. 기록은 best-effort라 실패해도 도구 응답에는
영향을 주지 않으며, 단독 모드에서는 비활성입니다.

## 세션·보안 특성

- 세션 토큰은 원본을 저장하지 않고 SHA-256 해시만 보관, 쿠키는 HttpOnly +
  SameSite=Lax + (HTTPS 시) Secure.
- 사용자 **비활성화** 시 모든 세션이 즉시 폐기되고 로그인·키 인증이 막힙니다.
- **마지막 관리자**는 강등/비활성화할 수 없습니다.
- 로그인 실패는 사용자 존재 여부와 무관하게 유사한 시간이 소요되도록
  처리합니다(타이밍 기반 사용자 열거 방지).
- 모든 인증 이벤트·권한 변경은 `audit/*.jsonl`에 기록됩니다.

## 감사 이벤트

`auth_login`, `auth_login_failed`, `auth_logout`, `auth_sso_login`,
`auth_password_changed`, `auth_profile_update`, `user_create`, `user_update`, `mcp_key_create`,
`mcp_key_rotate`, `mcp_key_revoke`, `profile_grant`, `profile_grant_remove`,
`db_profile_create/update/delete`.

## 운영 체크리스트

1. Postgres 준비(전용 DB/스키마, TLS 권장) → `-meta-db` DSN
2. `-bootstrap-admin`으로 첫 관리자 지정(또는 로그의 랜덤 비밀번호 확보)
3. HTTPS 리버스 프록시 뒤 배치(쿠키 Secure, 토큰 보호)
4. SSO 쓰면 `-oidc-*` 4종 + Keycloak 클라이언트 redirect URI 등록
5. 사용자·역할 정리(`/admin/users`), 프로파일 소유·grant 설계
6. MCP 클라이언트에는 로그인이 아닌 **MCP 키** 배포, TTL·회전 정책 수립

## 서버 설정을 메타 DB에서 관리 (`/admin/settings`)

런타임에 바꿀 수 있는 옵션은 플래그/env가 아니라 메타 DB(`jamypg_settings`)에
저장하고 **재기동 없이 즉시 적용**할 수 있습니다. 관리 화면은
`/admin/settings`(admin 전용), API는 `GET/PUT /api/settings`.

| 설정 | 키 | 적용 |
| --- | --- | --- |
| 마스터 관리 토큰 | `admin_token` | 즉시 |
| 허용 Origin | `allow_origins` | 즉시 |
| Keycloak SSO 4종 | `oidc_issuer`/`oidc_client_id`/`oidc_client_secret`/`oidc_redirect_url` | 즉시(제공자 재구성) |

- 우선순위: **저장된 설정(비어있지 않음) > 플래그/env > 기본값**. 값을
  삭제(null)하면 플래그/env 기본값으로 되돌아갑니다.
- 시크릿(`admin_token`, `oidc_client_secret`)은 조회 시 `••••••(set)`로
  마스킹되며, 입력란을 비워두면 기존 값이 유지됩니다.
- 부팅 전용 값(리슨 주소 `-addr`, 메타 DSN `-meta-db`, 전송 `-transport`)은
  성격상 재기동이 필요하므로 설정 화면에 읽기 전용으로 표시됩니다.

## 데이터셋 JSON을 메타 DB에서 관리

메타 DB가 활성이면 편집 가능한 카탈로그 JSON(물리/논리 모델, 용어사전,
지표사전, 조인관계, 패턴, 골든셋 등 14종)의 **진실 원본이 Postgres
`jamypg_datasets` 테이블**이 됩니다.

- **최초 기동**: 디스크의 파일 중 DB에 없는 것을 DB로 임포트(seed)합니다.
- **로드**: 매 컴파일 전에 DB → 파일로 materialize한 뒤 기존 로더로
  컴파일합니다(검증·핫스왑·롤백 로직 전부 재사용). 파일은 캐시일 뿐이고 DB가
  권위를 가집니다.
- **변경**: `/admin`·`/admin/editor`의 편집이나 `put_dataset`/`remove_dataset`
  MCP 도구가 검증 성공 시 DB에 커밋합니다. 검증 실패 시 파일 기반 롤백만
  일어나고 DB는 건드리지 않습니다.
- `reload_catalog`는 DB에서 다시 materialize하므로 외부에서 DB를 직접 바꾼
  경우도 반영됩니다.
- `feedback`/`audit`/`backups`는 런타임 로그라 파일로 유지됩니다.

단독 모드(메타 DB 미설정)에서는 기존처럼 `data/metadb`의 파일이 원본입니다.

## MCP: 프로파일 발견

`list_db_profiles` 도구로 LLM/에이전트가 사용 가능한 DB 프로파일 id를
발견합니다(권한 반영, 시크릿 제외). 반환된 id를 `run_sql_safely` /
`explain_sql` / `run_evaluation`의 `profile` 인자로 사용합니다.
