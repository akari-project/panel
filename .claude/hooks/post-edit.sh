#!/usr/bin/env bash
# PostToolUse：编辑后格式化。读取 stdin 的 JSON，取 tool_input.file_path。
set -uo pipefail
f="$(jq -r '.tool_input.file_path // .tool_input.path // empty')"
[ -z "$f" ] || [ ! -f "$f" ] && exit 0
case "$f" in
  *.go)
    if command -v goimports >/dev/null; then goimports -w "$f"; else gofmt -w "$f"; fi ;;
  *.proto)
    root="$(cd "$(dirname "$f")" && git rev-parse --show-toplevel 2>/dev/null)"
    [ -n "$root" ] && (cd "$root" && buf format -w "$f" >/dev/null 2>&1 && buf lint 1>&2) || true ;;
  *.ts|*.tsx|*.css|*.json)
    root="$(cd "$(dirname "$f")" && git rev-parse --show-toplevel 2>/dev/null)"
    [ -n "$root" ] && [ -f "$root/web/node_modules/.bin/prettier" ] && "$root/web/node_modules/.bin/prettier" --write "$f" >/dev/null || true ;;
esac
exit 0
