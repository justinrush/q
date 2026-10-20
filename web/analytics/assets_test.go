//go:build analyticsui

package analyticsweb

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedBuild(t *testing.T) {
	assets, err := Assets()
	if err != nil {
		t.Fatal(err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "/assets/") {
		t.Fatal("index is not a production build")
	}
	entries, err := fs.ReadDir(assets, "assets")
	if err != nil || len(entries) < 2 {
		t.Fatalf("missing compiled assets: %v", err)
	}
}
