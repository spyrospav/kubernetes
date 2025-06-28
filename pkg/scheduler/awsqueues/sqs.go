package awsqueues

import (
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
)

type SQSQueue struct {
	URL string
	svc *sqs.SQS
}

func WrapSQSMessageInput(queueURL, item string, delay int64) *sqs.SendMessageInput {
	return &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueURL),
		MessageBody: aws.String(item),
		// Set the delay to 0 seconds for immediate processing
		DelaySeconds: aws.Int64(delay),
	}
}

// Add sends a message to the SQS queue.
func (q *SQSQueue) AddAfter(item string, delay time.Duration) {
	// Calculate the delay in seconds
	delaySeconds := int64(delay.Seconds())

	// Create the SendMessageInput with the delay
	input := WrapSQSMessageInput(q.URL, item, delaySeconds)
	// Send the message to the SQS queue
	_, err := q.svc.SendMessage(input)
	if err != nil {
		log.Printf("Failed to send message to SQS queue: %v", err)
	}
}

func WrapSQSReceiveMessageInput(queueURL string) *sqs.ReceiveMessageInput {
	return &sqs.ReceiveMessageInput{
		AttributeNames: []*string{
			aws.String(sqs.MessageSystemAttributeNameSentTimestamp),
		},
		MessageAttributeNames: []*string{
			aws.String(sqs.QueueAttributeNameAll),
		},
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: aws.Int64(1),
	}
}

// Get retrieves a message from the SQS queue.
// It returns the message body and a boolean indicating whether to shut down.
func (q *SQSQueue) Get() (item string, shutdown bool) {
	// Create the ReceiveMessageInput with the queue URL
	input := WrapSQSReceiveMessageInput(q.URL)

	// Receive a message from the SQS queue
	result, err := q.svc.ReceiveMessage(input)
	if err != nil {
		log.Printf("Failed to receive message from SQS queue: %v", err)
		return "", false
	}

	if len(result.Messages) == 0 {
		return "", false // No messages available
	}

	// Return the first message body and set shutdown to false
	return *result.Messages[0].Body, false
}

func (q *SQSQueue) Done(messageID string) {
	// Create the DeleteMessageInput with the queue URL and message ID
	input := &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(q.URL),
		ReceiptHandle: aws.String(messageID),
	}

	// Delete the message from the SQS queue
	_, err := q.svc.DeleteMessage(input)
	if err != nil {
		log.Printf("Failed to delete message from SQS queue: %v", err)
	}
}

func (q *SQSQueue) Len() int {
	// Create the GetQueueAttributesInput with the queue URL
	input := &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(q.URL),
		AttributeNames: []*string{aws.String(sqs.QueueAttributeNameApproximateNumberOfMessages)},
	}

	// Get the queue attributes
	result, err := q.svc.GetQueueAttributes(input)
	if err != nil {
		log.Printf("Failed to get queue attributes: %v", err)
		return 0
	}

	if count, ok := result.Attributes[sqs.QueueAttributeNameApproximateNumberOfMessages]; ok {
		if count != nil {
			if n, err := strconv.Atoi(*count); err == nil {
				return n
			} else {
				log.Printf("Failed to convert message count to int: %v", err)
			}
		}
	}
	return 0
}

// CreateSQSQueue creates an SQS queue with the specified name if it does not already exist.
func CreateSQSQueue() (*SQSQueue, error) {
	// Create a new AWS session
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	// Create a new SQS client
	svc := sqs.New(sess)

	queueName := "pod-gc-queue"

	// Check if the queue already exists
	getQueueURLResult, err := svc.GetQueueUrl(&sqs.GetQueueUrlInput{
		QueueName: &queueName,
	})

	if err == nil {
		// Queue already exists, return its URL
		return &SQSQueue{
			URL: *getQueueURLResult.QueueUrl,
			svc: svc,
		}, nil
	}

	// If the queue does not exist, create it
	createQueueResult, err := svc.CreateQueue(&sqs.CreateQueueInput{
		QueueName: &queueName,
		Attributes: map[string]*string{
			"DelaySeconds":           aws.String("60"),
			"MessageRetentionPeriod": aws.String("86400"),
		},
	})
	if err != nil {
		log.Printf("Failed to create SQS queue: %v", err)
		// Return nil and the error if queue creation fails
		return nil, err
	}

	return &SQSQueue{
		URL: *createQueueResult.QueueUrl,
		svc: svc,
	}, nil
}
