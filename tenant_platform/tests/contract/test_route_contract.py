"""审查 F7: OpenAPI 与后端路由自动比对。

从 backend-go 源码提取全部字面路由注册(mux.HandleFunc), 与 OpenAPI
paths 双向比对:
- spec 声明但后端不存在的路由(幽灵接口, 误导客户端);
- 后端实现但 spec 未声明的路由(契约缺口, 客户端无法发现)。

排除规则: 不在此处自动判定的仅文档路由通过 OPENAPI_ONLY_ROUTES 白名单
显式声明, 防止测试替实现方做产品决策。
"""

import pathlib
import re

import yaml

ROOT = pathlib.Path(__file__).parents[2]

# 仅存在于 OpenAPI、当前后端未注册的合法路径(明确标注: 计划中/已废弃)。
# 新增幽灵接口必须先在此声明理由, 否则测试失败。
OPENAPI_ONLY_ROUTES: set[tuple[str, str]] = set()

# 已实现但尚未写入 OpenAPI 的存量缺口(审查 F7 基线): 新路由必须进 spec,
# 存量缺口在此显式声明并随迭代收敛; 任何新增后端路由若未在此声明且未写
# 入 spec, 测试立即失败。
KNOWN_SPEC_GAPS: set[tuple[str, str]] = set()  # 审查 I-4/OpenAPI 收敛: 存量缺口已全部补齐

BACKEND_GLOB = "backend-go/internal/api/*.go"


def _extract_backend_routes() -> set[tuple[str, str]]:
    """从 api 包源码提取 (method, path) 路由集合。"""
    routes: set[tuple[str, str]] = set()

    for f in sorted((ROOT / "backend-go" / "internal" / "api").glob("*.go")):
        if f.name.endswith("_test.go"):
            continue
        text = f.read_text(encoding="utf-8")
        # mux.HandleFunc("GET /v1/path", handler) 以及带 {} 路径参数的形式
        for m in re.finditer(r'HandleFunc\("(GET|POST|PUT|DELETE|PATCH)\s+([^"]+)"', text):
            method, path = m.group(1).lower(), m.group(2)
            routes.add((method, path))
    return routes


def _openapi_paths() -> dict:
    with open(ROOT / "contracts/openapi/platform.yaml", encoding="utf-8") as f:
        return yaml.safe_load(f)["paths"]


def test_backend_routes_matches_openapi_contract():
    backend = _extract_backend_routes()
    openapi = _openapi_paths()

    spec_ops: set[tuple[str, str]] = set()
    for path, item in openapi.items():
        for method in ("get", "post", "put", "delete", "patch"):
            if method in item:
                spec_ops.add((method, path))

    implemented_but_not_in_spec = backend - spec_ops
    # 未在已知缺口清单中的后端路由 → 契约缺口(新增路由未同步 spec)。
    new_gaps = implemented_but_not_in_spec - KNOWN_SPEC_GAPS
    # spec 声明但后端缺失 → 幽灵接口(白名单除外)
    ghost = {op for op in spec_ops - backend if op not in OPENAPI_ONLY_ROUTES}

    problems = []
    if new_gaps:
        problems.append(
            "NEW backend routes missing from OpenAPI (must be added to spec or KNOWN_SPEC_GAPS): "
            + ", ".join(f"{m.upper()} {p}" for m, p in sorted(new_gaps))
        )
    if ghost:
        problems.append(
            "OpenAPI routes missing from backend (ghost endpoints): "
            + ", ".join(f"{m.upper()} {p}" for m, p in sorted(ghost))
        )
    if problems:
        raise AssertionError("\n".join(problems))
