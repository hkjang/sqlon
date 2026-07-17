package dbconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// connectionDiagnostic turns low-level driver/network failures into stable,
// actionable connection-test feedback. It deliberately does not expose the
// resolved password or full DSN.
func connectionDiagnostic(err error, p Profile) (string, string, []string) {
	msg := strings.ToLower(err.Error())
	steps := []string{"컨테이너에서 DB 호스트와 포트에 접근 가능한지 확인하세요.", "프로파일의 DB 엔진, 접속 주소, 계정과 비밀번호 참조를 확인하세요."}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr):
		return "dns", "DB 호스트 이름을 찾지 못했습니다. Docker에서는 localhost가 컨테이너 자신을 가리킵니다.", []string{"같은 Compose 네트워크라면 서비스명을 호스트로 사용하세요.", "호스트 DB라면 host.docker.internal 또는 접근 가능한 호스트 IP를 사용하세요."}
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "i/o timeout"):
		return "timeout", "제한 시간 안에 DB가 응답하지 않았습니다.", []string{"방화벽·보안 그룹·포트 공개 여부를 확인하세요.", "DB bind-address와 Docker 네트워크 경로를 확인하세요."}
	case strings.Contains(msg, "connection refused") || (errors.As(err, &opErr) && strings.Contains(msg, "connect:")):
		return "network", "호스트에는 도달했지만 해당 포트에서 DB가 연결을 받지 않습니다.", []string{"DB 프로세스와 포트 매핑을 확인하세요.", "MySQL/MariaDB의 bind-address가 외부 연결을 허용하는지 확인하세요."}
	case strings.Contains(msg, "access denied") || strings.Contains(msg, "password authentication failed"):
		return "authentication", "DB가 계정 또는 비밀번호를 거부했습니다.", []string{"password_ref의 env/file 값이 컨테이너 내부에도 존재하는지 확인하세요.", "MySQL/MariaDB 계정의 user@host 허용 범위를 확인하세요."}
	case strings.Contains(msg, "unknown database") || strings.Contains(msg, "does not exist"):
		return "database", "지정한 데이터베이스가 존재하지 않거나 계정에 접근 권한이 없습니다.", []string{"connect_string 끝의 데이터베이스명을 확인하세요.", "해당 DB에 대한 CONNECT/USAGE 권한을 확인하세요."}
	case strings.Contains(msg, "tls") || strings.Contains(msg, "certificate") || strings.Contains(msg, "ssl"):
		return "tls", "TLS/인증서 설정이 서버 요구사항과 맞지 않습니다.", []string{"서버의 TLS 요구 여부와 CA 인증서를 확인하세요.", "필요한 tls 파라미터를 접속 문자열에 지정하세요."}
	case strings.Contains(msg, "unknown system variable"):
		return "compatibility", "DB 버전이 요청된 세션 변수를 지원하지 않습니다. MySQL과 MariaDB 엔진 선택이 맞는지 확인하세요.", steps
	case strings.Contains(msg, "password_ref") || strings.Contains(msg, "environment variable") || strings.Contains(msg, "no such file"):
		return "secret", "비밀번호 참조를 컨테이너에서 해석하지 못했습니다.", []string{"env: 변수는 docker run -e 또는 Compose environment로 전달하세요.", "file: 경로는 컨테이너 내부 경로로 마운트하세요."}
	default:
		return "driver", fmt.Sprintf("%s 연결 초기화에 실패했습니다. 상세 오류와 설정을 함께 확인하세요.", p.Type), steps
	}
}

// dbErrCode classifies an execution error into a stable, greppable code:
// context states, PG-<SQLSTATE> for PostgreSQL, MY-<errno> for MySQL/MariaDB.
func dbErrCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TIMEOUT"
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELED"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "PG-" + pgErr.Code
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return fmt.Sprintf("MY-%d", myErr.Number)
	}
	return "INTERNAL"
}

// sanitizeDBError trims driver noise and newlines from error messages so they
// are safe to surface through the API without leaking connection details.
func sanitizeDBError(err error) string {
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return msg
}
