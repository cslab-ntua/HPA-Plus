# Copyright 2022 The Predictive Horizontal Pod Autoscaler Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build the manager binary
FROM golang:1.27.1@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS builder

WORKDIR /workspace

# Copy the Go module manifests first so dependency downloads stay cached.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the go source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o manager main.go && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o worker-server ./cmd/worker-server && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o cgroup-collector ./cmd/cgroup-collector

FROM python:3.14.7-slim-trixie@sha256:caaf356f40667c496d405780745b9ac25771c189a51dfcc42430d531ea09f8a2
WORKDIR /app

# Install system dependencies for Python packages
RUN apt-get update && \
    apt-get upgrade -y && \
    apt-get install -y --no-install-recommends libgomp1 && \
    rm -rf /var/lib/apt/lists/*

# Install Python dependencies
COPY algorithms/requirements.txt ./algorithms/requirements.txt
RUN python -m pip install --no-cache-dir --require-hashes -r algorithms/requirements.txt

COPY algorithms/ ./algorithms
COPY --from=builder /workspace/manager /workspace/worker-server /workspace/cgroup-collector ./
RUN chmod -R go+rX /app/algorithms && \
    chmod +x /app/manager && \
    chown -R 65532:65532 /app
USER 65532:65532

ENTRYPOINT ["/app/manager"]
