"""图像通道能力建档探测（零成本三步；设计真值 .tasks/image-capability/DESIGN.zh-CN.md §8.6）。

为什么需要它：能力矩阵**必须实测**，不得按文档或按旧通道推断（§7），但生成一次要计费。
所以分三级探测，默认只跑 0 成本的两级：

  P0  GET  /v1/models                     —— 免费：模型清单
  P1  POST /images/generations 缺 prompt   —— 免费：端点在且适配器在转发（400 不产生图）
  P1  POST /images/edits       缺 image    —— 免费：改图端点/适配器是否存在
                                              （multipart 与 JSON 两种形态各试一次）
  P2  --params                            —— 免费（但信号弱）：装饰参数是否被校验
  P3  --paid                              —— **按张计费**：真图端到端（默认关闭；只在免费模型上用）

用法：
  python assets/probe_image_channel.py                     # P0+P1（默认，0 计费）
  python assets/probe_image_channel.py --models a,b        # 只探指定模型
  python assets/probe_image_channel.py --json out.json     # 结果落盘，便于回填 catalog

结论只写进报告，不自动改代码：档案是代码里的常量，必须人工复核后提交（可审计）。
"""
from __future__ import annotations

import argparse
import base64
import io
import json
import os
import sys
import time
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

# 默认探测目标：本渠道（newapi.myovo.cc.cd）的图像模型，见 DESIGN §2 与本文件报告
DEFAULT_MODELS = (
    "agnes-image-2.0-flash", "agnes-image-2.1-flash", "agnes-image-2.5-flash",
    "gemini-3.0-pro-image-preview", "gemini-3-pro-image", "gemini-3-pro-image-preview",
    "gemini-3.1-flash-image", "gemini-3.1-flash-image-preview",
    "gpt-image-2", "gpt-image-2.5-flare",
    "sensenova-u1-fast", "sensenova-u1.5-lite",
)

# 1x1 PNG（最小合法图；仅 --paid 时使用）
_TINY_PNG = bytes.fromhex(
    "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63fcffff3f0300050001ff1aa1e66e0000000049454e44ae426082"
)


def load_channel_cfg():
    """从 mykey.py 取生图通道配置（apibase/apikey）。缺失则报错退出，不猜。"""
    from llmcore import reload_mykeys
    keys = reload_mykeys()[0] or {}
    cfg = keys.get("image_gen") or keys.get("image_edit")
    if not cfg or not cfg.get("apikey") or not cfg.get("apibase"):
        raise SystemExit("mykey 里没有可用的 image_gen/image_edit 配置（需要 apibase + apikey）")
    return cfg


def _post(session, url, *, headers, timeout=30, **kw):
    t0 = time.time()
    try:
        resp = session.post(url, headers=headers, timeout=timeout, **kw)
    except requests.RequestException as e:
        return {"status": None, "error": f"{type(e).__name__}: {e}", "elapsed": round(time.time() - t0, 1)}
    body = ""
    try:
        body = (resp.text or "").strip()[:400]
    except Exception:
        pass
    return {"status": resp.status_code, "body": body, "elapsed": round(time.time() - t0, 1)}


def _looks_like_missing_field(body: str, field: str) -> bool:
    low = (body or "").lower()
    return field.lower() in low and any(h in low for h in
                                        ("required", "is required", "missing", "must", "invalid"))


def probe_models(session, base, headers):
    """P0：模型清单（免费）。"""
    try:
        resp = session.get(f"{base}/models", headers=headers, timeout=30)
        data = resp.json().get("data") or []
        return {"status": resp.status_code, "models": sorted(str(m.get("id")) for m in data)}
    except Exception as e:
        return {"status": None, "error": f"{type(e).__name__}: {e}", "models": []}


def probe_one(session, base, headers, model, *, params=False, paid=False):
    """P1/P2/P3：逐模型探测。默认只做 0 计费的两个端点探活。

    注意：本网关把**参数校验错**也标成 500（实测 gemini/gpt-image 的 size 档位错误是
    `500 new_api_error`），所以判定不能只看状态码，必须读 body 里的 message。"""
    out = {"model": model}
    # P1a 文生图端点：缺 prompt（不产生图 → 不计费）
    out["generate_endpoint"] = _post(session, f"{base}/images/generations", headers=headers,
                                     json={"model": model, "size": "1K"})
    out["generate_ok"] = _looks_like_missing_field(out["generate_endpoint"].get("body", ""), "prompt")
    # P1b 改图端点（multipart 形态）：不提供 image 字段。
    # 注意：requests 只在带 files 时才发 multipart 体——只传 data= 会发 urlencoded，
    # 网关会回 "failed to parse multipart form: multipart boundary not found"(500)，
    # 那是**我们发错了**而不是通道结论（2026-09-14 踩过，故这里用 files 真的发 multipart）。
    out["edit_multipart"] = _post(session, f"{base}/images/edits", headers=headers,
                                  files={"probe_marker": (None, "")},
                                  data={"model": model, "prompt": "x", "size": "1024x1024"})
    out["edit_multipart_ok"] = _looks_like_missing_field(out["edit_multipart"].get("body", ""), "image")
    # P1c 改图端点（JSON 形态，如 SenseNova）：缺 images 字段
    out["edit_json"] = _post(session, f"{base}/images/edits", headers=headers,
                             json={"model": model, "prompt": "x", "size": "1024x1024"})
    out["edit_json_ok"] = _looks_like_missing_field(out["edit_json"].get("body", ""), "images")
    if params:
        # P2：装饰参数探活。**必须同时留空 prompt**——只传非法 output_format 而 prompt 合法时，
        # 上游若忽略该参数就会真的出图（=计费）。这里让请求必然被拒（prompt 空），
        # 再看话术里是否点名了这个参数：命中=该通道**校验**该参数，未命中=信号不足(需 P3)。
        out["param_probe"] = _post(session, f"{base}/images/generations", headers=headers,
                                   json={"model": model, "prompt": "", "size": "1024x1024",
                                         "output_format": "zzz-invalid"})
        out["param_probe_mentions_param"] = "output_format" in out["param_probe"].get("body", "").lower()
    if paid:
        # P3：真图端到端（**按张计费**，默认关闭）。
        # 超时必须远高于 30s：慢模型实测 49.6s（SenseNova 改图），30s 会让 P3 永远"超时"
        # 而看起来像不支持（2026-09-14 踩过）。慢模型请看档案的 budget.read_timeout。
        t = 240
        out["paid_generate"] = _post(
            session, f"{base}/images/generations", headers=headers, timeout=t,
            json={"model": model, "prompt": "a small red square on white background", "size": "1024x1024"})
        b64 = base64.b64encode(_TINY_PNG).decode()
        out["paid_edit"] = _post(
            session, f"{base}/images/edits", headers=headers, timeout=t,
            files={"image": ("ref.png", _TINY_PNG, "image/png")},
            data={"model": model, "prompt": "make it green", "size": "1024x1024"})
        out["paid_edit_json"] = _post(
            session, f"{base}/images/edits", headers=headers, timeout=t,
            json={"model": model, "prompt": "make it green", "size": "1024x1024",
                  "images": [{"image_url": f"data:image/png;base64,{b64}"}]})
    return out


def _reference_png(size=512, color=(0, 0, 255)):
    """参考图（纯蓝方块）。有 pillow 就生成，否则回退 1x1 占位图（仅能验证链路，不能验像素）。"""
    try:
        from PIL import Image
    except ImportError:
        return _TINY_PNG
    buf = io.BytesIO()
    Image.new("RGB", (size, size), color).save(buf, "PNG")
    return buf.getvalue()


def _center_rgb(blob):
    """客观判据：改图后中心像素是否真的变了（"200 就算成功"不可信，DESIGN §2）。"""
    try:
        from PIL import Image
    except ImportError:
        return None
    try:
        im = Image.open(io.BytesIO(blob)).convert("RGB")
    except Exception:
        return None
    w, h = im.size
    return im.getpixel((w // 2, h // 2)), (w, h)


def probe_e2e(model, base, key, prompt="把整张图变成纯红色，不要添加任何其它元素", timeout=240):
    """P3' **生产路径**验证：用 llmcore 自己的客户端（档案驱动）真跑一次。

    与 P3（裸 HTTP）的区别：这一路同时验证「档案 → 请求构造 → 端点/形态 → 解析 → 魔数嗅探」
    整链，并对改图做像素级客观判据；这是把某条档案从 measured 升到可信的唯一依据。
    慢模型实测 50-70s，timeout 必须远大于 30s。"""
    from llmcore import OpenAIImageGenClient, sniff_image_format
    client = OpenAIImageGenClient({"name": "openai", "apibase": base, "apikey": key, "model": model,
                                   "read_timeout": timeout, "max_retries": 1})
    p = client.profile
    out = {"model": model, "api": p.api, "ops": sorted(p.ops), "size_style": p.size_style,
           "max_refs": p.max_refs, "cost": p.cost, "source": p.source}
    if "edit" in p.ops:
        t0 = time.time()
        imgs, err = client.generate(prompt, size="1024x1024",
                                    images=[("ref.png", _reference_png(), "image/png")])
        out["edit"] = {"elapsed": round(time.time() - t0, 1), "error": err,
                       "bytes": len(imgs[0]) if imgs else 0,
                       "magic": sniff_image_format(imgs[0]) if imgs else None,
                       "notices": list(client.last_notices)}
        ref_px = _center_rgb(_reference_png())
        got = _center_rgb(imgs[0]) if imgs else None
        if got:
            out["edit"]["ref_center"] = ref_px[0] if ref_px else None
            out["edit"]["out_center"] = got[0]
            out["edit"]["pixel_changed"] = (ref_px is not None and got[0] != ref_px[0])
    if "generate" in p.ops:
        t0 = time.time()
        imgs, err = client.generate("a small red square on a white background", size="1024x1024")
        out["generate"] = {"elapsed": round(time.time() - t0, 1), "error": err,
                           "bytes": len(imgs[0]) if imgs else 0,
                           "magic": sniff_image_format(imgs[0]) if imgs else None}
    return out


def summarize(results):
    """把探测结果压成"路由事实"（**不是能力结论**）。

    重要（2026-09-14 实证）：`image is required` 只能证明**网关侧路由可达且适配器在转换**，
    它**不能证明上游真能改图**——agnes 也回同一句话，而真实改图实测是 503/106s。
    所以本函数只输出路由存在性；把 `edit` 写进档案必须有 P3 真图证据（或官方能力 + 无相反实测）。"""
    rows = []
    for r in results:
        rows.append({
            "model": r["model"],
            "generate_route": bool(r.get("generate_ok")),
            "edit_route": ("multipart" if r.get("edit_multipart_ok") else
                           "json" if r.get("edit_json_ok") else None),
            "status": {k: r[k].get("status") for k in
                       ("generate_endpoint", "edit_multipart", "edit_json") if k in r},
        })
    return rows


def main():
    ap = argparse.ArgumentParser(description="图像通道能力建档探测（默认 0 计费）")
    ap.add_argument("--models", help="逗号分隔；默认探测内置清单")
    ap.add_argument("--params", action="store_true", help="额外跑 P2 装饰参数探活（仍不产生图）")
    ap.add_argument("--paid", action="store_true", help="跑 P3 真图验证（**按张计费**，默认关闭）")
    ap.add_argument("--e2e", action="store_true",
                    help="P3' 生产路径验证（走 llmcore 客户端 + 像素客观判据；**会真生成图片**）")
    ap.add_argument("--json", help="把完整结果写到该文件（便于回填 catalog）")
    args = ap.parse_args()

    cfg = load_channel_cfg()
    base = str(cfg["apibase"]).rstrip("/")
    headers = {"Authorization": f"Bearer {cfg['apikey']}", "Accept": "application/json"}
    models = [m.strip() for m in (args.models or ",".join(DEFAULT_MODELS)).split(",") if m.strip()]
    session = requests.Session()

    print(f"[probe] base={base} models={len(models)} params={args.params} paid={args.paid}")
    if args.paid:
        print("[probe] ⚠️ --paid 会真生成图片（可能计费）：只对**免费模型**使用，或确认预算")
    listing = probe_models(session, base, headers)
    print(f"[P0] GET /models -> {listing.get('status')} ({len(listing.get('models') or [])} models)")
    if args.json:
        missing = [m for m in models if m not in (listing.get("models") or [])]
        if missing:
            print(f"[P0] 警告：以下模型不在 /models 清单内: {missing}")

    if args.e2e:
        print("\n[P3'] 生产路径验证（llmcore 客户端 + 像素判据）")
        e2e = []
        for m in models:
            r = probe_e2e(m, base, cfg["apikey"])
            e2e.append(r)
            for k in ("generate", "edit"):
                if k in r:
                    v = r[k]
                    extra = (f" pixel={v.get('out_center')} changed={v.get('pixel_changed')}"
                             if "out_center" in v else "")
                    print(f"  {m:<28} {k:<8} {v['elapsed']:>6}s bytes={v['bytes']:<9} "
                          f"magic={v['magic']} err={str(v['error'])[:60]}{extra}")
        if args.json:
            Path(args.json).write_text(json.dumps({"listing": listing, "e2e": e2e},
                                                  ensure_ascii=False, indent=2), encoding="utf-8")
        print("\n[probe] e2e 会真生成图片：确保只对免费模型（cost=free）使用")
        return

    results = []
    for m in models:
        r = probe_one(session, base, headers, m, params=args.params, paid=args.paid)
        results.append(r)
        print(f"  {m:<32} gen={r['generate_endpoint'].get('status')} "
              f"edit_mp={r['edit_multipart'].get('status')} edit_json={r['edit_json'].get('status')}")

    print("\n[路由事实]（注意：路由可达 ≠ 能力已验证；edit 必须 P3 真图证据才能写进档案）")
    for row in summarize(results):
        print(f"  {row['model']:<32} generate_route={str(row['generate_route']):<5} "
              f"edit_route={row['edit_route'] or '无':<9} status={row['status']}")

    if args.json:
        Path(args.json).write_text(json.dumps({"listing": listing, "results": results},
                                              ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"\n[probe] 完整结果已写入 {args.json}")
    if not args.paid:
        print("[probe] 未跑 P3（真图验证）: 默认 0 计费，需要端到端证据时加 --paid（按张计费）")


if __name__ == "__main__":
    main()
