#!/usr/bin/env python3
"""Generate the jamypg integration-test environment:
  - deploy/test/init/{postgres,mysql,mariadb}/01-init.sql  (meta schema as text2sql target + seed)
  - data/metadb/*.json                                      (catalog dataset describing the meta schema)
Run from the repo root:  python3 deploy/test/gen_testenv.py
"""
import json, os, uuid
from collections import Counter

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
DATASET = os.path.join(ROOT, "data", "metadb")
INIT = os.path.join(ROOT, "deploy", "test", "init")

# ---------------------------------------------------------------- schema ----
# (table, [(col, pg_type, my_type, pk, fk, ko_name, description, pii)])
TABLES = [
    ("jamypg_users", "사용자", "jamypg 메타 DB의 사용자 계정. 로그인/역할/활성 상태를 관리한다.", [
        ("id",               "TEXT", "VARCHAR(64)",  True,  False, "사용자ID", "사용자 고유 식별자(UUID 또는 slug)", False),
        ("username",         "TEXT", "VARCHAR(128)", False, False, "로그인명", "로그인에 사용하는 사용자명(소문자 유일)", False),
        ("display_name",     "TEXT", "VARCHAR(256)", False, False, "표시이름", "화면에 표시되는 이름", False),
        ("email",            "TEXT", "VARCHAR(256)", False, False, "이메일", "사용자 이메일 주소", True),
        ("password_hash",    "TEXT", "VARCHAR(256)", False, False, "비밀번호해시", "argon2/bcrypt 해시(로컬 계정만)", True),
        ("role",             "TEXT", "VARCHAR(16)",  False, False, "역할", "admin 또는 user", False),
        ("provider",         "TEXT", "VARCHAR(16)",  False, False, "인증제공자", "local 또는 keycloak(SSO)", False),
        ("provider_subject", "TEXT", "VARCHAR(256)", False, False, "SSO주체", "OIDC subject(SSO 계정만)", False),
        ("is_active",        "BOOLEAN", "BOOLEAN",   False, False, "활성여부", "비활성(FALSE) 사용자는 로그인 불가", False),
        ("created_at",       "TIMESTAMPTZ", "DATETIME", False, False, "생성일시", "계정 생성 시각", False),
        ("updated_at",       "TIMESTAMPTZ", "DATETIME", False, False, "수정일시", "계정 수정 시각", False),
        ("last_login_at",    "TIMESTAMPTZ", "DATETIME NULL", False, False, "최종로그인일시", "마지막 로그인 시각(NULL이면 로그인 이력 없음)", False),
    ]),
    ("jamypg_sessions", "세션", "웹 콘솔 로그인 세션. 만료/폐기 시각을 가진다.", [
        ("token_hash", "TEXT", "VARCHAR(128)", True,  False, "토큰해시", "세션 토큰의 SHA-256 해시", True),
        ("user_id",    "TEXT", "VARCHAR(64)",  False, True,  "사용자ID", "세션 소유 사용자(jamypg_users.id)", False),
        ("created_at", "TIMESTAMPTZ", "DATETIME", False, False, "생성일시", "세션 생성 시각", False),
        ("expires_at", "TIMESTAMPTZ", "DATETIME", False, False, "만료일시", "세션 만료 시각", False),
        ("ip",         "TEXT", "VARCHAR(64)",  False, False, "접속IP", "로그인 IP 주소", True),
        ("user_agent", "TEXT", "VARCHAR(512)", False, False, "user-agent", "로그인 브라우저/클라이언트", False),
        ("revoked_at", "TIMESTAMPTZ", "DATETIME NULL", False, False, "폐기일시", "관리자/로그아웃으로 폐기된 시각(NULL이면 유효)", False),
    ]),
    ("jamypg_mcp_keys", "MCP키", "MCP 클라이언트 인증용 API 키. 만료/회전/폐기를 관리한다.", [
        ("id",           "TEXT", "VARCHAR(64)",  True,  False, "키ID", "MCP 키 고유 식별자", False),
        ("user_id",      "TEXT", "VARCHAR(64)",  False, True,  "사용자ID", "키 소유 사용자(jamypg_users.id)", False),
        ("name",         "TEXT", "VARCHAR(128)", False, False, "키이름", "사용자가 붙인 키 별칭", False),
        ("key_hash",     "TEXT", "VARCHAR(128)", False, False, "키해시", "API 키의 SHA-256 해시", True),
        ("key_prefix",   "TEXT", "VARCHAR(16)",  False, False, "키프리픽스", "식별용 앞 8자(마스킹 표시용)", False),
        ("created_at",   "TIMESTAMPTZ", "DATETIME", False, False, "생성일시", "키 발급 시각", False),
        ("expires_at",   "TIMESTAMPTZ", "DATETIME NULL", False, False, "만료일시", "키 만료 시각(NULL이면 무기한)", False),
        ("last_used_at", "TIMESTAMPTZ", "DATETIME NULL", False, False, "최종사용일시", "마지막 사용 시각", False),
        ("revoked_at",   "TIMESTAMPTZ", "DATETIME NULL", False, False, "폐기일시", "폐기 시각(NULL이면 유효)", False),
        ("rotated_from", "TEXT", "VARCHAR(64)",  False, False, "회전원본키", "키 회전 시 이전 키 id", False),
    ]),
    ("jamypg_db_profiles", "DB프로파일", "대상 데이터베이스(postgres/mysql/mariadb) 접속 프로파일 정의(JSON).", [
        ("id",         "TEXT", "VARCHAR(64)",  True,  False, "프로파일ID", "프로파일 고유 식별자", False),
        ("owner_id",   "TEXT", "VARCHAR(64)",  False, True,  "소유자ID", "프로파일 소유 사용자(jamypg_users.id)", False),
        ("definition", "JSONB", "JSON",        False, False, "정의JSON", "type/connect_string/pool/policy 정의", False),
        ("visibility", "TEXT", "VARCHAR(16)",  False, False, "공개범위", "private(소유자만) 또는 shared(전체 공개)", False),
        ("created_at", "TIMESTAMPTZ", "DATETIME", False, False, "생성일시", "프로파일 생성 시각", False),
        ("updated_at", "TIMESTAMPTZ", "DATETIME", False, False, "수정일시", "프로파일 수정 시각", False),
    ]),
    ("jamypg_profile_grants", "프로파일권한", "프로파일 사용/관리 권한 부여 내역.", [
        ("profile_id", "TEXT", "VARCHAR(64)", True,  True, "프로파일ID", "대상 프로파일(jamypg_db_profiles.id)", False),
        ("user_id",    "TEXT", "VARCHAR(64)", True,  True, "사용자ID", "권한을 받은 사용자(jamypg_users.id)", False),
        ("permission", "TEXT", "VARCHAR(16)", False, False, "권한", "use(사용) 또는 manage(관리)", False),
        ("granted_by", "TEXT", "VARCHAR(64)", False, False, "부여자", "권한을 부여한 사용자 id", False),
        ("granted_at", "TIMESTAMPTZ", "DATETIME", False, False, "부여일시", "권한 부여 시각", False),
    ]),
    ("jamypg_settings", "설정", "서버 런타임 설정 키-값 저장소. MySQL/MariaDB에서는 key 컬럼을 백틱(`key`)으로 감싸야 한다.", [
        ("key",   "TEXT", "VARCHAR(128)",  True,  False, "설정키", "설정 항목 이름 (MySQL에서는 예약어라 백틱 필요)", False),
        ("value", "TEXT", "VARCHAR(2048)", False, False, "설정값", "설정 값(문자열)", False),
        ("updated_by",    "TEXT", "VARCHAR(64)",   False, False, "수정자", "마지막으로 수정한 사용자", False),
        ("updated_at",    "TIMESTAMPTZ", "DATETIME", False, False, "수정일시", "마지막 수정 시각", False),
    ]),
    ("jamypg_mcp_activity", "MCP활동", "MCP 도구 호출 활동 로그. text2sql 실행/검증 이력이 쌓인다.", [
        ("id",         "TEXT", "VARCHAR(64)",  True,  False, "활동ID", "활동 로그 고유 식별자", False),
        ("created_at", "TIMESTAMPTZ", "DATETIME", False, False, "발생일시", "도구 호출 시각", False),
        ("user_id",    "TEXT", "VARCHAR(64)",  False, True,  "사용자ID", "호출 사용자(jamypg_users.id)", False),
        ("username",   "TEXT", "VARCHAR(128)", False, False, "사용자명", "호출 사용자명(비정규화)", False),
        ("session_id", "TEXT", "VARCHAR(64)",  False, False, "세션ID", "MCP 세션 식별자", False),
        ("tool",       "TEXT", "VARCHAR(64)",  False, False, "도구", "호출된 MCP 도구명(run_sql_safely, search_schema 등)", False),
        ("kind",       "TEXT", "VARCHAR(32)",  False, False, "종류", "call 또는 error", False),
        ("prompt",     "TEXT", "VARCHAR(2048)", False, False, "질문", "자연어 질문(있는 경우)", False),
        ("sql_text",   "TEXT", "VARCHAR(4096)", False, False, "SQL", "실행/검증된 SQL(있는 경우)", False),
        ("profile",    "TEXT", "VARCHAR(64)",  False, False, "프로파일", "실행 대상 DB 프로파일 id", False),
        ("status",     "TEXT", "VARCHAR(16)",  False, False, "상태", "ok 또는 error", False),
        ("row_count",  "INT",  "INT",          False, False, "행수", "반환된 행 수", False),
        ("elapsed_ms", "BIGINT", "BIGINT",     False, False, "소요ms", "실행 소요 시간(밀리초)", False),
        ("params",     "JSONB", "JSON NULL",   False, False, "파라미터", "도구 호출 파라미터(JSON)", False),
    ]),
]

RELATIONS = [
    ("jamypg_sessions", "user_id", "jamypg_users", "id", "N:1", "세션은 한 사용자에 속한다"),
    ("jamypg_mcp_keys", "user_id", "jamypg_users", "id", "N:1", "MCP 키는 한 사용자에 속한다"),
    ("jamypg_db_profiles", "owner_id", "jamypg_users", "id", "N:1", "프로파일 소유자"),
    ("jamypg_profile_grants", "profile_id", "jamypg_db_profiles", "id", "N:1", "권한이 걸린 프로파일"),
    ("jamypg_profile_grants", "user_id", "jamypg_users", "id", "N:1", "권한을 받은 사용자"),
    ("jamypg_mcp_activity", "user_id", "jamypg_users", "id", "N:1", "활동을 발생시킨 사용자"),
]

# ------------------------------------------------------------------ seed ----
USERS = [
    # id, username, display, email, role, provider, active, created, last_login
    ("u01", "admin",   "관리자",     "admin@example.com",   "admin", "local",    True,  "2026-01-05 09:00:00", "2026-07-09 08:30:00"),
    ("u02", "hkjang",  "장현규",     "hkjang@example.com",  "admin", "local",    True,  "2026-01-10 10:00:00", "2026-07-08 18:10:00"),
    ("u03", "mkim",    "김민지",     "mkim@example.com",    "user",  "local",    True,  "2026-02-01 11:00:00", "2026-07-09 09:12:00"),
    ("u04", "jlee",    "이준호",     "jlee@example.com",    "user",  "local",    True,  "2026-02-15 14:00:00", "2026-07-07 16:40:00"),
    ("u05", "spark",   "박서연",     "spark@example.com",   "user",  "keycloak", True,  "2026-03-02 09:30:00", "2026-07-06 11:05:00"),
    ("u06", "ychoi",   "최유진",     "ychoi@example.com",   "user",  "keycloak", True,  "2026-03-20 13:00:00", "2026-07-01 10:00:00"),
    ("u07", "dkang",   "강도현",     "dkang@example.com",   "user",  "local",    True,  "2026-04-11 15:00:00", None),
    ("u08", "hjeon",   "전하윤",     "hjeon@example.com",   "user",  "local",    True,  "2026-05-06 10:20:00", "2026-06-28 09:00:00"),
    ("u09", "retired1","퇴직자1",    "retired1@example.com","user",  "local",    False, "2026-01-20 09:00:00", "2026-03-31 17:00:00"),
    ("u10", "retired2","퇴직자2",    "retired2@example.com","user",  "local",    False, "2026-02-25 09:00:00", "2026-04-15 12:00:00"),
]

SESSIONS = [
    # token_hash, user, created, expires, ip, ua, revoked
    ("s01", "u01", "2026-07-09 08:30:00", "2026-07-16 08:30:00", "10.0.0.11", "Mozilla/5.0", None),
    ("s02", "u02", "2026-07-08 18:10:00", "2026-07-15 18:10:00", "10.0.0.12", "Mozilla/5.0", None),
    ("s03", "u03", "2026-07-09 09:12:00", "2026-07-16 09:12:00", "10.0.0.13", "Mozilla/5.0", None),
    ("s04", "u04", "2026-07-07 16:40:00", "2026-07-14 16:40:00", "10.0.0.14", "Mozilla/5.0", None),
    ("s05", "u05", "2026-07-06 11:05:00", "2026-07-13 11:05:00", "10.0.0.15", "Mozilla/5.0", None),
    ("s06", "u06", "2026-07-01 10:00:00", "2026-07-08 10:00:00", "10.0.0.16", "Mozilla/5.0", None),
    ("s07", "u03", "2026-06-20 09:00:00", "2026-06-27 09:00:00", "10.0.0.13", "Mozilla/5.0", None),
    ("s08", "u04", "2026-06-25 10:00:00", "2026-07-02 10:00:00", "10.0.0.14", "Mozilla/5.0", "2026-06-26 08:00:00"),
    ("s09", "u08", "2026-06-28 09:00:00", "2026-07-05 09:00:00", "10.0.0.18", "Mozilla/5.0", None),
    ("s10", "u02", "2026-06-15 08:00:00", "2026-06-22 08:00:00", "10.0.0.12", "Mozilla/5.0", "2026-06-16 09:00:00"),
]

KEYS = [
    # id, user, name, prefix, created, expires, last_used, revoked, rotated_from
    ("k01", "u01", "admin-cli",    "jmk_a1b2", "2026-02-01 09:00:00", None,                  "2026-07-09 08:00:00", None, ""),
    ("k02", "u02", "hk-notebook",  "jmk_c3d4", "2026-02-10 09:00:00", "2026-12-31 23:59:59", "2026-07-08 17:00:00", None, ""),
    ("k03", "u03", "mk-batch",     "jmk_e5f6", "2026-03-01 09:00:00", "2026-06-30 23:59:59", "2026-06-29 22:00:00", None, ""),
    ("k04", "u04", "jl-dev",       "jmk_g7h8", "2026-03-15 09:00:00", None,                  "2026-07-05 10:00:00", None, ""),
    ("k05", "u05", "sp-ide",       "jmk_i9j0", "2026-04-01 09:00:00", None,                  None,                  "2026-05-01 09:00:00", ""),
    ("k06", "u02", "hk-notebook2", "jmk_k1l2", "2026-06-01 09:00:00", None,                  "2026-07-09 09:00:00", None, "k02"),
    ("k07", "u06", "yc-cli",       "jmk_m3n4", "2026-06-10 09:00:00", None,                  "2026-07-02 11:00:00", None, ""),
]

PROFILES = [
    ("pg-meta",      "u01", '{"type": "postgres", "connect_string": "postgres-meta:5432/jamypg_meta"}', "shared",  "2026-02-01 09:00:00"),
    ("mysql-meta",   "u02", '{"type": "mysql", "connect_string": "mysql-meta:3306/public"}',            "shared",  "2026-02-05 09:00:00"),
    ("mariadb-meta", "u02", '{"type": "mariadb", "connect_string": "mariadb-meta:3306/public"}',        "shared",  "2026-02-05 09:30:00"),
    ("pg-dev",       "u03", '{"type": "postgres", "connect_string": "dev-host:5432/devdb"}',            "private", "2026-03-10 09:00:00"),
]

GRANTS = [
    ("pg-dev",       "u04", "use",    "u03", "2026-03-11 09:00:00"),
    ("pg-dev",       "u05", "manage", "u03", "2026-03-12 09:00:00"),
    ("pg-meta",      "u03", "manage", "u01", "2026-02-02 09:00:00"),
    ("mysql-meta",   "u03", "use",    "u02", "2026-02-06 09:00:00"),
    ("mariadb-meta", "u03", "use",    "u02", "2026-02-06 09:10:00"),
]

SETTINGS = [
    ("allow_origins", "https://console.example.com", "u01", "2026-02-01 09:00:00"),
    ("session_ttl_hours", "168", "u01", "2026-02-01 09:00:00"),
    ("default_max_rows", "100", "u02", "2026-03-01 09:00:00"),
    ("audit_retention_days", "90", "u01", "2026-04-01 09:00:00"),
]

TOOLS = ["run_sql_safely", "search_schema", "validate_sql", "analyze_question", "explain_sql"]
ACTIVITY = []
_seq = 0
def act(day, hour, user, tool, status, rows, ms, profile="pg-meta", prompt="", sql=""):
    global _seq
    _seq += 1
    uname = dict((u[0], u[1]) for u in USERS)[user]
    ACTIVITY.append((f"a{_seq:03d}", f"2026-07-{day:02d} {hour:02d}:00:00", user, uname,
                     f"sess-{user}", tool, "error" if status == "error" else "call",
                     prompt, sql, profile, status, rows, ms))

# 2026-07-01 ~ 2026-07-09 activity: run_sql_safely heaviest, u03 most active
for day, n_runs in [(1, 3), (2, 4), (3, 2), (6, 5), (7, 4), (8, 6), (9, 4)]:
    for i in range(n_runs):
        user = ["u03", "u02", "u04", "u03", "u05", "u03"][i % 6]
        prof = ["pg-meta", "mysql-meta", "mariadb-meta"][i % 3]
        act(day, 9 + i, user, "run_sql_safely", "ok", 10 + day * i, 120 + 40 * i, prof,
            "활성 사용자 수를 알려줘", "SELECT COUNT(*) FROM public.jamypg_users WHERE is_active = TRUE")
for day in (2, 6, 8):
    act(day, 14, "u04", "run_sql_safely", "error", 0, 5000, "pg-meta", "지난달 활동", "SELECT bad_col FROM public.jamypg_mcp_activity")
for day in (1, 2, 3, 6, 7, 8, 9):
    act(day, 10, "u03", "search_schema", "ok", 0, 35)
    act(day, 11, "u02", "validate_sql", "ok", 0, 20)
for day in (6, 7, 8):
    act(day, 13, "u05", "analyze_question", "ok", 0, 15)
    act(day, 15, "u06", "explain_sql", "ok", 0, 80, "mysql-meta")

# ------------------------------------------------------------- SQL emit ----
def sql_str(v):
    if v is None:
        return "NULL"
    if isinstance(v, bool):
        return "TRUE" if v else "FALSE"
    if isinstance(v, (int, float)):
        return str(v)
    return "'" + str(v).replace("'", "''") + "'"

def emit_inserts(qualify, quote=lambda c: c):
    out = []
    def ins(table, cols, rows):
        qcols = [quote(c) for c in cols]
        for r in rows:
            out.append(f"INSERT INTO {qualify}{table} ({', '.join(qcols)}) VALUES ({', '.join(sql_str(v) for v in r)});")
    ins("jamypg_users",
        ["id", "username", "display_name", "email", "password_hash", "role", "provider", "provider_subject", "is_active", "created_at", "updated_at", "last_login_at"],
        [(u[0], u[1], u[2], u[3], "x" * 8, u[4], u[5], (u[1] if u[5] == "keycloak" else None), u[6], u[7], u[7], u[8]) for u in USERS])
    ins("jamypg_sessions",
        ["token_hash", "user_id", "created_at", "expires_at", "ip", "user_agent", "revoked_at"],
        [("hash-" + s[0], s[1], s[2], s[3], s[4], s[5], s[6]) for s in SESSIONS])
    ins("jamypg_mcp_keys",
        ["id", "user_id", "name", "key_hash", "key_prefix", "created_at", "expires_at", "last_used_at", "revoked_at", "rotated_from"],
        [(k[0], k[1], k[2], "hash-" + k[0], k[3], k[4], k[5], k[6], k[7], k[8]) for k in KEYS])
    ins("jamypg_db_profiles",
        ["id", "owner_id", "definition", "visibility", "created_at", "updated_at"],
        [(p[0], p[1], p[2], p[3], p[4], p[4]) for p in PROFILES])
    ins("jamypg_profile_grants",
        ["profile_id", "user_id", "permission", "granted_by", "granted_at"], GRANTS)
    ins("jamypg_settings",
        ["key", "value", "updated_by", "updated_at"], SETTINGS)
    ins("jamypg_mcp_activity",
        ["id", "created_at", "user_id", "username", "session_id", "tool", "kind", "prompt", "sql_text", "profile", "status", "row_count", "elapsed_ms"],
        ACTIVITY)
    return "\n".join(out)

def pg_init():
    out = ["-- jamypg meta schema as text2sql target (PostgreSQL). Auto-generated by gen_testenv.py.",
           "-- The same tables double as the live jamypg meta DB (server migrations are IF NOT EXISTS)."]
    for table, _, _, cols in TABLES:
        pks = [c[0] for c in cols if c[3]]
        lines = [f"\t{c[0]} {c[1].replace(' NULL', '')}" for c in cols]
        lines.append(f"\tPRIMARY KEY ({', '.join(pks)})")
        out.append(f"CREATE TABLE IF NOT EXISTS {table} (\n" + ",\n".join(lines) + "\n);")
    out.append(emit_inserts(""))
    out.append("""
CREATE ROLE jamypg_ro LOGIN PASSWORD 'jamypg_ro_pw';
GRANT USAGE ON SCHEMA public TO jamypg_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO jamypg_ro;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO jamypg_ro;
""")
    return "\n\n".join(out) + "\n"

def my_init():
    out = ["-- jamypg meta schema as text2sql target (MySQL/MariaDB). Auto-generated by gen_testenv.py.",
           "-- The database is named `public` so schema-qualified SQL matches PostgreSQL.",
           "CREATE DATABASE IF NOT EXISTS public;", "USE public;"]
    for table, _, _, cols in TABLES:
        pks = [f"`{c[0]}`" if c[0] in ("key", "value") else c[0] for c in cols if c[3]]
        lines = []
        for c in cols:
            typ = c[2]
            name = f"`{c[0]}`" if c[0] in ("key", "value") else c[0]
            null = "" if typ.endswith("NULL") or c[0] in ("provider_subject", "expires_at", "last_used_at", "revoked_at", "last_login_at", "password_hash") else " NOT NULL"
            lines.append(f"\t{name} {typ}{null}" if not typ.endswith("NULL") else f"\t{name} {typ}")
        lines.append(f"\tPRIMARY KEY ({', '.join(pks)})")
        out.append(f"CREATE TABLE IF NOT EXISTS {table} (\n" + ",\n".join(lines) + "\n);")
    out.append(emit_inserts("public.", quote=lambda c: f"`{c}`" if c in ("key", "value") else c))
    out.append("""
CREATE USER IF NOT EXISTS 'jamypg_ro'@'%' IDENTIFIED BY 'jamypg_ro_pw';
GRANT SELECT ON public.* TO 'jamypg_ro'@'%';
FLUSH PRIVILEGES;
""")
    return "\n\n".join(out) + "\n"

# --------------------------------------------------------- dataset emit ----
def dataset():
    os.makedirs(DATASET, exist_ok=True)
    phys, logi, rels = [], [], []
    for table, ko, desc, cols in TABLES:
        for i, c in enumerate(cols, 1):
            phys.append({
                "id": str(uuid.uuid5(uuid.NAMESPACE_DNS, f"{table}.{c[0]}")),
                "schema_name": "public", "table_name": table,
                "column_order": str(i), "column_name": c[0],
                "data_type": c[1].replace(" NULL", ""), "length_precision": "",
                "null_constraint": "", "is_pk": "Y" if c[3] else "N", "is_fk": "Y" if c[4] else "N",
                "description": desc, "version": 1,
            })
            logi.append({
                "schema_name": "public", "entity_name_en": table, "entity_name_ko": ko,
                "entity_order": str(i), "attribute_name_ko": c[5], "attribute_name_en": c[0],
                "data_type": c[1].replace(" NULL", ""), "length_precision": "",
                "is_pk": "Y" if c[3] else "N", "is_fk": "Y" if c[4] else "N",
                "description": c[6], "note": "", "version": 1,
            })
    for i, (bt, bc, rt, rc, card, d) in enumerate(RELATIONS, 1):
        rels.append({
            "id": i, "base_schema": "public", "base_table": bt, "base_column": bc,
            "reference_schema": "public", "reference_table": rt, "reference_column": rc,
            "cardinality": card, "join_type": "INNER", "provision_type": "FK",
            "description": d, "meta_version": 1,
        })

    overrides = {
        "dialect": "postgres",
        "pii_columns": ["*.email", "*.password_hash", "*.token_hash", "*.key_hash", "*.ip"],
        "tables": [
            {"table": "public.jamypg_users", "domain": "인증", "grain": "사용자 1행", "row_count": len(USERS)},
            {"table": "public.jamypg_sessions", "domain": "인증", "grain": "세션 1행", "row_count": len(SESSIONS)},
            {"table": "public.jamypg_mcp_keys", "domain": "인증", "grain": "MCP 키 1행", "row_count": len(KEYS)},
            {"table": "public.jamypg_db_profiles", "domain": "커넥터", "grain": "프로파일 1행", "row_count": len(PROFILES)},
            {"table": "public.jamypg_profile_grants", "domain": "커넥터", "grain": "프로파일-사용자 권한 1행", "row_count": len(GRANTS)},
            {"table": "public.jamypg_settings", "domain": "운영", "grain": "설정 키 1행", "row_count": len(SETTINGS)},
            {"table": "public.jamypg_mcp_activity", "domain": "감사", "grain": "도구 호출 1행", "row_count": len(ACTIVITY)},
        ],
        "columns": [
            {"table": "public.jamypg_users", "column": "role", "sample_values": ["admin", "user"]},
            {"table": "public.jamypg_users", "column": "provider", "sample_values": ["local", "keycloak"]},
            {"table": "public.jamypg_db_profiles", "column": "visibility", "sample_values": ["private", "shared"]},
            {"table": "public.jamypg_profile_grants", "column": "permission", "sample_values": ["use", "manage"]},
            {"table": "public.jamypg_mcp_activity", "column": "tool", "sample_values": TOOLS},
            {"table": "public.jamypg_mcp_activity", "column": "status", "sample_values": ["ok", "error"]},
            {"table": "public.jamypg_mcp_activity", "column": "created_at", "semantic_type": "TIMESTAMP"},
            {"table": "public.jamypg_users", "column": "created_at", "semantic_type": "TIMESTAMP"},
            {"table": "public.jamypg_users", "column": "last_login_at", "semantic_type": "TIMESTAMP"},
        ],
    }

    glossary = {"entries": [
        {"term": "사용자", "category": "entity", "synonyms": ["유저", "계정", "user", "account", "멤버"]},
        {"term": "활성 사용자", "category": "entity", "synonyms": ["active user", "유효 사용자", "is_active"]},
        {"term": "세션", "category": "entity", "synonyms": ["session", "로그인 세션", "접속"]},
        {"term": "MCP 키", "category": "entity", "synonyms": ["api key", "api 키", "mcp key", "키"]},
        {"term": "프로파일", "category": "entity", "synonyms": ["profile", "접속 프로파일", "db 프로파일", "커넥션"]},
        {"term": "활동", "category": "entity", "synonyms": ["activity", "호출", "로그", "이력", "audit"]},
        {"term": "도구", "category": "entity", "synonyms": ["tool", "mcp tool", "툴"]},
        {"term": "관리자", "category": "entity", "synonyms": ["admin", "어드민"]},
        {"term": "공개범위", "category": "dimension", "synonyms": ["visibility", "공유", "비공개", "shared", "private"]},
        {"term": "폐기", "category": "dimension", "synonyms": ["revoked", "revoked_at", "폐기일시", "회수"]},
        {"term": "상태", "category": "dimension", "synonyms": ["status", "실패", "성공", "오류"]},
    ]}

    samples = [
        {"id": 1, "question": "활성 사용자 수를 알려줘",
         "target_sql": "SELECT COUNT(*) AS active_users FROM public.jamypg_users WHERE is_active = TRUE",
         "target_domain": "인증", "target_table": "public.jamypg_users",
         "target_column": "jamypg_users.is_active", "target_intent": "agg_count|cond_compare"},
        {"id": 2, "question": "역할별 사용자 수를 보여줘",
         "target_sql": "SELECT role, COUNT(*) AS cnt FROM public.jamypg_users GROUP BY role ORDER BY cnt DESC",
         "target_domain": "인증", "target_table": "public.jamypg_users",
         "target_column": "jamypg_users.role", "target_intent": "agg_count|group_by"},
        {"id": 3, "question": "폐기되지 않은 유효한 MCP 키 개수는?",
         "target_sql": "SELECT COUNT(*) AS live_keys FROM public.jamypg_mcp_keys WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)",
         "target_domain": "인증", "target_table": "public.jamypg_mcp_keys",
         "target_column": "jamypg_mcp_keys.revoked_at|jamypg_mcp_keys.expires_at", "target_intent": "agg_count|cond_null|cond_logic"},
        {"id": 4, "question": "가장 많이 호출된 MCP 도구 상위 3개를 알려줘",
         "target_sql": "SELECT tool, COUNT(*) AS calls FROM public.jamypg_mcp_activity GROUP BY tool ORDER BY calls DESC LIMIT 3",
         "target_domain": "감사", "target_table": "public.jamypg_mcp_activity",
         "target_column": "jamypg_mcp_activity.tool", "target_intent": "agg_count|group_by|top_n"},
        {"id": 5, "question": "사용자별 도구 호출 건수를 많은 순으로 보여줘",
         "target_sql": "SELECT u.username, COUNT(*) AS calls FROM public.jamypg_mcp_activity a JOIN public.jamypg_users u ON a.user_id = u.id GROUP BY u.username ORDER BY calls DESC",
         "target_domain": "감사", "target_table": "public.jamypg_mcp_activity",
         "target_column": "jamypg_mcp_activity.user_id|jamypg_users.username", "target_intent": "agg_count|join|group_by"},
        {"id": 6, "question": "공유(shared) 상태인 DB 프로파일 목록을 보여줘",
         "target_sql": "SELECT id, owner_id, visibility, created_at FROM public.jamypg_db_profiles WHERE visibility = 'shared' ORDER BY created_at",
         "target_domain": "커넥터", "target_table": "public.jamypg_db_profiles",
         "target_column": "jamypg_db_profiles.visibility", "target_intent": "cond_compare|list"},
        {"id": 7, "question": "일별 run_sql_safely 실행 건수를 보여줘",
         "target_sql": "SELECT CAST(created_at AS DATE) AS d, COUNT(*) AS runs FROM public.jamypg_mcp_activity WHERE tool = 'run_sql_safely' GROUP BY CAST(created_at AS DATE) ORDER BY d",
         "target_domain": "감사", "target_table": "public.jamypg_mcp_activity",
         "target_column": "jamypg_mcp_activity.created_at|jamypg_mcp_activity.tool", "target_intent": "agg_count|group_by|time_series"},
        {"id": 8, "question": "도구별 평균 실행 시간(ms)이 큰 순서로 알려줘",
         "target_sql": "SELECT tool, ROUND(AVG(elapsed_ms), 1) AS avg_ms FROM public.jamypg_mcp_activity GROUP BY tool ORDER BY avg_ms DESC",
         "target_domain": "감사", "target_table": "public.jamypg_mcp_activity",
         "target_column": "jamypg_mcp_activity.elapsed_ms", "target_intent": "agg_avg|group_by"},
        {"id": 9, "question": "실패(error)한 쿼리 실행이 있었던 사용자와 건수는?",
         "target_sql": "SELECT u.username, COUNT(*) AS errors FROM public.jamypg_mcp_activity a JOIN public.jamypg_users u ON a.user_id = u.id WHERE a.status = 'error' GROUP BY u.username ORDER BY errors DESC",
         "target_domain": "감사", "target_table": "public.jamypg_mcp_activity",
         "target_column": "jamypg_mcp_activity.status", "target_intent": "agg_count|join|cond_compare"},
        {"id": 10, "question": "프로파일별로 use 권한을 받은 사용자 수를 보여줘",
         "target_sql": "SELECT p.id AS profile_id, COUNT(g.user_id) AS grantees FROM public.jamypg_db_profiles p LEFT JOIN public.jamypg_profile_grants g ON g.profile_id = p.id AND g.permission = 'use' GROUP BY p.id ORDER BY grantees DESC",
         "target_domain": "커넥터", "target_table": "public.jamypg_db_profiles",
         "target_column": "jamypg_profile_grants.permission", "target_intent": "agg_count|join|group_by"},
        # ids 11-12 are dataset-only additions (not golden cases): they teach the
        # catalog that time filters on jamypg_mcp_activity target created_at, so
        # skeleton generation picks it as the period column.
        {"id": 11, "question": "2026년 7월에 발생한 도구 호출 수를 알려줘",
         "target_sql": "SELECT COUNT(*) AS calls FROM public.jamypg_mcp_activity WHERE created_at >= DATE '2026-07-01' AND created_at < DATE '2026-08-01'",
         "target_domain": "감사", "target_table": "public.jamypg_mcp_activity",
         "target_column": "jamypg_mcp_activity.created_at", "target_intent": "agg_count|cond_range"},
        {"id": 12, "question": "최근 도구 호출 내역을 시간 순으로 20건 보여줘",
         "target_sql": "SELECT id, created_at, tool, status, elapsed_ms FROM public.jamypg_mcp_activity ORDER BY created_at DESC LIMIT 20",
         "target_domain": "감사", "target_table": "public.jamypg_mcp_activity",
         "target_column": "jamypg_mcp_activity.created_at", "target_intent": "list|sort_order|limit_topn"},
    ]

    golden = [
        {"id": 1, "question": "활성 사용자 수를 알려줘",
         "expected_tables": ["public.jamypg_users"], "expected_columns": ["is_active"], "expected_metrics": [],
         "expected_sql": "SELECT COUNT(*) AS active_users FROM public.jamypg_users T1 WHERE T1.is_active = TRUE",
         "expected_row_count": 1},
        {"id": 2, "question": "역할별 사용자 수를 보여줘",
         "expected_tables": ["public.jamypg_users"], "expected_columns": ["role"], "expected_metrics": [],
         "expected_sql": "SELECT T1.role, COUNT(*) AS cnt FROM public.jamypg_users T1 GROUP BY T1.role ORDER BY cnt DESC"},
        {"id": 3, "question": "가장 많이 호출된 MCP 도구 상위 3개를 알려줘",
         "expected_tables": ["public.jamypg_mcp_activity"], "expected_columns": ["tool"], "expected_metrics": [],
         "expected_sql": "SELECT T1.tool, COUNT(*) AS calls FROM public.jamypg_mcp_activity T1 GROUP BY T1.tool ORDER BY calls DESC LIMIT 3"},
        {"id": 4, "question": "사용자별 도구 호출 건수를 많은 순으로 보여줘",
         "expected_tables": ["public.jamypg_mcp_activity", "public.jamypg_users"],
         "expected_columns": ["user_id", "username"], "expected_metrics": [],
         "expected_sql": "SELECT T2.username, COUNT(*) AS calls FROM public.jamypg_mcp_activity T1 JOIN public.jamypg_users T2 ON T1.user_id = T2.id GROUP BY T2.username ORDER BY calls DESC"},
        {"id": 5, "question": "공유 상태인 DB 프로파일 목록을 보여줘",
         "expected_tables": ["public.jamypg_db_profiles"], "expected_columns": ["visibility"], "expected_metrics": [],
         "expected_sql": "SELECT T1.id, T1.visibility, T1.created_at FROM public.jamypg_db_profiles T1 WHERE T1.visibility = 'shared' ORDER BY T1.created_at"},
        {"id": 6, "question": "폐기되지 않은 유효한 MCP 키 개수는?",
         "expected_tables": ["public.jamypg_mcp_keys"], "expected_columns": ["revoked_at", "expires_at"], "expected_metrics": [],
         "expected_sql": "SELECT COUNT(*) AS live_keys FROM public.jamypg_mcp_keys T1 WHERE T1.revoked_at IS NULL AND (T1.expires_at IS NULL OR T1.expires_at > CURRENT_TIMESTAMP)"},
        {"id": 7, "question": "도구별 평균 실행 시간이 큰 순서로 알려줘",
         "expected_tables": ["public.jamypg_mcp_activity"], "expected_columns": ["tool", "elapsed_ms"], "expected_metrics": [],
         "expected_sql": "SELECT T1.tool, AVG(T1.elapsed_ms) AS avg_ms FROM public.jamypg_mcp_activity T1 GROUP BY T1.tool ORDER BY avg_ms DESC"},
        {"id": 8, "question": "실패한 도구 호출이 있었던 사용자와 건수는?",
         "expected_tables": ["public.jamypg_mcp_activity", "public.jamypg_users"],
         "expected_columns": ["status", "username"], "expected_metrics": [],
         "expected_sql": "SELECT T2.username, COUNT(*) AS errors FROM public.jamypg_mcp_activity T1 JOIN public.jamypg_users T2 ON T1.user_id = T2.id WHERE T1.status = 'error' GROUP BY T2.username ORDER BY errors DESC"},
    ]

    databases = [{"id": 1, "dbms": "POSTGRES", "port": 55432, "name": "jamypg_meta", "alias": "meta"}]

    profiles = [
        {"id": "pg-meta", "name": "PostgreSQL 메타DB", "type": "postgres",
         "connect_string": "127.0.0.1:55432/jamypg_meta", "username": "jamypg_ro", "password_ref": "plain:jamypg_ro_pw"},
        {"id": "mysql-meta", "name": "MySQL 메타DB 사본", "type": "mysql",
         "connect_string": "127.0.0.1:53306/public", "username": "jamypg_ro", "password_ref": "plain:jamypg_ro_pw"},
        {"id": "mariadb-meta", "name": "MariaDB 메타DB 사본", "type": "mariadb",
         "connect_string": "127.0.0.1:53307/public", "username": "jamypg_ro", "password_ref": "plain:jamypg_ro_pw"},
    ]

    metrics = [
        {"name": "도구 호출 수", "business_name": "MCP 도구 호출 수", "aliases": ["호출 수", "호출 건수", "call count", "실행 건수"],
         "description": "조건을 만족하는 MCP 도구 호출(활동 로그)의 수.", "expression": "COUNT(*)", "aggregation": "COUNT",
         "tables": ["public.jamypg_mcp_activity"], "columns": ["id"]},
        {"name": "평균 실행시간", "business_name": "평균 실행 시간(ms)", "aliases": ["평균 소요시간", "avg elapsed", "평균 실행 시간"],
         "description": "도구 호출의 평균 실행 시간(밀리초).", "expression": "AVG(ELAPSED_MS)", "aggregation": "AVG",
         "tables": ["public.jamypg_mcp_activity"], "columns": ["elapsed_ms"]},
        {"name": "총 반환 행 수", "business_name": "총 반환 행 수", "aliases": ["행 수 합계", "총 행수", "row count 합"],
         "description": "도구 호출이 반환한 행 수의 합계.", "expression": "SUM(ROW_COUNT)", "aggregation": "SUM",
         "tables": ["public.jamypg_mcp_activity"], "columns": ["row_count"]},
        {"name": "사용자 수", "business_name": "사용자 수", "aliases": ["유저 수", "계정 수", "user count", "몇 명"],
         "description": "조건을 만족하는 사용자 계정의 수.", "expression": "COUNT(*)", "aggregation": "COUNT",
         "tables": ["public.jamypg_users"], "columns": ["id"]},
    ]

    code_dict = [
        {"schema_name": "public", "table_name": "jamypg_users", "column_name": "role",
         "common_division_code": "ROLE", "code_dict_txt": "admin:관리자, user:일반 사용자"},
        {"schema_name": "public", "table_name": "jamypg_users", "column_name": "provider",
         "common_division_code": "PROVIDER", "code_dict_txt": "local:로컬 계정, keycloak:SSO 계정"},
        {"schema_name": "public", "table_name": "jamypg_mcp_activity", "column_name": "status",
         "common_division_code": "ACT_STATUS", "code_dict_txt": "ok:정상 실행, error:실행 오류"},
        {"schema_name": "public", "table_name": "jamypg_mcp_activity", "column_name": "kind",
         "common_division_code": "ACT_KIND", "code_dict_txt": "call:정상 호출, error:오류 호출"},
        {"schema_name": "public", "table_name": "jamypg_db_profiles", "column_name": "visibility",
         "common_division_code": "VISIBILITY", "code_dict_txt": "private:소유자 전용, shared:전체 공개"},
        {"schema_name": "public", "table_name": "jamypg_profile_grants", "column_name": "permission",
         "common_division_code": "PERMISSION", "code_dict_txt": "use:사용 권한, manage:관리 권한"},
    ]

    tool_top = [{"value": t, "count": n} for t, n in
                sorted(Counter(a[5] for a in ACTIVITY).items(), key=lambda kv: (-kv[1], kv[0]))]
    column_stats = [
        {"schema_name": "public", "table_name": "jamypg_mcp_activity", "column_name": "tool",
         "row_count": len(ACTIVITY), "null_ratio": 0.0, "distinct_count": len(tool_top), "top_values": tool_top},
        {"schema_name": "public", "table_name": "jamypg_mcp_activity", "column_name": "elapsed_ms",
         "row_count": len(ACTIVITY), "null_ratio": 0.0, "distinct_count": 20, "min": "15", "max": "5000"},
        {"schema_name": "public", "table_name": "jamypg_users", "column_name": "role",
         "row_count": len(USERS), "null_ratio": 0.0, "distinct_count": 2,
         "top_values": [{"value": "user", "count": 8}, {"value": "admin", "count": 2}]},
        {"schema_name": "public", "table_name": "jamypg_mcp_activity", "column_name": "created_at",
         "row_count": len(ACTIVITY), "null_ratio": 0.0, "distinct_count": 30,
         "min": "2026-07-01 09:00:00", "max": "2026-07-09 15:00:00", "last_updated": "20260709"},
    ]

    def w(name, obj):
        with open(os.path.join(DATASET, name), "w", encoding="utf-8") as f:
            json.dump(obj, f, ensure_ascii=False, indent=1)
            f.write("\n")
    w("meta_physical_models.json", phys)
    w("meta_logical_models.json", logi)
    w("topology_relations.json", rels)
    w("overrides.json", overrides)
    w("glossary.json", glossary)
    w("sql_datasets.json", samples)
    w("golden_queries.json", golden)
    w("databases.json", databases)
    w("db_profiles.json", profiles)
    w("metrics.json", metrics)
    w("meta_code_dict.json", code_dict)
    w("column_stats.json", column_stats)

if __name__ == "__main__":
    for sub in ("postgres", "mysql", "mariadb"):
        os.makedirs(os.path.join(INIT, sub), exist_ok=True)
    with open(os.path.join(INIT, "postgres", "01-init.sql"), "w", encoding="utf-8") as f:
        f.write(pg_init())
    my = my_init()
    for sub in ("mysql", "mariadb"):
        with open(os.path.join(INIT, sub, "01-init.sql"), "w", encoding="utf-8") as f:
            f.write(my)
    dataset()
    print("generated: deploy/test/init/{postgres,mysql,mariadb}/01-init.sql and data/metadb/*.json")
