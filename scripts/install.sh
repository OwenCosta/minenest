#!/bin/sh
# Installs MineNest on Linux:
#   curl -fsSL https://github.com/OwenCosta/minenest/releases/latest/download/install.sh | sh
# As a normal user it goes in ~/.local/bin; as root, in /usr/local/bin. On a desktop it adds
# MineNest to the applications menu. The download is checked against the release's SHA256SUMS.
set -eu

repo=OwenCosta/minenest
base="${MINENEST_RELEASE_URL:-https://github.com/$repo/releases/latest/download}"
case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "MineNest doesn't have a build for $(uname -m) yet." >&2; exit 1 ;;
esac
if [ "$(id -u)" = 0 ]; then bindir=/usr/local/bin; appdir=/usr/local/share/applications; icondir=/usr/local/share/icons/hicolor/scalable/apps
else bindir="$HOME/.local/bin"; appdir="${XDG_DATA_HOME:-$HOME/.local/share}/applications"; icondir="${XDG_DATA_HOME:-$HOME/.local/share}/icons/hicolor/scalable/apps"; fi

fetch() { if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"; else wget -qO "$2" "$1"; fi; }

echo "Installing MineNest..."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
asset="minenest-linux-$arch"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"
fetch "$base/$asset" "$tmp/$asset"
fetch "$base/minenest.svg" "$tmp/minenest.svg" || true
want=$(awk -v a="$asset" '$2 == a || $2 == "*"a {print $1}' "$tmp/SHA256SUMS")
got=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
if [ -z "$want" ] || [ "$want" != "$got" ]; then
    echo "The download didn't match its checksum, so it wasn't installed. Try again." >&2
    exit 1
fi
if [ -x "$bindir/minenest" ]; then "$bindir/minenest" quit >/dev/null 2>&1 || true; sleep 3; fi
mkdir -p "$bindir"
install -m 755 "$tmp/$asset" "$bindir/minenest"

if [ -s "$tmp/minenest.svg" ]; then
    mkdir -p "$appdir" "$icondir"
    cp "$tmp/minenest.svg" "$icondir/minenest.svg"
    cat > "$appdir/minenest.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=MineNest
Comment=Easy Minecraft servers
Exec=$bindir/minenest gui
Icon=minenest
Terminal=false
Categories=Game;Network;
EOF
fi

echo "MineNest is installed: $bindir/minenest"
case ":$PATH:" in *":$bindir:"*) ;; *) echo "Add $bindir to your PATH to type 'minenest' anywhere." ;; esac
echo "Open MineNest from your apps, or run: minenest help"
echo "To keep it running when you're signed out: minenest autostart on, then: loginctl enable-linger \$USER"
