#!/bin/bash
set -e

# 配置变量
REGISTRY="registry.cn-shanghai.aliyuncs.com"
NAMESPACE="hxh_ai"
IMAGE_NAME="cli-proxy-api-plus"
VERSION_FILE=".docker-version"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 检查版本文件是否存在
if [ ! -f "$VERSION_FILE" ]; then
    echo "0" > "$VERSION_FILE"
    echo -e "${YELLOW}创建版本文件: $VERSION_FILE${NC}"
fi

# 读取当前版本号
CURRENT_VERSION=$(cat "$VERSION_FILE")
echo -e "${GREEN}当前版本号: ${CURRENT_VERSION}${NC}"

# 递增版本号
NEW_VERSION=$((CURRENT_VERSION + 1))
echo -e "${GREEN}新版本号: ${NEW_VERSION}${NC}"

# 获取 Git 信息
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_TAG=$(git describe --tags --always --dirty 2>/dev/null || echo "v${NEW_VERSION}")
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# 完整镜像名
FULL_IMAGE="${REGISTRY}/${NAMESPACE}/${IMAGE_NAME}"

echo ""
echo "================================"
echo "Docker 镜像构建与推送"
echo "================================"
echo "镜像仓库: ${FULL_IMAGE}"
echo "版本号: v${NEW_VERSION}"
echo "Git Commit: ${GIT_COMMIT}"
echo "Git Tag: ${GIT_TAG}"
echo "构建时间: ${BUILD_DATE}"
echo "================================"
echo ""

# 询问用户是否继续
read -p "是否继续构建并推送? (y/n): " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo -e "${RED}操作已取消${NC}"
    exit 1
fi

# 询问是否使用缓存
read -p "是否使用 Docker 缓存加速构建? (y/n, 默认 y): " -n 1 -r
echo
USE_CACHE=true
if [[ $REPLY =~ ^[Nn]$ ]]; then
    USE_CACHE=false
    echo -e "${YELLOW}将不使用缓存构建（可能需要较长时间）${NC}"
else
    echo -e "${GREEN}将使用缓存加速构建${NC}"
fi

# 构建镜像
echo ""
echo -e "${GREEN}[1/3] 正在构建镜像...${NC}"
if [ "$USE_CACHE" = false ]; then
    docker build --no-cache \
        --build-arg VERSION="${GIT_TAG}" \
        --build-arg COMMIT="${GIT_COMMIT}" \
        --build-arg BUILD_DATE="${BUILD_DATE}" \
        -t ${FULL_IMAGE}:v${NEW_VERSION} \
        -t ${FULL_IMAGE}:latest \
        .
else
    docker build \
        --build-arg VERSION="${GIT_TAG}" \
        --build-arg COMMIT="${GIT_COMMIT}" \
        --build-arg BUILD_DATE="${BUILD_DATE}" \
        -t ${FULL_IMAGE}:v${NEW_VERSION} \
        -t ${FULL_IMAGE}:latest \
        .
fi

if [ $? -ne 0 ]; then
    echo -e "${RED}构建失败！${NC}"
    exit 1
fi

echo -e "${GREEN}✓ 镜像构建成功${NC}"

# 登录阿里云
echo ""
echo -e "${GREEN}[2/3] 登录阿里云镜像仓库...${NC}"
docker login ${REGISTRY}

if [ $? -ne 0 ]; then
    echo -e "${RED}登录失败！${NC}"
    exit 1
fi

# 推送镜像
echo ""
echo -e "${GREEN}[3/3] 正在推送镜像...${NC}"
docker push ${FULL_IMAGE}:v${NEW_VERSION}
docker push ${FULL_IMAGE}:latest

if [ $? -ne 0 ]; then
    echo -e "${RED}推送失败！${NC}"
    exit 1
fi

# 保存新版本号
echo "$NEW_VERSION" > "$VERSION_FILE"

echo ""
echo "================================"
echo -e "${GREEN}✓ 完成！${NC}"
echo "================================"
echo "版本号已更新: ${CURRENT_VERSION} → ${NEW_VERSION}"
echo ""
echo "已推送的镜像:"
echo "  - ${FULL_IMAGE}:v${NEW_VERSION}"
echo "  - ${FULL_IMAGE}:latest"
echo ""
echo "拉取镜像命令:"
echo "  docker pull ${FULL_IMAGE}:v${NEW_VERSION}"
echo "  docker pull ${FULL_IMAGE}:latest"
echo "================================"
