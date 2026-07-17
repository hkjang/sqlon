#!/usr/bin/env python3
"""Seed three famous open-source service schemas into the jamypg test DBs and
generate matching text2sql catalog datasets with known-answer golden queries:

  - sakila    (MySQL의 대표 샘플 DB — DVD 렌탈)
  - northwind (고전 주문/제품/고객 스키마)
  - wordpress (가장 널리 쓰이는 오픈소스 CMS의 핵심 테이블)

Outputs:
  deploy/test/init/{postgres,mysql,mariadb}/02-oss.sql
  data/{sakila,northwind,wordpress}/*.json

Cross-engine parity: on PostgreSQL each schema is a schema in jamypg_meta; on
MySQL/MariaDB each schema is a database of the same name, so schema-qualified
SQL (e.g. sakila.film) runs unchanged on all three engines. Monetary columns
are integer cents so aggregates compare exactly across engines.

Run from the repo root:  python3 deploy/test/gen_oss_testenv.py
"""
import json, os, uuid
from collections import Counter, defaultdict

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
INIT = os.path.join(ROOT, "deploy", "test", "init")
DATA = os.path.join(ROOT, "data")

# =====================================================================
# schema definitions: (table, ko_name, table_desc, [(col, pg, my, pk, fk, ko, desc)])
# =====================================================================

SAKILA_TABLES = [
    ("category", "카테고리", "영화 장르 카테고리 마스터.", [
        ("category_id", "INT", "INT", True, False, "카테고리ID", "카테고리 고유 번호", False),
        ("name", "TEXT", "VARCHAR(64)", False, False, "카테고리명", "장르 이름 (Action, Comedy...)", False),
    ]),
    ("actor", "배우", "배우 마스터.", [
        ("actor_id", "INT", "INT", True, False, "배우ID", "배우 고유 번호", False),
        ("first_name", "TEXT", "VARCHAR(64)", False, False, "이름", "배우 이름", False),
        ("last_name", "TEXT", "VARCHAR(64)", False, False, "성", "배우 성", False),
    ]),
    ("film", "영화", "대여 가능한 영화 마스터. rental_rate는 센트 단위 정수.", [
        ("film_id", "INT", "INT", True, False, "영화ID", "영화 고유 번호", False),
        ("title", "TEXT", "VARCHAR(256)", False, False, "제목", "영화 제목", False),
        ("release_year", "INT", "INT", False, False, "개봉연도", "개봉 연도", False),
        ("rental_duration", "INT", "INT", False, False, "대여기간", "기본 대여 기간(일)", False),
        ("rental_rate", "INT", "INT", False, False, "대여료", "대여료(센트 단위 정수)", False),
        ("length", "INT", "INT", False, False, "상영시간", "상영 시간(분)", False),
        ("rating", "TEXT", "VARCHAR(8)", False, False, "등급", "관람 등급 (G/PG/PG-13/R)", False),
    ]),
    ("film_category", "영화-카테고리", "영화와 카테고리 매핑(N:M).", [
        ("film_id", "INT", "INT", True, True, "영화ID", "영화(sakila.film)", False),
        ("category_id", "INT", "INT", True, True, "카테고리ID", "카테고리(sakila.category)", False),
    ]),
    ("film_actor", "영화-배우", "영화와 배우 출연 매핑(N:M).", [
        ("actor_id", "INT", "INT", True, True, "배우ID", "배우(sakila.actor)", False),
        ("film_id", "INT", "INT", True, True, "영화ID", "영화(sakila.film)", False),
    ]),
    ("inventory", "재고", "매장별 보유 영화 재고(대여 단위).", [
        ("inventory_id", "INT", "INT", True, False, "재고ID", "재고 고유 번호", False),
        ("film_id", "INT", "INT", False, True, "영화ID", "영화(sakila.film)", False),
        ("store_id", "INT", "INT", False, False, "매장ID", "보유 매장 번호(1 또는 2)", False),
    ]),
    ("customer", "고객", "대여 고객 마스터.", [
        ("customer_id", "INT", "INT", True, False, "고객ID", "고객 고유 번호", False),
        ("first_name", "TEXT", "VARCHAR(64)", False, False, "이름", "고객 이름", False),
        ("last_name", "TEXT", "VARCHAR(64)", False, False, "성", "고객 성", False),
        ("email", "TEXT", "VARCHAR(256)", False, False, "이메일", "고객 이메일", True),
        ("active", "BOOLEAN", "BOOLEAN", False, False, "활성여부", "활성 고객 여부", False),
        ("create_date", "TIMESTAMPTZ", "DATETIME", False, False, "가입일시", "고객 등록 시각", False),
    ]),
    ("rental", "대여", "재고 단위 대여 이력. return_date가 NULL이면 미반납.", [
        ("rental_id", "INT", "INT", True, False, "대여ID", "대여 고유 번호", False),
        ("rental_date", "TIMESTAMPTZ", "DATETIME", False, False, "대여일시", "대여 시각", False),
        ("inventory_id", "INT", "INT", False, True, "재고ID", "대여된 재고(sakila.inventory)", False),
        ("customer_id", "INT", "INT", False, True, "고객ID", "대여 고객(sakila.customer)", False),
        ("return_date", "TIMESTAMPTZ", "DATETIME NULL", False, False, "반납일시", "반납 시각(NULL이면 미반납)", False),
    ]),
    ("payment", "결제", "대여 결제 이력. amount는 센트 단위 정수.", [
        ("payment_id", "INT", "INT", True, False, "결제ID", "결제 고유 번호", False),
        ("customer_id", "INT", "INT", False, True, "고객ID", "결제 고객(sakila.customer)", False),
        ("rental_id", "INT", "INT", False, True, "대여ID", "결제 대상 대여(sakila.rental)", False),
        ("amount", "INT", "INT", False, False, "결제금액", "결제 금액(센트 단위 정수)", False),
        ("payment_date", "TIMESTAMPTZ", "DATETIME", False, False, "결제일시", "결제 시각", False),
    ]),
]

SAKILA_RELS = [
    ("film_category", "film_id", "film", "film_id", "N:1", "매핑된 영화"),
    ("film_category", "category_id", "category", "category_id", "N:1", "매핑된 카테고리"),
    ("film_actor", "film_id", "film", "film_id", "N:1", "출연 영화"),
    ("film_actor", "actor_id", "actor", "actor_id", "N:1", "출연 배우"),
    ("inventory", "film_id", "film", "film_id", "N:1", "재고의 영화"),
    ("rental", "inventory_id", "inventory", "inventory_id", "N:1", "대여된 재고"),
    ("rental", "customer_id", "customer", "customer_id", "N:1", "대여 고객"),
    ("payment", "customer_id", "customer", "customer_id", "N:1", "결제 고객"),
    ("payment", "rental_id", "rental", "rental_id", "N:1", "결제 대상 대여"),
]

NORTHWIND_TABLES = [
    ("categories", "제품카테고리", "제품 카테고리 마스터.", [
        ("category_id", "INT", "INT", True, False, "카테고리ID", "카테고리 고유 번호", False),
        ("category_name", "TEXT", "VARCHAR(64)", False, False, "카테고리명", "카테고리 이름", False),
        ("description", "TEXT", "VARCHAR(256)", False, False, "설명", "카테고리 설명", False),
    ]),
    ("suppliers", "공급업체", "제품 공급업체 마스터.", [
        ("supplier_id", "INT", "INT", True, False, "공급업체ID", "공급업체 고유 번호", False),
        ("company_name", "TEXT", "VARCHAR(128)", False, False, "회사명", "공급업체 회사명", False),
        ("country", "TEXT", "VARCHAR(64)", False, False, "국가", "공급업체 국가", False),
    ]),
    ("products", "제품", "판매 제품 마스터. unit_price는 센트 단위 정수.", [
        ("product_id", "INT", "INT", True, False, "제품ID", "제품 고유 번호", False),
        ("product_name", "TEXT", "VARCHAR(128)", False, False, "제품명", "제품 이름", False),
        ("supplier_id", "INT", "INT", False, True, "공급업체ID", "공급업체(northwind.suppliers)", False),
        ("category_id", "INT", "INT", False, True, "카테고리ID", "카테고리(northwind.categories)", False),
        ("unit_price", "INT", "INT", False, False, "단가", "단가(센트 단위 정수)", False),
        ("units_in_stock", "INT", "INT", False, False, "재고수량", "현재 재고 수량", False),
        ("discontinued", "BOOLEAN", "BOOLEAN", False, False, "단종여부", "단종 여부(TRUE=단종)", False),
    ]),
    ("customers", "고객사", "주문 고객사 마스터.", [
        ("customer_id", "TEXT", "VARCHAR(8)", True, False, "고객사ID", "고객사 코드(5자)", False),
        ("company_name", "TEXT", "VARCHAR(128)", False, False, "회사명", "고객사 회사명", False),
        ("country", "TEXT", "VARCHAR(64)", False, False, "국가", "고객사 국가", False),
        ("city", "TEXT", "VARCHAR(64)", False, False, "도시", "고객사 도시", False),
    ]),
    ("employees", "직원", "주문 처리 직원 마스터.", [
        ("employee_id", "INT", "INT", True, False, "직원ID", "직원 고유 번호", False),
        ("last_name", "TEXT", "VARCHAR(64)", False, False, "성", "직원 성", False),
        ("first_name", "TEXT", "VARCHAR(64)", False, False, "이름", "직원 이름", False),
        ("title", "TEXT", "VARCHAR(64)", False, False, "직함", "직함", False),
        ("hire_date", "TIMESTAMPTZ", "DATETIME", False, False, "입사일", "입사 일자", False),
    ]),
    ("shippers", "배송업체", "배송 업체 마스터.", [
        ("shipper_id", "INT", "INT", True, False, "배송업체ID", "배송업체 고유 번호", False),
        ("company_name", "TEXT", "VARCHAR(128)", False, False, "회사명", "배송업체 회사명", False),
    ]),
    ("orders", "주문", "고객사 주문 헤더. shipped_date가 NULL이면 미배송.", [
        ("order_id", "INT", "INT", True, False, "주문ID", "주문 고유 번호", False),
        ("customer_id", "TEXT", "VARCHAR(8)", False, True, "고객사ID", "주문 고객사(northwind.customers)", False),
        ("employee_id", "INT", "INT", False, True, "직원ID", "처리 직원(northwind.employees)", False),
        ("order_date", "TIMESTAMPTZ", "DATETIME", False, False, "주문일", "주문 일자", False),
        ("shipped_date", "TIMESTAMPTZ", "DATETIME NULL", False, False, "배송일", "배송 일자(NULL이면 미배송)", False),
        ("ship_via", "INT", "INT", False, True, "배송업체ID", "배송업체(northwind.shippers)", False),
        ("freight", "INT", "INT", False, False, "운임", "운임(센트 단위 정수)", False),
        ("ship_country", "TEXT", "VARCHAR(64)", False, False, "배송국가", "배송지 국가", False),
    ]),
    ("order_details", "주문상세", "주문 라인 아이템. unit_price는 센트 단위 정수, discount_pct는 % 정수.", [
        ("order_id", "INT", "INT", True, True, "주문ID", "주문(northwind.orders)", False),
        ("product_id", "INT", "INT", True, True, "제품ID", "제품(northwind.products)", False),
        ("unit_price", "INT", "INT", False, False, "단가", "판매 단가(센트 단위 정수)", False),
        ("quantity", "INT", "INT", False, False, "수량", "주문 수량", False),
        ("discount_pct", "INT", "INT", False, False, "할인율", "할인율(% 정수, 0~30)", False),
    ]),
]

NORTHWIND_RELS = [
    ("products", "supplier_id", "suppliers", "supplier_id", "N:1", "제품 공급업체"),
    ("products", "category_id", "categories", "category_id", "N:1", "제품 카테고리"),
    ("orders", "customer_id", "customers", "customer_id", "N:1", "주문 고객사"),
    ("orders", "employee_id", "employees", "employee_id", "N:1", "주문 처리 직원"),
    ("orders", "ship_via", "shippers", "shipper_id", "N:1", "주문 배송업체"),
    ("order_details", "order_id", "orders", "order_id", "N:1", "상세의 주문"),
    ("order_details", "product_id", "products", "product_id", "N:1", "상세의 제품"),
]

WORDPRESS_TABLES = [
    ("wp_users", "사용자", "WordPress 사용자(작성자) 계정.", [
        ("id", "INT", "INT", True, False, "사용자ID", "사용자 고유 번호", False),
        ("user_login", "TEXT", "VARCHAR(64)", False, False, "로그인명", "로그인 이름", False),
        ("user_email", "TEXT", "VARCHAR(256)", False, False, "이메일", "사용자 이메일", True),
        ("display_name", "TEXT", "VARCHAR(128)", False, False, "표시이름", "표시 이름", False),
        ("user_registered", "TIMESTAMPTZ", "DATETIME", False, False, "가입일시", "가입 시각", False),
    ]),
    ("wp_posts", "글", "글/페이지 본문. post_type=post|page, post_status=publish|draft.", [
        ("id", "INT", "INT", True, False, "글ID", "글 고유 번호", False),
        ("post_author", "INT", "INT", False, True, "작성자ID", "작성자(wordpress.wp_users)", False),
        ("post_date", "TIMESTAMPTZ", "DATETIME", False, False, "작성일시", "작성 시각", False),
        ("post_title", "TEXT", "VARCHAR(256)", False, False, "제목", "글 제목", False),
        ("post_status", "TEXT", "VARCHAR(20)", False, False, "상태", "publish(발행) 또는 draft(임시)", False),
        ("post_type", "TEXT", "VARCHAR(20)", False, False, "유형", "post(글) 또는 page(페이지)", False),
        ("comment_count", "INT", "INT", False, False, "댓글수", "댓글 수(비정규화)", False),
    ]),
    ("wp_comments", "댓글", "글에 달린 댓글. comment_approved='1'이면 승인.", [
        ("comment_id", "INT", "INT", True, False, "댓글ID", "댓글 고유 번호", False),
        ("comment_post_id", "INT", "INT", False, True, "글ID", "댓글이 달린 글(wordpress.wp_posts)", False),
        ("comment_author", "TEXT", "VARCHAR(128)", False, False, "작성자명", "댓글 작성자 표시명", False),
        ("comment_date", "TIMESTAMPTZ", "DATETIME", False, False, "작성일시", "댓글 작성 시각", False),
        ("comment_approved", "TEXT", "VARCHAR(4)", False, False, "승인여부", "'1'=승인, '0'=대기", False),
    ]),
    ("wp_terms", "용어", "카테고리/태그 이름(term) 마스터.", [
        ("term_id", "INT", "INT", True, False, "용어ID", "용어 고유 번호", False),
        ("name", "TEXT", "VARCHAR(128)", False, False, "이름", "카테고리/태그 이름", False),
        ("slug", "TEXT", "VARCHAR(128)", False, False, "슬러그", "URL 슬러그", False),
    ]),
    ("wp_term_taxonomy", "용어분류", "term의 분류 체계: taxonomy=category|post_tag.", [
        ("term_taxonomy_id", "INT", "INT", True, False, "분류ID", "분류 고유 번호", False),
        ("term_id", "INT", "INT", False, True, "용어ID", "용어(wordpress.wp_terms)", False),
        ("taxonomy", "TEXT", "VARCHAR(32)", False, False, "분류체계", "category(카테고리) 또는 post_tag(태그)", False),
    ]),
    ("wp_term_relationships", "글-용어", "글과 카테고리/태그 매핑(N:M).", [
        ("object_id", "INT", "INT", True, True, "글ID", "글(wordpress.wp_posts)", False),
        ("term_taxonomy_id", "INT", "INT", True, True, "분류ID", "분류(wordpress.wp_term_taxonomy)", False),
    ]),
    ("wp_postmeta", "글메타", "글의 커스텀 필드 키-값.", [
        ("meta_id", "INT", "INT", True, False, "메타ID", "메타 고유 번호", False),
        ("post_id", "INT", "INT", False, True, "글ID", "대상 글(wordpress.wp_posts)", False),
        ("meta_key", "TEXT", "VARCHAR(128)", False, False, "메타키", "커스텀 필드 키", False),
        ("meta_value", "TEXT", "VARCHAR(512)", False, False, "메타값", "커스텀 필드 값", False),
    ]),
    ("wp_options", "설정", "사이트 전역 설정 키-값.", [
        ("option_id", "INT", "INT", True, False, "설정ID", "설정 고유 번호", False),
        ("option_name", "TEXT", "VARCHAR(128)", False, False, "설정키", "설정 이름", False),
        ("option_value", "TEXT", "VARCHAR(512)", False, False, "설정값", "설정 값", False),
    ]),
]

WORDPRESS_RELS = [
    ("wp_posts", "post_author", "wp_users", "id", "N:1", "글 작성자"),
    ("wp_comments", "comment_post_id", "wp_posts", "id", "N:1", "댓글이 달린 글"),
    ("wp_term_taxonomy", "term_id", "wp_terms", "term_id", "N:1", "분류의 용어"),
    ("wp_term_relationships", "object_id", "wp_posts", "id", "N:1", "매핑된 글"),
    ("wp_term_relationships", "term_taxonomy_id", "wp_term_taxonomy", "term_taxonomy_id", "N:1", "매핑된 분류"),
    ("wp_postmeta", "post_id", "wp_posts", "id", "N:1", "메타의 글"),
]

# =====================================================================
# deterministic seed data
# =====================================================================

def sakila_seed():
    cats = [(i + 1, n) for i, n in enumerate(["Action", "Comedy", "Drama", "Horror", "Sci-Fi", "Documentary"])]
    firsts = ["PENELOPE", "NICK", "ED", "JENNIFER", "JOHNNY", "BETTE", "GRACE", "MATTHEW", "JOE", "CHRISTIAN"]
    lasts = ["GUINESS", "WAHLBERG", "CHASE", "DAVIS", "LOLLOBRIGIDA", "NICHOLSON", "MOSTEL", "JOHANSSON", "SWANK", "GABLE"]
    actors = [(i + 1, firsts[i], lasts[i]) for i in range(10)]
    ratings = ["G", "PG", "PG-13", "R"]
    films = [(i, f"FILM TITLE {i:02d}", 2018 + i % 6, 3 + i % 5, 99 + (i % 3) * 100, 80 + i * 3, ratings[i % 4])
             for i in range(1, 21)]
    film_category = [(i, (i % 6) + 1) for i in range(1, 21)]
    film_actor = sorted({((i % 10) + 1, i) for i in range(1, 21)} | {(((i + 3) % 10) + 1, i) for i in range(1, 21)}
                        | {(5, 2), (5, 6)})  # actor 5 gets a strict top film count (no ties)
    inventory, inv_id = [], 0
    for i in range(1, 21):
        for _ in range(1 + i % 2):
            inv_id += 1
            inventory.append((inv_id, i, 1 + inv_id % 2))
    cfirst = ["MARY", "PATRICIA", "LINDA", "BARBARA", "ELIZABETH", "JENNIFER", "MARIA", "SUSAN"]
    customers = [(i + 1, cfirst[i], f"CUST{i+1:02d}", f"cust{i+1:02d}@sakila.example.com", i != 7,
                  f"2026-01-{(i % 28) + 1:02d} 10:00:00") for i in range(8)]
    rentals, payments = [], []
    for j in range(1, 61):
        cust = (j % 8) + 1
        inv = (j % len(inventory)) + 1
        day, hour = 1 + j // 8, 8 + j % 12
        rd = f"2026-06-{day:02d} {hour:02d}:00:00"
        ret = None if j % 5 == 0 else f"2026-06-{day + 2:02d} {hour:02d}:00:00"
        rentals.append((j, rd, inv, cust, ret))
        payments.append((j, cust, j, 199 + (j % 4) * 100, rd))
    return dict(category=cats, actor=actors, film=films, film_category=film_category,
                film_actor=film_actor, inventory=inventory, customer=customers,
                rental=rentals, payment=payments)

def northwind_seed():
    cats = [(i + 1, n, f"{n} products") for i, n in enumerate(["Beverages", "Condiments", "Confections", "Dairy", "Seafood"])]
    sups = [(i + 1, f"Supplier {chr(65+i)}", c) for i, c in enumerate(["USA", "UK", "Japan", "Germany", "Korea", "France"])]
    products = [(i, f"Product {i:02d}", (i % 6) + 1, (i % 5) + 1, 500 + i * 150, 10 + (i * 7) % 90, i % 5 == 0)
                for i in range(1, 16)]
    countries = ["Korea", "USA", "UK", "Japan", "Germany", "France", "Canada", "Spain"]
    customers = [(f"CUST{i+1:X}".ljust(5, "0")[:5], f"Customer Co {i+1}", countries[i], f"City{i+1}") for i in range(8)]
    emps = [(i + 1, l, f, t, f"2024-0{i+1}-15 09:00:00") for i, (l, f, t) in enumerate([
        ("Davolio", "Nancy", "Sales Representative"), ("Fuller", "Andrew", "Vice President"),
        ("Leverling", "Janet", "Sales Representative"), ("Peacock", "Margaret", "Sales Representative"),
        ("Buchanan", "Steven", "Sales Manager")])]
    shippers = [(1, "Speedy Express"), (2, "United Package"), (3, "Federal Shipping")]
    orders, details = [], []
    for i in range(1, 26):
        cust = customers[i % 8][0]
        day = (i % 28) + 1
        shipped = None if i % 6 == 0 else f"2026-06-{min(day + 3, 28):02d} 00:00:00"
        orders.append((10247 + i, cust, (i % 5) + 1, f"2026-06-{day:02d} 00:00:00", shipped,
                       (i % 3) + 1, 300 + i * 20, customers[i % 8][2]))
        for pid in {(i % 15) + 1, ((i + 7) % 15) + 1}:
            details.append((10247 + i, pid, products[pid - 1][4], (i % 5) + 1, 0 if i % 2 else 10))
    return dict(categories=cats, suppliers=sups, products=products, customers=customers,
                employees=emps, shippers=shippers, orders=orders, order_details=details)

def wordpress_seed():
    users = [(i + 1, u, f"{u}@blog.example.com", d, f"2025-0{i+1}-01 09:00:00") for i, (u, d) in enumerate([
        ("admin", "관리자"), ("alice", "Alice Kim"), ("bob", "Bob Lee"), ("carol", "Carol Park"), ("dave", "Dave Choi")])]
    posts, comments, rels, postmeta = [], [], [], []
    for i in range(1, 21):
        status = "publish" if i % 3 else "draft"
        ptype = "page" if i % 7 == 0 else "post"
        author = 2 if i in (5, 10) else (i % 5) + 1  # author 2 is the strict top (avoids collation-dependent ties)
        posts.append([i, author, f"2026-05-{(i % 28) + 1:02d} 12:00:00", f"Post Title {i:02d}", status, ptype, 0])
        rels.append((i, (i % 6) + 1))
        postmeta.append((i, i, "views", str(100 + i * 13)))
    for j in range(1, 31):
        pid = (j % 20) + 1
        comments.append((j, pid, f"commenter{j % 7}", f"2026-06-{(j % 28) + 1:02d} 15:00:00", "1" if j % 6 else "0"))
    counts = Counter(c[1] for c in comments)
    for p in posts:
        p[6] = counts.get(p[0], 0)
    posts = [tuple(p) for p in posts]
    terms = [(i + 1, n, n.lower()) for i, n in enumerate(["News", "Tech", "Life", "golang", "database", "ai"])]
    tax = [(i + 1, i + 1, "category" if i < 3 else "post_tag") for i in range(6)]
    options = [(1, "blogname", "jamypg blog"), (2, "blogdescription", "text2sql test blog"),
               (3, "posts_per_page", "10"), (4, "timezone_string", "Asia/Seoul"), (5, "template", "twentytwentysix")]
    return dict(wp_users=users, wp_posts=posts, wp_comments=comments, wp_terms=terms,
                wp_term_taxonomy=tax, wp_term_relationships=rels, wp_postmeta=postmeta, wp_options=options)

# =====================================================================
# golden queries with computed expected answers
# =====================================================================

def sakila_golden(s):
    cat_names = dict(s["category"])
    fc_counts = Counter(cat_names[c] for _, c in s["film_category"])
    top_cat = sorted(fc_counts.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    cust_last = {c[0]: c[2] for c in s["customer"]}
    rent_by_cust = Counter(cust_last[r[3]] for r in s["rental"])
    top_cust = sorted(rent_by_cust.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    unreturned = sum(1 for r in s["rental"] if r[4] is None)
    active = sum(1 for c in s["customer"] if c[4])
    actor_last = {a[0]: a[2] for a in s["actor"]}
    fa_counts = Counter(actor_last[a] for a, _ in s["film_actor"])
    top_actor = sorted(fa_counts.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    pay_by_cust = defaultdict(int)
    for p in s["payment"]:
        pay_by_cust[cust_last[p[1]]] += p[3]
    top_pay = sorted(pay_by_cust.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    return [
        {"id": 1, "question": "카테고리별 영화 수를 많은 순으로 보여줘",
         "expected_tables": ["sakila.film_category", "sakila.category"], "expected_columns": ["category_id", "name"],
         "expected_sql": "SELECT T2.name, COUNT(*) AS films FROM sakila.film_category T1 JOIN sakila.category T2 ON T1.category_id = T2.category_id GROUP BY T2.name ORDER BY films DESC, T2.name",
         "expected_first_row": {"name": top_cat[0], "films": top_cat[1]}},
        {"id": 2, "question": "대여 횟수가 가장 많은 고객 상위 3명은?",
         "expected_tables": ["sakila.rental", "sakila.customer"], "expected_columns": ["customer_id", "last_name"],
         "expected_sql": "SELECT T2.last_name, COUNT(*) AS rentals FROM sakila.rental T1 JOIN sakila.customer T2 ON T1.customer_id = T2.customer_id GROUP BY T2.last_name ORDER BY rentals DESC, T2.last_name LIMIT 3",
         "expected_first_row": {"last_name": top_cust[0], "rentals": top_cust[1]}},
        {"id": 3, "question": "활성 고객 수를 알려줘",
         "expected_tables": ["sakila.customer"], "expected_columns": ["active"],
         "expected_sql": "SELECT COUNT(*) AS active_customers FROM sakila.customer T1 WHERE T1.active = TRUE",
         "expected_first_row": {"active_customers": active}},
        {"id": 4, "question": "아직 반납되지 않은 대여 건수는?",
         "expected_tables": ["sakila.rental"], "expected_columns": ["return_date"],
         "expected_sql": "SELECT COUNT(*) AS unreturned FROM sakila.rental T1 WHERE T1.return_date IS NULL",
         "expected_first_row": {"unreturned": unreturned}},
        {"id": 5, "question": "출연 영화가 가장 많은 배우 상위 3명을 알려줘",
         "expected_tables": ["sakila.film_actor", "sakila.actor"], "expected_columns": ["actor_id", "last_name"],
         "expected_sql": "SELECT T2.last_name, COUNT(*) AS films FROM sakila.film_actor T1 JOIN sakila.actor T2 ON T1.actor_id = T2.actor_id GROUP BY T2.last_name ORDER BY films DESC, T2.last_name LIMIT 3",
         "expected_first_row": {"last_name": top_actor[0], "films": top_actor[1]}},
        {"id": 6, "question": "결제 금액 합계가 가장 큰 고객은 누구야?",
         "expected_tables": ["sakila.payment", "sakila.customer"], "expected_columns": ["amount", "customer_id"],
         "expected_sql": "SELECT T2.last_name, SUM(T1.amount) AS total_amount FROM sakila.payment T1 JOIN sakila.customer T2 ON T1.customer_id = T2.customer_id GROUP BY T2.last_name ORDER BY total_amount DESC, T2.last_name LIMIT 1",
         "expected_first_row": {"last_name": top_pay[0], "total_amount": top_pay[1]}},
    ]

def northwind_golden(s):
    live = sum(1 for p in s["products"] if not p[6])
    cat_names = {c[0]: c[1] for c in s["categories"]}
    prod_by_cat = Counter(cat_names[p[3]] for p in s["products"])
    top_cat = sorted(prod_by_cat.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    orders_by_cust = Counter(o[1] for o in s["orders"])
    top_cust = sorted(orders_by_cust.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    unshipped = sum(1 for o in s["orders"] if o[4] is None)
    emp_names = {e[0]: e[1] for e in s["employees"]}
    orders_by_emp = Counter(emp_names[o[2]] for o in s["orders"])
    top_emp = sorted(orders_by_emp.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    total_qty = sum(d[3] for d in s["order_details"])
    return [
        {"id": 1, "question": "단종되지 않은 제품 수를 알려줘",
         "expected_tables": ["northwind.products"], "expected_columns": ["discontinued"],
         "expected_sql": "SELECT COUNT(*) AS live_products FROM northwind.products T1 WHERE T1.discontinued = FALSE",
         "expected_first_row": {"live_products": live}},
        {"id": 2, "question": "카테고리별 제품 수를 많은 순으로 보여줘",
         "expected_tables": ["northwind.products", "northwind.categories"], "expected_columns": ["category_id", "category_name"],
         "expected_sql": "SELECT T2.category_name, COUNT(*) AS products FROM northwind.products T1 JOIN northwind.categories T2 ON T1.category_id = T2.category_id GROUP BY T2.category_name ORDER BY products DESC, T2.category_name",
         "expected_first_row": {"category_name": top_cat[0], "products": top_cat[1]}},
        {"id": 3, "question": "주문이 가장 많은 고객사 상위 3곳은?",
         "expected_tables": ["northwind.orders", "northwind.customers"], "expected_columns": ["customer_id", "company_name"],
         "expected_sql": "SELECT T1.customer_id, COUNT(*) AS orders FROM northwind.orders T1 GROUP BY T1.customer_id ORDER BY orders DESC, T1.customer_id LIMIT 3",
         "expected_first_row": {"customer_id": top_cust[0], "orders": top_cust[1]}},
        {"id": 4, "question": "아직 배송되지 않은 주문 건수는?",
         "expected_tables": ["northwind.orders"], "expected_columns": ["shipped_date"],
         "expected_sql": "SELECT COUNT(*) AS unshipped FROM northwind.orders T1 WHERE T1.shipped_date IS NULL",
         "expected_first_row": {"unshipped": unshipped}},
        {"id": 5, "question": "직원별 처리 주문 수를 많은 순으로 알려줘",
         "expected_tables": ["northwind.orders", "northwind.employees"], "expected_columns": ["employee_id", "last_name"],
         "expected_sql": "SELECT T2.last_name, COUNT(*) AS orders FROM northwind.orders T1 JOIN northwind.employees T2 ON T1.employee_id = T2.employee_id GROUP BY T2.last_name ORDER BY orders DESC, T2.last_name",
         "expected_first_row": {"last_name": top_emp[0], "orders": top_emp[1]}},
        {"id": 6, "question": "전체 주문 상세의 총 주문 수량은?",
         "expected_tables": ["northwind.order_details"], "expected_columns": ["quantity"],
         "expected_sql": "SELECT SUM(T1.quantity) AS total_quantity FROM northwind.order_details T1",
         "expected_first_row": {"total_quantity": total_qty}},
    ]

def wordpress_golden(s):
    published = sum(1 for p in s["wp_posts"] if p[4] == "publish" and p[5] == "post")
    user_names = {u[0]: u[3] for u in s["wp_users"]}
    posts_by_author = Counter(user_names[p[1]] for p in s["wp_posts"] if p[5] == "post")
    top_author = sorted(posts_by_author.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    approved = sum(1 for c in s["wp_comments"] if c[4] == "1")
    tax_terms = {t[0]: t[1] for t in s["wp_term_taxonomy"]}
    term_names = {t[0]: t[1] for t in s["wp_terms"]}
    cat_counts = Counter(term_names[tax_terms[tt]] for _, tt in s["wp_term_relationships"]
                         if next(x[2] for x in s["wp_term_taxonomy"] if x[0] == tt) == "category")
    top_term = sorted(cat_counts.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    com_by_post = Counter(c[1] for c in s["wp_comments"])
    post_titles = {p[0]: p[3] for p in s["wp_posts"]}
    top_post = sorted(com_by_post.items(), key=lambda kv: (-kv[1], kv[0]))[0]
    return [
        {"id": 1, "question": "발행된(publish) 글 수를 알려줘",
         "expected_tables": ["wordpress.wp_posts"], "expected_columns": ["post_status", "post_type"],
         "expected_sql": "SELECT COUNT(*) AS published_posts FROM wordpress.wp_posts T1 WHERE T1.post_status = 'publish' AND T1.post_type = 'post'",
         "expected_first_row": {"published_posts": published}},
        {"id": 2, "question": "작성자별 글 수를 많은 순으로 보여줘",
         "expected_tables": ["wordpress.wp_posts", "wordpress.wp_users"], "expected_columns": ["post_author", "display_name"],
         "expected_sql": "SELECT T2.display_name, COUNT(*) AS posts FROM wordpress.wp_posts T1 JOIN wordpress.wp_users T2 ON T1.post_author = T2.id WHERE T1.post_type = 'post' GROUP BY T2.display_name ORDER BY posts DESC, T2.display_name",
         "expected_first_row": {"display_name": top_author[0], "posts": top_author[1]}},
        {"id": 3, "question": "승인된 댓글 수는?",
         "expected_tables": ["wordpress.wp_comments"], "expected_columns": ["comment_approved"],
         "expected_sql": "SELECT COUNT(*) AS approved_comments FROM wordpress.wp_comments T1 WHERE T1.comment_approved = '1'",
         "expected_first_row": {"approved_comments": approved}},
        {"id": 4, "question": "카테고리별 글 수를 많은 순으로 알려줘",
         "expected_tables": ["wordpress.wp_term_relationships", "wordpress.wp_term_taxonomy", "wordpress.wp_terms"],
         "expected_columns": ["taxonomy", "name"],
         "expected_sql": "SELECT T3.name, COUNT(*) AS posts FROM wordpress.wp_term_relationships T1 JOIN wordpress.wp_term_taxonomy T2 ON T1.term_taxonomy_id = T2.term_taxonomy_id JOIN wordpress.wp_terms T3 ON T2.term_id = T3.term_id WHERE T2.taxonomy = 'category' GROUP BY T3.name ORDER BY posts DESC, T3.name",
         "expected_first_row": {"name": top_term[0], "posts": top_term[1]}},
        {"id": 5, "question": "댓글이 가장 많이 달린 글은 무엇이야?",
         "expected_tables": ["wordpress.wp_comments", "wordpress.wp_posts"], "expected_columns": ["comment_post_id", "post_title"],
         "expected_sql": "SELECT T2.post_title, COUNT(*) AS comments FROM wordpress.wp_comments T1 JOIN wordpress.wp_posts T2 ON T1.comment_post_id = T2.id GROUP BY T2.post_title ORDER BY comments DESC, T2.post_title LIMIT 1",
         "expected_first_row": {"post_title": post_titles[top_post[0]], "comments": top_post[1]}},
    ]

# =====================================================================
# glossaries and example SQL per dataset
# =====================================================================

GLOSSARY = {
    "sakila": [
        {"term": "영화", "category": "entity", "synonyms": ["film", "movie", "작품"]},
        {"term": "고객", "category": "entity", "synonyms": ["customer", "회원", "대여자"]},
        {"term": "대여", "category": "entity", "synonyms": ["rental", "렌탈", "빌린"]},
        {"term": "반납", "category": "entity", "synonyms": ["return", "return_date", "반납일"]},
        {"term": "결제", "category": "metric", "synonyms": ["payment", "지불", "결제금액", "amount"]},
        {"term": "카테고리", "category": "entity", "synonyms": ["category", "장르", "genre"]},
        {"term": "배우", "category": "entity", "synonyms": ["actor", "출연자", "출연"]},
        {"term": "재고", "category": "entity", "synonyms": ["inventory", "보유", "매장 재고"]},
    ],
    "northwind": [
        {"term": "제품", "category": "entity", "synonyms": ["product", "상품", "품목"]},
        {"term": "주문", "category": "entity", "synonyms": ["order", "발주", "주문건"]},
        {"term": "고객사", "category": "entity", "synonyms": ["customer", "고객", "거래처"]},
        {"term": "직원", "category": "entity", "synonyms": ["employee", "담당자", "사원"]},
        {"term": "공급업체", "category": "entity", "synonyms": ["supplier", "공급사", "벤더"]},
        {"term": "배송", "category": "entity", "synonyms": ["ship", "shipper", "shipped_date", "출고"]},
        {"term": "단종", "category": "entity", "synonyms": ["discontinued", "판매중지"]},
        {"term": "카테고리", "category": "entity", "synonyms": ["category", "분류"]},
    ],
    "wordpress": [
        {"term": "글", "category": "entity", "synonyms": ["post", "포스트", "게시글", "게시물"]},
        {"term": "페이지", "category": "entity", "synonyms": ["page"]},
        {"term": "댓글", "category": "entity", "synonyms": ["comment", "코멘트", "리플"]},
        {"term": "작성자", "category": "entity", "synonyms": ["author", "user", "사용자", "글쓴이"]},
        {"term": "발행", "category": "entity", "synonyms": ["publish", "published", "공개"]},
        {"term": "카테고리", "category": "entity", "synonyms": ["category", "분류", "term"]},
        {"term": "태그", "category": "entity", "synonyms": ["tag", "post_tag"]},
        {"term": "승인", "category": "entity", "synonyms": ["approved", "comment_approved"]},
    ],
}

def samples_from_golden(golden, schema):
    out = []
    for g in golden:
        out.append({
            "id": g["id"], "question": g["question"], "target_sql": g["expected_sql"],
            "target_domain": schema, "target_table": g["expected_tables"][0],
            "target_column": "|".join(g["expected_columns"]), "target_intent": "agg_count|group_by",
        })
    return out

# =====================================================================
# emitters
# =====================================================================

def sql_str(v):
    if v is None:
        return "NULL"
    if isinstance(v, bool):
        return "TRUE" if v else "FALSE"
    if isinstance(v, (int, float)):
        return str(v)
    return "'" + str(v).replace("'", "''") + "'"

def emit_schema_sql(schema, tables, seed, engine):
    out = []
    if engine == "pg":
        out.append(f"CREATE SCHEMA IF NOT EXISTS {schema};")
    else:
        out.append(f"CREATE DATABASE IF NOT EXISTS {schema};")
    for table, _, _, cols in tables:
        pks = [c[0] for c in cols if c[3]]
        lines = []
        for c in cols:
            typ = c[1] if engine == "pg" else c[2]
            if typ.endswith(" NULL"):
                lines.append(f"\t{c[0]} {typ.replace(' NULL', '') if engine == 'pg' else typ}")
            else:
                lines.append(f"\t{c[0]} {typ} NOT NULL" if engine != "pg" else f"\t{c[0]} {typ}")
        lines.append(f"\tPRIMARY KEY ({', '.join(pks)})")
        out.append(f"CREATE TABLE IF NOT EXISTS {schema}.{table} (\n" + ",\n".join(lines) + "\n);")
    for table, _, _, cols in tables:
        names = [c[0] for c in cols]
        for row in seed[table]:
            out.append(f"INSERT INTO {schema}.{table} ({', '.join(names)}) VALUES ({', '.join(sql_str(v) for v in row)});")
    if engine == "pg":
        out.append(f"GRANT USAGE ON SCHEMA {schema} TO jamypg_ro;")
        out.append(f"GRANT SELECT ON ALL TABLES IN SCHEMA {schema} TO jamypg_ro;")
    else:
        out.append(f"GRANT SELECT ON {schema}.* TO 'jamypg_ro'@'%';")
    return "\n".join(out)

def emit_dataset(schema, ko_title, tables, rels, seed, golden):
    d = os.path.join(DATA, schema)
    os.makedirs(d, exist_ok=True)
    phys, logi, relrows = [], [], []
    for table, ko, tdesc, cols in tables:
        for i, c in enumerate(cols, 1):
            phys.append({
                "id": str(uuid.uuid5(uuid.NAMESPACE_DNS, f"{schema}.{table}.{c[0]}")),
                "schema_name": schema, "table_name": table, "column_order": str(i),
                "column_name": c[0], "data_type": c[1].replace(" NULL", ""), "length_precision": "",
                "null_constraint": "", "is_pk": "Y" if c[3] else "N", "is_fk": "Y" if c[4] else "N",
                "description": tdesc, "version": 1,
            })
            logi.append({
                "schema_name": schema, "entity_name_en": table, "entity_name_ko": ko,
                "entity_order": str(i), "attribute_name_ko": c[5], "attribute_name_en": c[0],
                "data_type": c[1].replace(" NULL", ""), "length_precision": "",
                "is_pk": "Y" if c[3] else "N", "is_fk": "Y" if c[4] else "N",
                "description": c[6], "note": "", "version": 1,
            })
    for i, (bt, bc, rt, rc, card, desc) in enumerate(rels, 1):
        relrows.append({
            "id": i, "base_schema": schema, "base_table": bt, "base_column": bc,
            "reference_schema": schema, "reference_table": rt, "reference_column": rc,
            "cardinality": card, "join_type": "INNER", "provision_type": "FK",
            "description": desc, "meta_version": 1,
        })
    overrides = {
        "dialect": "postgres",
        "pii_columns": [f"{schema}.{t}.{c[0]}" for t, _, _, cols in tables for c in cols if c[7]],
        "tables": [{"table": f"{schema}.{t}", "domain": ko_title, "row_count": len(seed[t])} for t, _, _, _ in tables],
    }
    routing = {"schemas": [schema], "tags": [f"oss:{schema}"], "priority": 10}
    profiles = [
        {"id": f"pg-{schema}", "name": f"PostgreSQL {schema}", "type": "postgres",
         "connect_string": "127.0.0.1:55432/jamypg_meta", "username": "jamypg_ro", "password_ref": "plain:jamypg_ro_pw",
         "routing": routing},
        {"id": f"mysql-{schema}", "name": f"MySQL {schema}", "type": "mysql",
         "connect_string": f"127.0.0.1:53306/{schema}", "username": "jamypg_ro", "password_ref": "plain:jamypg_ro_pw",
         "routing": routing},
        {"id": f"mariadb-{schema}", "name": f"MariaDB {schema}", "type": "mariadb",
         "connect_string": f"127.0.0.1:53307/{schema}", "username": "jamypg_ro", "password_ref": "plain:jamypg_ro_pw",
         "routing": routing},
    ]
    def w(name, obj):
        with open(os.path.join(d, name), "w", encoding="utf-8") as f:
            json.dump(obj, f, ensure_ascii=False, indent=1)
            f.write("\n")
    w("meta_physical_models.json", phys)
    w("meta_logical_models.json", logi)
    w("topology_relations.json", relrows)
    w("overrides.json", overrides)
    w("glossary.json", {"entries": GLOSSARY[schema]})
    w("sql_datasets.json", samples_from_golden(golden, schema))
    w("golden_queries.json", golden)
    w("databases.json", [{"id": 1, "dbms": "POSTGRES", "port": 55432, "name": "jamypg_meta", "alias": schema}])
    w("db_profiles.json", profiles)

if __name__ == "__main__":
    datasets = [
        ("sakila", "DVD렌탈", SAKILA_TABLES, SAKILA_RELS, sakila_seed(), sakila_golden),
        ("northwind", "주문관리", NORTHWIND_TABLES, NORTHWIND_RELS, northwind_seed(), northwind_golden),
        ("wordpress", "블로그", WORDPRESS_TABLES, WORDPRESS_RELS, wordpress_seed(), wordpress_golden),
    ]
    pg_parts, my_parts = ["-- Famous OSS schemas as text2sql targets. Auto-generated by gen_oss_testenv.py."], \
                         ["-- Famous OSS schemas as text2sql targets. Auto-generated by gen_oss_testenv.py."]
    for schema, ko, tables, rels, seed, goldfn in datasets:
        golden = goldfn(seed)
        pg_parts.append(emit_schema_sql(schema, tables, seed, "pg"))
        my_parts.append(emit_schema_sql(schema, tables, seed, "my"))
        emit_dataset(schema, ko, tables, rels, seed, golden)
        rows = sum(len(v) for v in seed.values())
        print(f"{schema}: {len(tables)} tables, {rows} rows, {len(golden)} golden queries")
    with open(os.path.join(INIT, "postgres", "02-oss.sql"), "w", encoding="utf-8") as f:
        f.write("\n\n".join(pg_parts) + "\n")
    my = "\n\n".join(my_parts) + "\nFLUSH PRIVILEGES;\n"
    for sub in ("mysql", "mariadb"):
        with open(os.path.join(INIT, sub, "02-oss.sql"), "w", encoding="utf-8") as f:
            f.write(my)
    print("generated: deploy/test/init/*/02-oss.sql and data/{sakila,northwind,wordpress}/")
