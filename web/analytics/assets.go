//go:build !analyticsui

// Package analyticsweb contains the optional compiled dashboard.
package analyticsweb

import (
	"errors"
	"io/fs"
)

func Assets() (fs.FS, error) {
	return nil, errors.New("analytics UI is not built; run ./install.sh, or npm --prefix web/analytics ci && npm --prefix web/analytics run build && go build -tags analyticsui ./cmd/q")
}
