//go:build oracle

package dbconn

// The Oracle edition is intentionally opt-in: godror uses ODPI-C/OCI and
// requires CGO plus Oracle Client libraries at runtime. The blank import only
// registers database/sql driver "godror"; all product code remains behind the
// normal Dialect/Connector boundaries.
import _ "github.com/godror/godror"
