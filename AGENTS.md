docker 镜像构建时需要添加以下构建参数：
```
--build-arg HTTPS_PROXY=http://172.29.240.1:7890 --build-arg HTTP_PROXY=http://172.29.240.1:7890
```

启动 docker-compose.yaml 方式：
```bash
podman compose up -d --pull never
```
docker-compose 中服务镜像构建触发方式：
```bash
podman compose build --build-arg HTTPS_PROXY=http://172.29.240.1:7890
```
