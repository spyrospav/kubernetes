# Build stage: compile the lambda-kube-apiserver binary
FROM golang:1.24 AS builder

WORKDIR /workspace

# Copy go module files first for better layer caching
COPY go.mod go.sum go.work go.work.sum ./
COPY vendor/ vendor/
COPY staging/ staging/
# COPY _local/apiserver/ _local/apiserver/

# Copy the source needed for the build
COPY cmd/lambda-kube-apiserver/ cmd/lambda-kube-apiserver/
COPY cmd/kube-apiserver/ cmd/kube-apiserver/
COPY pkg/ pkg/
COPY plugin/ plugin/
COPY third_party/forked/gonum/ third_party/forked/gonum/

# Build a statically-linked binary for the Lambda runtime
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -mod=vendor -o /lambda-kube-apiserver \
    ./cmd/lambda-kube-apiserver

# Generate service account key pair for token signing in builder stage
RUN mkdir -p /etc/kubernetes/pki && \
    openssl genrsa -out /etc/kubernetes/pki/sa.key 2048 && \
    openssl rsa -in /etc/kubernetes/pki/sa.key -pubout -out /etc/kubernetes/pki/sa.pub

# Create a static token auth file for local/serverless testing.
# Format: token,username,uid,"group1,group2"
RUN mkdir -p /etc/kubernetes/auth && \
    printf "devtoken123,lambda-user,uid-lambda,\"system:masters\"\n" > /etc/kubernetes/auth/tokens.csv

# Runtime stage: AWS Lambda provided.al2023 base image
FROM public.ecr.aws/lambda/provided:al2023 AS runtime

ENV AWS_SDK_LOAD_CONFIG=1 \
    AWS_SHARED_CREDENTIALS_FILE=/root/.aws/credentials \
    AWS_CONFIG_FILE=/root/.aws/config \
    AWS_EC2_METADATA_DISABLED=true

COPY --from=builder /lambda-kube-apiserver ${LAMBDA_RUNTIME_DIR}/bootstrap
COPY --from=builder /etc/kubernetes/pki /etc/kubernetes/pki
COPY --from=builder /etc/kubernetes/auth /etc/kubernetes/auth

CMD ["lambda-kube-apiserver"]
