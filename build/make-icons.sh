#!/usr/bin/env bash
# Rebuilds every platform icon from the official Kolo logo (build/kolo-logo.svg).
# Needs rsvg-convert, ImageMagick (magick) and, on macOS, iconutil.
set -euo pipefail
cd "$(dirname "$0")"
BG='#070b12'
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
# The logo switches colors with prefers-color-scheme; a tile is always dark, so force the dark colors.
echo '.r{stroke:#eef3fb !important}.d{fill:#eef3fb !important}.a{fill:#6ab5fb !important}' > "$W/dark.css"
# Heavier variants of the same mark for small sizes: thicker ring and larger dots, viewBox widened so nothing clips.
for v in 1:2.4:3.0 2:3.2:3.6; do
  IFS=: read n sw r <<<"$v"
  sed -e "s/viewBox=\"0 0 26 26\"/viewBox=\"-1.5 -1.5 29 29\"/" -e "s/stroke-width=\"1.4\"/stroke-width=\"$sw\"/" -e "s/r=\"2.4\"/r=\"$r\"/g" kolo-logo.svg > "$W/logo-h$n.svg"
done
cp kolo-logo.svg "$W/logo-h0.svg"

# mark <px> <heaviness 0|1|2> <out>
mark() { rsvg-convert -w "$1" -h "$1" -s "$W/dark.css" "$W/logo-h$2.svg" -o "$3"; }
# tile <size> <radius> <mark px> <heaviness> <out>: rounded dark square with the mark centered
tile() {
  magick -size "$1x$1" xc:none -fill "$BG" -draw "roundrectangle 0,0 $(($1-1)),$(($1-1)) $2,$2" "$W/t.png"
  mark "$3" "$4" "$W/m.png"
  magick "$W/t.png" "$W/m.png" -gravity center -composite -depth 8 "PNG32:$5"
}

# macOS: 1024 canvas, 824 body (100 px margin), radius 185, mark 60% of the body, soft shadow below
magick -size 1024x1024 xc:none -fill "$BG" -draw "roundrectangle 100,100 923,923 185,185" "$W/body.png"
magick "$W/body.png" \( +clone -background black -shadow 30x14+0+12 \) +swap -background none -layers merge +repage -gravity center -extent 1024x1024 "$W/body-shadow.png"
mark 494 0 "$W/m.png"
magick "$W/body-shadow.png" "$W/m.png" -gravity center -composite -depth 8 PNG32:appicon.png
if command -v iconutil >/dev/null; then
  S="$W/icons.iconset"; mkdir "$S"
  for p in 16:icon_16x16 32:icon_16x16@2x 32:icon_32x32 64:icon_32x32@2x 128:icon_128x128 256:icon_128x128@2x 256:icon_256x256 512:icon_256x256@2x 512:icon_512x512 1024:icon_512x512@2x; do
    magick appicon.png -filter Lanczos -resize "${p%%:*}x${p%%:*}" "$S/${p##*:}.png"
  done
  iconutil -c icns "$S" -o darwin/icons.icns
  cp darwin/icons.icns darwin/dmg-file-icon.icns; cp appicon.png darwin/dmg-file-icon.png
fi

# Windows: rounded tile (radius 20%), small padding, hand-tuned 16/24/32, one .ico with 7 sizes
mkdir -p "$W/win"
tile 16 3 12 2 "$W/win/16.png"
tile 24 5 18 1 "$W/win/24.png"
tile 32 6 25 1 "$W/win/32.png"
tile 48 10 38 0 "$W/win/48.png"
tile 64 13 50 0 "$W/win/64.png"
tile 128 26 100 0 "$W/win/128.png"
tile 256 51 200 0 "$W/win/256.png"
magick "$W"/win/16.png "$W"/win/24.png "$W"/win/32.png "$W"/win/48.png "$W"/win/64.png "$W"/win/128.png "$W"/win/256.png windows/icon.ico

# Linux: freedesktop hicolor sizes, rounded body like macOS with a 4% margin, plus a scalable SVG
for s in 16 22 24 32 48 64 128 256 512; do
  d="linux/icons/hicolor/${s}x${s}/apps"; mkdir -p "$d"
  m=$((s*4/100)); [ $m -lt 1 ] && m=1
  body=$((s-2*m)); rad=$((body*225/1000)); mk=$((body*60/100)); h=0; [ $s -le 24 ] && h=2; [ $s -gt 24 ] && [ $s -le 32 ] && h=1
  magick -size "${s}x${s}" xc:none -fill "$BG" -draw "roundrectangle $m,$m $((s-m-1)),$((s-m-1)) $rad,$rad" "$W/t.png"
  mark "$mk" "$h" "$W/m.png"
  magick "$W/t.png" "$W/m.png" -gravity center -composite -depth 8 "PNG32:$d/kolo-chat.png"
done
mkdir -p linux/icons/hicolor/scalable/apps
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024" width="1024" height="1024">'
  echo "<rect x=\"41\" y=\"41\" width=\"942\" height=\"942\" rx=\"212\" fill=\"$BG\"/>"
  echo '<g transform="translate(257 257) scale(20.77)" fill="none">'
  echo '<circle cx="13" cy="13" r="10" stroke="#eef3fb" stroke-width="1.4"/>'
  echo '<circle cx="13" cy="3" r="2.4" fill="#6ab5fb"/><circle cx="22.7" cy="15.8" r="2.4" fill="#eef3fb"/><circle cx="7" cy="21.2" r="2.4" fill="#eef3fb"/>'
  echo '</g></svg>'
} > linux/icons/hicolor/scalable/apps/kolo-chat.svg
