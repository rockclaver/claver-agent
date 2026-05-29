# Phase 9 AC6: dockerized agent image used by the CI E2E smoke harness.
# Builds the agent and ships a minimal runtime image with git + tmux on PATH
# (both are required by the projects and sessions subsystems).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/claver-agent ./cmd/claver-agent

FROM alpine:3.20
RUN apk add --no-cache git tmux ca-certificates openssh-client bash
COPY --from=build /out/claver-agent /usr/local/bin/claver-agent
RUN mkdir -p /var/lib/claver
ENV CLAVER_DATA_DIR=/var/lib/claver
EXPOSE 7676
# We deliberately bind only to loopback inside the container; the E2E harness
# port-forwards via `docker run -p 127.0.0.1:7676:7676` and the agent's
# loopback self-check happily binds because the container's lo is loopback.
ENTRYPOINT ["/usr/local/bin/claver-agent", "--addr=127.0.0.1:7676", "--data-dir=/var/lib/claver"]
