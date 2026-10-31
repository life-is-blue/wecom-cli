#!/usr/bin/env bash
# Starts a throwaway MySQL 8.0 in Docker for development and tests, with a user `oe`/`oe`.
#   scripts/dev-mysql.sh            # listens on 127.0.0.1:33307
#   export OPENETL_TEST_DSN='oe:oe@tcp(127.0.0.1:33307)/openetl_test'
#   scripts/dev-mysql.sh stop       # remove the container
set -euo pipefail
NAME=${OPENETL_DEV_MYSQL:-openetl-mysql8}; PORT=${OPENETL_DEV_MYSQL_PORT:-33307}
if [ "${1:-}" = stop ]; then docker rm -f "$NAME" >/dev/null; exit 0; fi
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -e MYSQL_ALLOW_EMPTY_PASSWORD=yes -p 127.0.0.1:$PORT:3306 mysql:8.0 --character-set-server=utf8mb4 >/dev/null
for _ in $(seq 60); do docker exec "$NAME" mysqladmin -uroot ping >/dev/null 2>&1 && break; sleep 2; done
docker exec -i "$NAME" mysql -uroot -e "CREATE USER IF NOT EXISTS 'oe'@'%' IDENTIFIED WITH mysql_native_password BY 'oe'; GRANT ALL ON *.* TO 'oe'@'%'; CREATE DATABASE IF NOT EXISTS openetl_test CHARACTER SET utf8mb4;"
echo "MySQL $(docker exec "$NAME" mysql -uroot -N -e 'SELECT VERSION()') ready on 127.0.0.1:$PORT (user oe/oe, database openetl_test)"
