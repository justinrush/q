#!/bin/sh
# Build q and install it somewhere on your PATH.
#
#   ./install.sh                     # install to ~/.local/bin
#   ./install.sh --prefix /usr/local # install to /usr/local/bin
#   ./install.sh --uninstall         # remove the installed binary
#
# The binary is stamped with the version and commit it was built from, so
# `q --version` and `q doctor` can tell you exactly what is running. That matters
# more than usual here: a running daemon keeps serving with the binary it started
# with, so after reinstalling you also want `q daemon restart`.

set -eu

BINARY=q
MODULE=github.com/justinrush/q
PACKAGE=./cmd/q
VERSION_VAR="$MODULE/internal/buildinfo"

PREFIX="${PREFIX:-$HOME/.local}"
UNINSTALL=0

usage() {
	sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
	case "$1" in
	--prefix)
		[ $# -ge 2 ] || { echo "install.sh: --prefix needs a directory" >&2; exit 2; }
		PREFIX="$2"
		shift 2
		;;
	--prefix=*)
		PREFIX="${1#--prefix=}"
		shift
		;;
	--uninstall)
		UNINSTALL=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "install.sh: unknown option $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

BINDIR="$PREFIX/bin"
TARGET="$BINDIR/$BINARY"

if [ "$UNINSTALL" -eq 1 ]; then
	if [ -e "$TARGET" ]; then
		rm -f "$TARGET"
		echo "removed $TARGET"
	else
		echo "nothing to remove at $TARGET"
	fi

	exit 0
fi

# cd to the repository root so the script works from anywhere.
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

if ! command -v go >/dev/null 2>&1; then
	echo "install.sh: go is required to build q; see https://go.dev/dl/" >&2
	exit 1
fi

# The version is the closest tag when the checkout has one, and the short commit
# otherwise, so an install from a branch is still identifiable.
if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
	COMMIT="$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
	VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
else
	COMMIT=unknown
	VERSION=dev
fi

echo "building $BINARY $VERSION"

mkdir -p bin
go build \
	-trimpath \
	-ldflags "-s -w -X $VERSION_VAR.Version=$VERSION -X $VERSION_VAR.Commit=$COMMIT" \
	-o "bin/$BINARY" \
	"$PACKAGE"

mkdir -p "$BINDIR"
# Install through a temporary name and rename into place: replacing a running
# binary in one step keeps any daemon that is mid-exec from reading a half-written
# file.
cp "bin/$BINARY" "$TARGET.new"
chmod 755 "$TARGET.new"
mv -f "$TARGET.new" "$TARGET"

echo "installed $TARGET"

case ":$PATH:" in
*":$BINDIR:"*) ;;
*)
	echo
	echo "note: $BINDIR is not on your PATH. Add this to your shell profile:"
	echo "  export PATH=\"$BINDIR:\$PATH\""
	;;
esac

# A daemon started from the old binary keeps serving until it is told otherwise.
if [ -n "${Q_SKIP_RESTART_HINT:-}" ]; then
	exit 0
fi

echo
echo "next:"
echo "  q doctor          # check git, tmux, an agent, and your editor are reachable"
echo "  q config init     # write ~/.q-config.json with the current effective values"
echo "  q daemon restart  # if a daemon from an older build is running"
echo "  q                 # open the board"
