# syntax=docker/dockerfile:1
#
# Multi-stage build for the Z.AI bridge. The repo ships WITHOUT go.mod/go.sum
# by design (see .gitignore / README step 2), so the module is initialised at
# image build time — same as a local `go mod init && go mod tidy`.

FROM golang:1.26 AS build
WORKDIR /app
COPY . .
RUN go mod init zai-api \
    && go mod tidy \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/zai-api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/zai-api /zai-api
ENV PORT=3001 \
    HOST=0.0.0.0
EXPOSE 3001
# /data 持有 tokens.sqlite / accounts.sqlite（账号 token 池）——挂卷持久化。
WORKDIR /data
VOLUME ["/data"]
ENTRYPOINT ["/zai-api"]
