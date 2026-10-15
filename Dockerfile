# 二进制在本机构建（go.mod 要求 go1.26，且模块走 goproxy.cn），镜像只负责打包：
#   CGO_ENABLED=0 GOPROXY=https://goproxy.cn,direct go build -o ai-assistant-linux .
FROM scratch

# 调上游 LLM API 是 HTTPS，scratch 里没有 CA 根证书；时区数据让日志落在本地时区
COPY .docker-assets/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY .docker-assets/zoneinfo /usr/share/zoneinfo
ENV TZ=Asia/Shanghai

COPY ai-assistant-linux /ai-assistant
WORKDIR /app
EXPOSE 28888
# 容器内必须绑 0.0.0.0 宿主端口映射才进得来；宿主侧 -p 仍只绑 127.0.0.1
ENTRYPOINT ["/ai-assistant"]
CMD ["-addr", "0.0.0.0:28888", "-data", "/app/data", "-misc", "/app/misc"]
