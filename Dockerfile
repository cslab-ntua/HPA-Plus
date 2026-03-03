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
FROM golang:1.20 as builder

WORKDIR /workspace
# Always use vendored dependencies to avoid needing network access during build
ENV GOFLAGS=-mod=vendor

# Copy the Go Modules manifests and vendor directory so dependency layer stays cached
COPY go.mod go.mod
COPY go.sum go.sum
COPY vendor/ vendor/

# Copy the go source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

FROM python:3.8-slim-bullseye
WORKDIR /app

# Install system dependencies for Python packages
RUN apt-get update && \
    apt-get install -y --no-install-recommends libgomp1 && \
    rm -rf /var/lib/apt/lists/*

# Install Python dependencies
COPY algorithms/requirements.txt ./algorithms/requirements.txt
RUN python -m pip install -r algorithms/requirements.txt

COPY algorithms/ ./algorithms
COPY --from=builder /workspace/manager .
RUN chmod -R go+rX /app/algorithms && \
    chmod +x /app/manager && \
    chown -R 65532:65532 /app
USER 65532:65532

ENTRYPOINT ["/app/manager"]
