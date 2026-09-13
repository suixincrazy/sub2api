#!/bin/bash
# 确保 .env 文件存在的启动脚本
# 用途：防止服务器重启后 .env 丢失

set -euo pipefail

ENV_FILE="/root/sub2api/deploy/.env"
ENV_BACKUP="/root/sub2api/.env.backup"

# 如果 .env 不存在但备份存在，则恢复
if [ ! -f "$ENV_FILE" ] && [ -f "$ENV_BACKUP" ]; then
    echo "[$(date)] 检测到 .env 丢失，从备份恢复..."
    cp "$ENV_BACKUP" "$ENV_FILE"
    echo "[$(date)] .env 已恢复"
fi

# 如果 .env 存在但备份不存在或已过期，则更新备份
if [ -f "$ENV_FILE" ]; then
    if [ ! -f "$ENV_BACKUP" ] || [ "$ENV_FILE" -nt "$ENV_BACKUP" ]; then
        echo "[$(date)] 更新 .env 备份..."
        cp "$ENV_FILE" "$ENV_BACKUP"
        chmod 600 "$ENV_BACKUP"
        echo "[$(date)] 备份已更新"
    fi
fi

# 验证必需的环境变量
if [ -f "$ENV_FILE" ]; then
    if ! grep -q "POSTGRES_PASSWORD=" "$ENV_FILE"; then
        echo "[$(date)] 错误: .env 缺少 POSTGRES_PASSWORD"
        exit 1
    fi
    echo "[$(date)] .env 检查通过"
else
    echo "[$(date)] 错误: .env 文件不存在且无备份"
    exit 1
fi
