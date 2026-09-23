#!/usr/bin/env bash
# PreToolUse：拦截对生成代码、已提交迁移、密钥文件的修改。退出码 2 表示拒绝，stderr 反馈给 Claude。
set -uo pipefail
f="$(jq -r '.tool_input.file_path // .tool_input.path // empty')"
[ -z "$f" ] && exit 0
case "$f" in
  */gen/*|*.pb.go|*_gen.go|*.gen.ts|*/sqlc/*.go)
    echo "拒绝：$f 是生成代码。请修改源（proto / OpenAPI / SQL）后运行 make gen。" >&2; exit 2 ;;
  *.env|*.env.*|*/secrets/*)
    echo "拒绝：不允许读写密钥文件 $f。" >&2; exit 2 ;;
  */migrations/*.sql)
    if [ -f "$f" ] && [ "${ALLOW_MIGRATION_EDIT:-0}" != "1" ]; then
      dir="$(dirname "$f")"
      if git -C "$dir" ls-files --error-unmatch "$f" >/dev/null 2>&1; then
        echo "拒绝：$f 已提交，迁移只能新增不能修改。请新建一个迁移文件。" >&2; exit 2
      fi
    fi ;;
esac
exit 0
