#!/bin/bash

# 停止并删除现有 charming_hawking 容器
docker rm -f charming_hawking 2>/dev/null || true

# 构建 ruiqi-debug 镜像
docker build -f Dockerfile.debug -t ruiqi-debug .

# 运行容器，挂载本地代码目录并映射端口
docker run -it \
  --name charming_hawking \
  -p 40000:40000 \
  -p 2334:2333 \
  -p 80:80 \
  -p 8080:8080 \
  -v /Users/liangfeng/GolandProjects/RuiQi-main/server:/app/server \
  ruiqi-debug

# 启动 waf-demo-mongodb 容器
docker start waf-demo-mongodb