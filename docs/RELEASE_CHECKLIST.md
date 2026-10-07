# 릴리즈 체크리스트

오프라인망 사이트가 실제로 받는 것은 git 태그가 아니라 **`docker load` 하는 이미지 파일**입니다.
그래서 저장한 이미지 파일을 다시 불러와 실제로 동작하는 것을 확인한 뒤에 릴리즈를 공개합니다.

## 산출물 이름

| 무엇 | 형식 | 예 |
| --- | --- | --- |
| Docker 이미지 | `sqlon:vX.Y.Z` | `sqlon:v0.6.1` |
| 이미지 파일 | `sqlon-vX.Y.Z.tar.gz` + `.sha256` | `sqlon-v0.6.1.tar.gz` |

이미지는 `Dockerfile.oracle` 로 빌드합니다. Oracle Instant Client 와 godror 가 들어 있고
PostgreSQL·MySQL·MariaDB 드라이버도 함께 있어 이미지 하나로 모든 엔진을 다룹니다.
`Dockerfile` 은 Oracle 이 없는 경량 로컬 빌드용이며 릴리즈하지 않습니다.

## 1. 코드

```sh
go test ./...
```

## 2. 버전

`internal/mcp/server.go` 의 `Version`, README 의 `sqlon:vX.Y.Z`, CHANGELOG 를 함께 올리고
`release: vX.Y.Z` 커밋 → `git tag -a vX.Y.Z` → main·태그 push.
깨끗한 트리에서 빌드해야 이미지가 태그 커밋을 그대로 담습니다.

## 3. 빌드·패키징·이미지 검증

```sh
SQLON_VERIFY_ORACLE='<docker-network> <host:port/service> <user> <password>' \
  sh scripts/release.sh vX.Y.Z
```

`release.sh` 는 바이너리·패키지를 만들고, 이미지를 빌드해 `sqlon-vX.Y.Z.tar.gz` 로 저장한 뒤
`scripts/verify-image.sh` 로 **그 파일을** 불러와 검사합니다. 하나라도 실패하면 멈춥니다.

Oracle 검증 대상은 같은 docker 네트워크의 Oracle 입니다. 예:

```sh
docker network create sqlon-relcheck
docker run -d --name sqlon-relcheck-ora --network sqlon-relcheck \
  -e ORACLE_PASSWORD=... -e APP_USER=sqlon_monitor -e APP_USER_PASSWORD=... gvenzl/oracle-free
# SQLON_VERIFY_ORACLE='sqlon-relcheck sqlon-relcheck-ora:1521/FREEPDB1 sqlon_monitor <APP_USER_PASSWORD>'
```

| 확인 | 왜 |
| --- | --- |
| `.sha256` 이 파일과 맞는지 | 반입 후 사이트가 하는 첫 확인입니다 |
| `docker load -i` 가 정확히 `sqlon:vX.Y.Z` 로 불러오는지 | 로컬 빌드 캐시가 아니라 파일 안의 이미지를 검사해야 합니다. 이름이 다르면 안내한 `docker run` 이 동작하지 않습니다 |
| 데이터 볼륨을 붙여 HEALTHCHECK `healthy` | 볼륨 없이 띄우면 통과하고 실제 사용에서 깨지는 문제가 있습니다 |
| MCP `serverInfo.version` 이 태그와 같은지 | 버전은 빌드 인자로 주입됩니다 |
| 서버 프로세스가 비root(uid 10001)인지 | 권한 축소 유지 |
| 드라이버에 `oracle` 이 있는지 | 이 이미지의 존재 이유입니다 |
| **실제 Oracle 연결** (`/api/db-profiles/{id}/test` → `ok`) | Instant Client 와 godror 가 실제로 붙는지는 연결해 봐야 압니다 |
| 틀린 비밀번호가 `authentication` / `ORA-01017` 로 안내되는지 | v0.6.0 까지 godror 오류의 `connectionClassLength` 에 걸려 TLS 문제로 안내됐습니다 |
| 볼륨에 쓰기 → 새 컨테이너에서 그대로 보이는지 | 영속성의 목적 |
| `docker stop` 이 10초 안에 정상 종료(137 아님) | SIGTERM 에 시계열을 flush 합니다. 강제 종료되면 최근 데이터가 사라집니다 |
| **빈 호스트 디렉터리**를 바인드 마운트해도 뜨는지 | 관리자 가이드의 방식입니다. v0.6.0 까지 `load SQLON catalog` 으로 바로 종료했습니다 |
| 쓸 수 없는 호스트 디렉터리면 `chown` 안내와 함께 종료하는지 | 원인을 모르는 기동 실패보다 낫습니다 |

## 4. 공개

```sh
gh release create vX.Y.Z --title "SQLON vX.Y.Z Release" --notes-file notes.md \
  dist/vX.Y.Z/pkg/sqlon-vX.Y.Z.tar.gz dist/vX.Y.Z/pkg/sqlon-vX.Y.Z.tar.gz.sha256 \
  dist/vX.Y.Z/pkg/sqlon-vX.Y.Z-linux-amd64.tar.gz dist/vX.Y.Z/pkg/sqlon-vX.Y.Z-linux-arm64.tar.gz \
  dist/vX.Y.Z/pkg/sqlon-vX.Y.Z-windows-amd64.zip dist/vX.Y.Z/pkg/sqlon-disk-report.sh \
  dist/vX.Y.Z/pkg/SHA256SUMS.txt
```

업로드 후 원격 에셋 크기가 로컬과 같은지 확인합니다.

릴리즈 노트: 요약 → `## Changes` → `### Fixes` → `## Assets` 표 → `## Docker image`(`docker load` 와
`docker run` 안내, 이미지 파일을 맨 앞에).

릴리즈가 결함을 안고 나가면 그 결함을 잡는 확인을 이 표와 `verify-image.sh` 에 추가합니다.
