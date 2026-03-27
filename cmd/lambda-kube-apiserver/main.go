package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/storage/storagebackend"

	"k8s.io/klog/v2"
	app "k8s.io/kubernetes/cmd/kube-apiserver/app"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
)

func main() {

	ctx := context.Background()
	s := options.NewServerRunOptions()

	// Hardcode key startup options for serverless mode.
	s.Authorization.Modes = []string{"AlwaysAllow"}
	s.Authentication.Anonymous.Allow = true

	dynamoRegion := getenvDefault("DYNAMO_REGION", "us-east-1")
	dynamoTable := getenvDefault("DYNAMO_TABLE", "dynamo")
	dynamoEndpoint := os.Getenv("DYNAMO_ENDPOINT")

	if os.Getenv("AWS_REGION") == "" {
		_ = os.Setenv("AWS_REGION", dynamoRegion)
	}
	if os.Getenv("AWS_DEFAULT_REGION") == "" {
		_ = os.Setenv("AWS_DEFAULT_REGION", dynamoRegion)
	}
	if dynamoEndpoint == "" && !hasAWSCredentialSource() {
		klog.InfoS("No explicit AWS credential source detected; DynamoDB auth may fail", "hint", "set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, AWS_PROFILE with mounted /root/.aws, or run with Lambda execution role")
	}

	s.Etcd.StorageConfig.Transport.ServerList = []string{"https://127.0.0.1:2379"}
	s.Etcd.StorageConfig.Type = storagebackend.StorageTypeDynamo
	s.Etcd.EnableWatchCache = false
	s.CustomStorage.DynamoRegion = dynamoRegion
	s.CustomStorage.DynamoTable = dynamoTable
	s.CustomStorage.DynamoEndpoint = dynamoEndpoint
	s.Authentication.TokenFile.TokenFile = "/etc/kubernetes/auth/tokens.csv"
	// Populate required options from a known-good kubeadm apiserver config.
	s.Authentication.ServiceAccounts.Issuers = []string{"https://kubernetes.default.svc.cluster.local"}
	s.Authentication.ServiceAccounts.KeyFiles = []string{"/etc/kubernetes/pki/sa.pub"}
	s.ServiceAccountSigningKeyFile = "/etc/kubernetes/pki/sa.key"

	completedOptions, err := s.Complete(ctx)
	if err != nil {
		log.Fatalf("failed to complete default options: %v", err)
	}

	// Defensively set Dynamo fields on the completed options consumed by the
	// storage factory to avoid empty-region failures during initialization.
	completedOptions.Etcd.StorageConfig.Type = storagebackend.StorageTypeDynamo
	completedOptions.Etcd.StorageConfig.Dynamo.Region = dynamoRegion
	completedOptions.Etcd.StorageConfig.Dynamo.TableName = dynamoTable
	completedOptions.Etcd.StorageConfig.Dynamo.Endpoint = dynamoEndpoint
	completedOptions.Etcd.EnableWatchCache = false
	completedOptions.Etcd.SkipHealthEndpoints = true
	completedOptions.CustomStorage.DynamoRegion = dynamoRegion
	completedOptions.CustomStorage.DynamoTable = dynamoTable
	completedOptions.CustomStorage.DynamoEndpoint = dynamoEndpoint

	if errs := completedOptions.Validate(); len(errs) != 0 {
		log.Fatalf("invalid default options: %v", utilerrors.NewAggregate(errs))
	}

	handler, err := buildHandler(ctx, completedOptions)
	if err != nil {
		log.Fatalf("failed to build handler: %v", err)
	}

	adapter := httpadapter.New(handler)
	lambda.Start(adapter.ProxyWithContext)
}

// buildHandler constructs the full http.Handler for the Lambda API server.
// This runs once per Lambda cold start. The returned handler is reused across
// all warm invocations.
func buildHandler(ctx context.Context, opts options.CompletedOptions) (http.Handler, error) {
	// To help debugging, immediately log version

	klog.InfoS("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	config, err := app.NewConfig(opts)
	if err != nil {
		return nil, err
	}
	completed, err := config.Complete()
	if err != nil {
		return nil, err
	}
	server, err := app.CreateServerChain(completed)
	if err != nil {
		return nil, err
	}

	prepared, err := server.PrepareRun()
	if err != nil {
		return nil, err
	}

	prepared.APIAggregator.GenericAPIServer.RunPostStartHooks(ctx)

	handler := prepared.APIAggregator.GenericAPIServer.Handler
	return handler, nil
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func hasAWSCredentialSource() bool {
	for _, key := range []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_PROFILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}

	if _, err := os.Stat("/root/.aws/credentials"); err == nil {
		return true
	}
	return false
}
