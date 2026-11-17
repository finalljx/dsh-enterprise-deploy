#!/usr/bin/env python3
"""通过控制面开通一个用户。

和管理台走同一条 API（POST /internal/provision），所以命名、校验、K8s 资源、
数据库登记只有一份实现。用法：

    ./provision.py alice@corp --model-token sk-... --control-url http://127.0.0.1:8080
    ./provision.py intern01@corp --tier T0 --model-token sk-... --dry-run

必需：
    ADMIN_TOKEN    控制面管理员令牌（或 --admin-token）
    modelToken     模型网关密钥（或 --model-token / DSH_MODEL_TOKEN）

控制面从自己的环境变量取集群级约定（命名空间、镜像、StorageClass、RuntimeClass、
Issuer、域名后缀），脚本不再渲染任何 YAML，也不再直接调 kubectl。
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("user", help="企业邮箱小写，如 alice@corp")
    ap.add_argument("--tier", default="T1", choices=["T0", "T1", "T2"])
    ap.add_argument("--model-token", default=os.environ.get("DSH_MODEL_TOKEN", ""),
                    help="模型网关密钥；留空则要求同名 Secret 已存在")
    ap.add_argument("--control-url",
                    default=os.environ.get("CONTROL_URL",
                                           "http://dsh-control.dsh-system.svc.cluster.local:8080"),
                    help="控制面地址；本机执行时先 port-forward")
    ap.add_argument("--admin-token", default=os.environ.get("ADMIN_TOKEN", ""))
    ap.add_argument("--dry-run", action="store_true", help="只打印请求，不发出去")
    args = ap.parse_args()

    payload = {
        "subject": args.user.strip().lower(),
        "tier": args.tier,
        "modelToken": args.model_token,
    }
    url = args.control_url.rstrip("/") + "/internal/provision"

    if args.dry_run:
        print(json.dumps({"url": url, "body": {**payload, "modelToken": "***" if payload["modelToken"] else ""}},
                         ensure_ascii=False, indent=2))
        return 0
    if not args.admin_token:
        print("缺少 ADMIN_TOKEN（或 --admin-token）", file=sys.stderr)
        return 2

    req = urllib.request.Request(
        url,
        data=json.dumps(payload).encode(),
        method="POST",
        headers={
            "content-type": "application/json",
            "authorization": f"Bearer {args.admin_token}",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=90) as resp:
            body = resp.read().decode()
            print(body)
            return 0
    except urllib.error.HTTPError as exc:
        print(f"开通失败（HTTP {exc.code}）：{exc.read().decode(errors='replace')}", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 - 网络错误要显式报出来
        print(f"开通失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
