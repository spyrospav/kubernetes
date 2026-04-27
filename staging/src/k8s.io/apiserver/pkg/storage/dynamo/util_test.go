package dynamo

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestTxnCanceledReasonHelpers(t *testing.T) {
	err := &ddbtypes.TransactionCanceledException{
		CancellationReasons: []ddbtypes.CancellationReason{
			{Code: aws.String("TransactionConflict")},
			{Code: aws.String("ConditionalCheckFailed")},
		},
	}

	if !isTxnConflictError(err) {
		t.Fatalf("expected TransactionConflict to be retryable")
	}
	if !txnCanceledConditionalFailed(err, 1) {
		t.Fatalf("expected index 1 to be detected as ConditionalCheckFailed")
	}
	if txnCanceledConditionalFailed(err, 0) {
		t.Fatalf("did not expect index 0 to be detected as ConditionalCheckFailed")
	}
	if txnCanceledConditionalFailed(errors.New("plain error"), 0) {
		t.Fatalf("did not expect plain errors to be detected as transaction cancellation reasons")
	}
}
