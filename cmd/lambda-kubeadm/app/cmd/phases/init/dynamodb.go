package phases

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	"sigs.k8s.io/yaml"
)

//go:embed resources.csv
var resourcesCSV string

func NewDynamoDBPhase() workflow.Phase {
	return workflow.Phase{
		Name:  "dynamodb",
		Short: "DynamoDB configuration",
		Run:   runDynamoDB,
		InheritFlags: []string{
			"template-path",
		},
	}
}

func runDynamoDB(c workflow.RunData) error {
	data, ok := c.(InitData)
	if !ok {
		return errors.New("dynamodb phase invoked with an invalid data struct")
	}

	templatePath := data.TemplatePath()
	if templatePath == "" {
		return errors.New("template-path is empty; expected previous phase to initialize the SAM template path")
	}
	path := filepath.Clean(templatePath)

	klog.V(0).Infoln("[dynamodb] Updating SAM template: ", path)
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	template := map[string]any{}
	if err := yaml.Unmarshal(content, &template); err != nil {
		return err
	}

	resources := map[string]any{}
	if existingResources, ok := template["Resources"]; ok {
		typed, ok := existingResources.(map[string]any)
		if !ok {
			return errors.New("template Resources section is not a map")
		}
		resources = typed
	}

	csvResources, err := loadResourcesFromCSV()
	if err != nil {
		return err
	}

	for _, resource := range csvResources {
		logicalID := dynamoDBLogicalID(resource)
		if _, exists := resources[logicalID]; exists {
			klog.V(1).Infoln("[dynamodb] Resource already exists; leaving unchanged: ", logicalID)
			continue
		}
		resources[logicalID] = dynamoDBTableResource(resource, "key")
		klog.V(0).Infoln("[dynamodb] Added DynamoDB table resource: ", logicalID)
	}

	template["Resources"] = resources
	updated, err := yaml.Marshal(template)
	if err != nil {
		return err
	}

	return os.WriteFile(path, updated, 0o644)
}

func dynamoDBTableResource(tableName string, hashKey string) map[string]any {
	return map[string]any{
		"Type": "AWS::DynamoDB::Table",
		"Properties": map[string]any{
			"TableName":   tableName,
			"BillingMode": "PAY_PER_REQUEST",
			"AttributeDefinitions": []map[string]any{
				{
					"AttributeName": hashKey,
					"AttributeType": "S",
				},
			},
			"KeySchema": []map[string]any{
				{
					"AttributeName": hashKey,
					"KeyType":       "HASH",
				},
			},
		},
	}
}

func dynamoDBLogicalID(resource string) string {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return "DynamoDBTable"
	}
	var b strings.Builder
	for _, r := range resource {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	cleaned := b.String()
	if cleaned == "" {
		return "DynamoDBTable"
	}
	return upperFirst(cleaned) + "Table"
}

func upperFirst(value string) string {
	if value == "" {
		return value
	}
	runes := []rune(value)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func loadResourcesFromCSV() ([]string, error) {
	reader := csv.NewReader(bytes.NewBufferString(resourcesCSV))
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	var resources []string
	for i, record := range records {
		if i == 0 {
			continue
		}
		if len(record) < 2 {
			continue
		}
		resources = append(resources, record[1])
	}

	return resources, nil
}
