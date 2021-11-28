package outgoing

import (
	"encoding/json"
	"time"
)

// Document is the body sent to ElasticSearch
// It tries to closly resumble the FirehoseRequest as much as possible
// It deviates from the FirehoseRequest in that it does not contain nested events
// Each event is sent as a separate document
type Document struct {
	RequestID string    `json:"requestId"`
	TimeStamp time.Time `json:"@timestamp"`
	Record   Record    `json:"records"`
}

type Record struct {
	Data Data `json:"data"`
}

type Data struct {
	MessageType         string   `json:"messageType"`
	Owner               string   `json:"owner"`
	LogGroup            string   `json:"logGroup"`
	LogStream           string   `json:"logStream"`
	SubscriptionFilters []string `json:"subscriptionFilters"`
	LogEvent           LogEvent `json:"logEvents"`
}

type LogEvent struct {
	ID        string          `json:"id"`
	Timestamp time.Time           `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}
