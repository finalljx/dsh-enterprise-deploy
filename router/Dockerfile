FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
# 不要逐个列文件名：加了新 .go 文件却忘了改这里，镜像会在 COPY 阶段莫名其妙少文件
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dsh-router .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/dsh-router /dsh-router
USER 65532:65532
ENTRYPOINT ["/dsh-router"]
