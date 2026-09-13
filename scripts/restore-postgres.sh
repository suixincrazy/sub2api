#!/bin/bash
# 数据恢复脚本
# 用法: ./restore-postgres.sh <备份文件路径>

set -euo pipefail

if [ $# -eq 0 ]; then
    echo "用法: $0 <备份文件路径>"
    echo "示例: $0 /root/backups/postgres/sub2api-20260913.sql.gz"
    exit 1
fi

BACKUP_FILE="$1"
CONTAINER_NAME="sub2api-postgres"
DB_USER="sub2api"
DB_NAME="sub2api"

if [ ! -f "$BACKUP_FILE" ]; then
    echo "错误: 备份文件不存在: $BACKUP_FILE"
    exit 1
fi

echo "[$(date)] 开始恢复数据库..."
echo "[$(date)] 备份文件: $BACKUP_FILE"
echo ""
echo "警告: 此操作将覆盖当前数据库！"
read -p "确认继续？(输入 YES 继续): " CONFIRM

if [ "$CONFIRM" != "YES" ]; then
    echo "取消恢复"
    exit 0
fi

# 恢复数据库
echo "[$(date)] 正在恢复..."
gunzip < "$BACKUP_FILE" | docker exec -i "$CONTAINER_NAME" psql -U "$DB_USER" "$DB_NAME"

if [ $? -eq 0 ]; then
    echo "[$(date)] 恢复成功！"
else
    echo "[$(date)] 恢复失败！"
    exit 1
fi
