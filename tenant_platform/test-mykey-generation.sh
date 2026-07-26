#!/bin/bash
set -e

echo "=== 测试 mykey.py 生成功能 ==="

# 加载环境变量
cd "$(dirname "$0")"
export $(grep -v '^#' .env | xargs)

# 等待后端启动（假设已经在运行）
echo ""
echo "1. 检查后端服务..."
if ! curl -s http://127.0.0.1:8080/health > /dev/null; then
    echo "❌ 后端未运行，请先启动: ./start-backend.ps1"
    exit 1
fi
echo "✅ 后端正在运行"

# 管理员登录
echo ""
echo "2. 管理员登录..."
LOGIN_RESP=$(curl -s -X POST http://127.0.0.1:8080/v1/admin/login \
    -H "Content-Type: application/json" \
    -H "X-Platform-Dev-Token: f02adcb48a1eaa121a4e3b6edfdafdb30c860de02492d65de879ed0da9e74149" \
    -d '{"username":"admin","password":"admin"}')

TOKEN=$(echo "$LOGIN_RESP" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)

if [ -z "$TOKEN" ]; then
    echo "❌ 登录失败"
    echo "$LOGIN_RESP"
    exit 1
fi

echo "✅ 登录成功，token: ${TOKEN:0:20}..."

# 创建测试 Provider
echo ""
echo "3. 创建测试 LLM Provider..."
PROVIDER_RESP=$(curl -s -X POST http://127.0.0.1:8080/v1/admin/llm-providers \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" \
    -d '{
        "name": "test-gpt-'$(date +%s)'",
        "provider_type": "native_oai",
        "base_url": "https://api.openai.com/v1",
        "model": "gpt-4",
        "api_key": "sk-test-key-12345",
        "config": {
            "thinking_type": "adaptive",
            "max_tokens": 8192,
            "temperature": 1.0
        }
    }')

PROVIDER_ID=$(echo "$PROVIDER_RESP" | grep -o '"id":[0-9]*' | cut -d':' -f2)

if [ -z "$PROVIDER_ID" ]; then
    echo "❌ Provider 创建失败"
    echo "$PROVIDER_RESP"
    exit 1
fi

echo "✅ Provider 创建成功，ID: $PROVIDER_ID"

# 获取 mykey.py
echo ""
echo "4. 获取生成的 mykey.py..."
MYKEY_CONTENT=$(curl -s -X GET http://127.0.0.1:8080/v1/config/mykey.py \
    -H "Authorization: Bearer $TOKEN")

if [ -z "$MYKEY_CONTENT" ]; then
    echo "❌ 获取 mykey.py 失败"
    exit 1
fi

echo "✅ mykey.py 内容："
echo "---"
echo "$MYKEY_CONTENT"
echo "---"

# 验证内容
echo ""
echo "5. 验证 mykey.py 内容..."
PASS=0
FAIL=0

if echo "$MYKEY_CONTENT" | grep -q "mixin_config"; then
    echo "  ✅ 包含 mixin_config"
    ((PASS++))
else
    echo "  ❌ 缺少 mixin_config"
    ((FAIL++))
fi

if echo "$MYKEY_CONTENT" | grep -q "native_oai"; then
    echo "  ✅ 包含 provider type"
    ((PASS++))
else
    echo "  ❌ 缺少 provider type"
    ((FAIL++))
fi

if echo "$MYKEY_CONTENT" | grep -q "thinking_type"; then
    echo "  ✅ 包含 config 字段"
    ((PASS++))
else
    echo "  ❌ 缺少 config 字段"
    ((FAIL++))
fi

if echo "$MYKEY_CONTENT" | grep -q "sk-test-key"; then
    echo "  ✅ 包含 API key"
    ((PASS++))
else
    echo "  ❌ 缺少 API key"
    ((FAIL++))
fi

# 保存到文件
echo ""
echo "6. 保存到文件..."
mkdir -p config
echo "$MYKEY_CONTENT" > config/mykey.py
echo "✅ 已保存到: config/mykey.py"

echo ""
echo "=== 测试完成 ==="
echo "通过: $PASS, 失败: $FAIL"

if [ $FAIL -gt 0 ]; then
    exit 1
fi
