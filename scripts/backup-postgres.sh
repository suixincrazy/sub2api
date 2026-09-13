#!/bin/bash
# PostgreSQL 自动备份脚本
# 用法：每日通过 cron 执行，或手动运行

set -euo pipefail

BACKUP_DIR="/root/backups/postgres"
CONTAINER_NAME="sub2api-postgres"
DB_USER="sub2api"
DB_NAME="sub2api"
RETENTION_DAYS=7

# 创建备份目录
mkdir -p "$BACKUP_DIR"

# 生成时间戳文件名
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
BACKUP_FILE="$BACKUP_DIR/sub2api-${TIMESTAMP}.sql.gz"

# 执行备份
echo "[$(date)] 开始备份数据库..."
docker exec "$CONTAINER_NAME" pg_dump -U "$DB_USER" "$DB_NAME" | gzip > "$BACKUP_FILE"

if [ $? -eq 0 ]; then
    echo "[$(date)] 备份成功: $BACKUP_FILE"
    SIZE=$(du -h "$BACKUP_FILE" | cut -f1)
    echo "[$(date)] 备份大小: $SIZE"
else
    echo "[$(date)] 备份失败！"
    exit 1
fi

# 清理旧备份（保留最近 N 天）
echo "[$(date)] 清理 ${RETENTION_DAYS} 天前的备份..."
find "$BACKUP_DIR" -name "sub2api-*.sql.gz" -type f -mtime +${RETENTION_DAYS} -delete

echo "[$(date)] 备份完成"
