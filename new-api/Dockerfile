FROM oven/bun:1.4.0@sha256:5ff609364c049b54eb0ff560ec96319729a972078ef2c755d758f0c6ef89c2d6 AS builder

WORKDIR /build/web
COPY web/package.json web/bun.lock ./
RUN --mount=type=cache,id=relay-native-bun,target=/root/.bun/install/cache,sharing=locked bun install --frozen-lockfile --verbose || bun install --frozen-lockfile --verbose || bun install --frozen-lockfile --verbose
COPY ./web ./
COPY ./VERSION /build/VERSION
RUN DISABLE_ESLINT_PLUGIN='true' VITE_REACT_APP_VERSION=$(cat /build/VERSION) bun run build

FROM golang:1.26.10-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS builder2
ENV GO111MODULE=on CGO_ENABLED=0 GOWORK=off
ENV GOPROXY=https://proxy.golang.org|https://goproxy.cn|direct

ARG TARGETOS
ARG TARGETARCH
ENV GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}
ENV GOEXPERIMENT=greenteagc

WORKDIR /build

ADD go.mod go.sum ./
# relaykit and tokenkit are local submodules referenced via replace; their
# go.mod files must be present for go mod download to resolve the main module graph.
ADD relaykit/go.mod ./relaykit/go.mod
ADD tokenkit/go.mod ./tokenkit/go.mod
RUN --mount=type=cache,id=relay-native-gomod,target=/go/pkg/mod,sharing=locked go mod download -x || go mod download -x || go mod download -x

COPY . .
COPY --from=builder /build/web/dist ./web/dist
RUN --mount=type=cache,id=relay-native-gomod,target=/go/pkg/mod,sharing=locked --mount=type=cache,id=relay-native-gobuild,target=/root/.cache/go-build (go mod download || go mod download) && go build -p 2 -pgo="$(go list -m -f '{{.Dir}}' github.com/Calcium-Ion/moejs)/default.pgo" -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(cat VERSION)'" -o new-api

FROM calciumion/new-api:v1.0.0-rc.46@sha256:abf1f675efa1847b90c049c03766e2daaf62a78998aae3625cf34b6a045b1b97

COPY --from=builder2 /build/new-api /
COPY LICENSE NOTICE THIRD-PARTY-LICENSES.md /licenses/
EXPOSE 3000
WORKDIR /data
ENTRYPOINT ["/new-api"]
