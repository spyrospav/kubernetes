package awsstore

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const PartitionStoreTableName = "scheduler-state"
const NodeStoreTableName = "scheduler-nodes"

func EnsurePartitionStoreTable(ctx context.Context, ddb *dynamodb.Client, table string) error {
	_, err := ddb.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(table),
	})
	if err == nil {
		return nil
	}
	var rnfe *types.ResourceNotFoundException
	if !errors.As(err, &rnfe) {
		return err
	}
	_, err = ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(table),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String(attrPK), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrSK), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(attrPK), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(attrSK), KeyType: types.KeyTypeRange},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	return err
}

func EnsureNodeStoreTable(ctx context.Context, ddb *dynamodb.Client, table string) error {
	// If exists, return.
	_, err := ddb.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
	if err == nil {
		return nil
	}
	var rnfe *types.ResourceNotFoundException
	if !errors.As(err, &rnfe) {
		return err
	}

	// Create with PK/SK and GSI on Gen.
	_, err = ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(table),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String(nodeTablePK), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String(nodeTableSK), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String(nodeAttrGen), AttributeType: types.ScalarAttributeTypeN},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(nodeTablePK), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(nodeTableSK), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String(nodeGSIByGenName),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String(nodeTablePK), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String(nodeAttrGen), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	return err
}
