#!/usr/bin/env bash
# 仓库自检：提交前跑一遍，避免低级的脚本错误进到部署流程里。
#
#   ./hack/lint.sh
#
# 检查项：
#   1. 所有 *.sh 语法（bash -n）
#   2. 「变量紧跟中文/全角标点」——在 set -u 下会变成 unbound variable，
#      已经踩了五次：`${CODE}，`、`${KREL}（`、`${np}，`、`${IMAGES}）`…
#   3. Go 代码格式（gofmt -l），Go 可用时才查
set -uo pipefail
cd "$(dirname "$0")/.."

FAIL=0
ok()  { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL + 1)); }
say() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

say "1. Shell 语法"
while IFS= read -r f; do
  if bash -n "$f" 2>/dev/null; then
    ok "$f"
  else
    bad "${f}（bash -n 失败）"
  fi
done < <(find . -name '*.sh' -not -path './.git/*' | sort)

say "2. 变量紧跟非 ASCII 字符"
# 这类写法在 set -u 下会报 "XXX: unbound variable"，而且位置很隐蔽：
#   bad "返回 ${CODE}，期望 302"      ← 全角逗号被当成变量名的一部分
python3 - <<'PY' || FAIL=$((FAIL + 1))
import glob, re, sys
pat = re.compile(r'\$([A-Za-z_][A-Za-z0-9_]*)(?=[^\x00-\x7f])')
hits = 0
for f in sorted(glob.glob('**/*.sh', recursive=True)):
    s = open(f, encoding='utf-8').read()
    for m in pat.finditer(s):
        line = s.count('\n', 0, m.start()) + 1
        print(f"  \033[31mFAIL\033[0m  {f}:{line}  ${m.group(1)} 后面紧跟非 ASCII 字符")
        hits += 1
print(f"  \033[32mOK\033[0m    检查了 {len(glob.glob('**/*.sh', recursive=True))} 个脚本" if hits == 0 else "")
sys.exit(1 if hits else 0)
PY

say "3. Go 格式"
if command -v go >/dev/null 2>&1; then
  for d in control router; do
    out=$(cd "$d" && gofmt -l . 2>/dev/null)
    if [ -z "$out" ]; then ok "$d"; else bad "$d: $out"; fi
  done
else
  printf '  \033[33mSKIP\033[0m  本机没有 go\n'
fi

say "结论"
[ "$FAIL" -eq 0 ] && { printf '  \033[32m全部通过\033[0m\n'; exit 0; }
printf '  \033[31m%d 项失败\033[0m\n' "$FAIL"
exit 1
