FROM golang:1.17-alpine as cloudreve_builder

# install dependencies and build tools
RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.tuna.tsinghua.edu.cn/g' /etc/apk/repositories
RUN apk update && apk add --no-cache wget curl git yarn build-base gcc abuild binutils binutils-doc gcc-doc zip

WORKDIR /cloudreve_builder
COPY . ./Cloudreve

# build frontend
# pre-build

# build backend
WORKDIR /cloudreve_builder/Cloudreve

RUN go env -w GOPROXY="https://goproxy.io,direct" \
    && go build -a -o cloudreve

# build final image
FROM alpine:latest

WORKDIR /cloudreve

RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.tuna.tsinghua.edu.cn/g' /etc/apk/repositories
RUN apk update && apk add --no-cache tzdata ffmpeg

# we using the `Asia/Shanghai` timezone by default, you can do modification at your will
RUN cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

COPY --from=cloudreve_builder /cloudreve_builder/Cloudreve/cloudreve ./

# prepare permissions and aria2 dir
RUN chmod +x ./cloudreve

EXPOSE 5212
VOLUME ["/cloudreve/uploads", "/cloudreve/avatar", "/data"]

ENTRYPOINT ["./cloudreve"]
