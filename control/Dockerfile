FROM golang:1.22-alpine AS build
# 内网或国内：--build-arg GOPROXY=https://goproxy.cn,direct（或企业自己的 goproxy）
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum* ./
RUN GOPROXY="$GOPROXY" go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dsh-control .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/dsh-control /dsh-control
USER 65532:65532
ENTRYPOINT ["/dsh-control"]
