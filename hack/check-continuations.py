#!/usr/bin/env python3
"""检查 shell 续行把下一条命令吞进上一条的情况。

典型症状：kubectl set env 的参数列表末尾多一个反斜杠，下一行的 kubectl 被当成参数，
运行时报 `unknown flag: --timeout`；而 `bash -n` 检查不出来（语法完全合法）。
"""
import glob
import sys


def logical_lines(text):
    """把以反斜杠结尾的行拼成逻辑行，yield (起始行号, 拼接结果)。"""
    buf = ""
    start = 1
    for i, line in enumerate(text.split("\n"), 1):
        if not buf:
            start = i
        cur = line.rstrip()
        if cur.endswith("\\"):
            buf += cur[:-1] + " "
            continue
        yield start, buf + cur
        buf = ""
    if buf:
        yield start, buf


def main():
    bad = 0
    for path in sorted(glob.glob("**/*.sh", recursive=True)):
        with open(path, encoding="utf-8") as fh:
            for line, parts in logical_lines(fh.read()):
                if parts.lstrip().startswith("#"):
                    continue          # 注释里出现同样的词不算
                # set env 的参数列表漏掉反斜杠时，下一条 kubectl 会被吞进来
                if "set env" in parts and parts.count("kubectl ") > 1:
                    print(f"  \033[31mFAIL\033[0m  {path}:{line}  续行把下一条 kubectl 吞进了 set env")
                    bad += 1
    if bad == 0:
        print("  \033[32mOK\033[0m    没有发现被吞掉的命令")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
