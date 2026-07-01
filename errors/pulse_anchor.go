package errors

// Aperture depends on the published github.com/frankbardon/pulse module for its
// expression evaluator (consumed by the rules package in E2-S3). Pulse's own
// *CodedError values pass through Aperture's error surfaces verbatim — Aperture
// never re-stamps an upstream pulse code.
//
// This blank import anchors pulse as a direct dependency in go.mod now, so the
// scaffold pins the module (no `replace` directive — it is consumed from the Go
// proxy) before any story wires real usage. pulse/errors is pure-Go; importing
// it keeps Aperture CGO-free (it does not pull pulse's geo/h3 packages).
import _ "github.com/frankbardon/pulse/errors"
