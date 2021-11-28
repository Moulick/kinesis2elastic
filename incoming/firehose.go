// This file was generated from JSON Schema using quicktype, do not modify it directly.
// To parse and unparse this JSON data, add this code to your project and do:
//
//    firehoseRequest, err := UnmarshalFirehoseRequest(bytes)
//    bytes, err = firehoseRequest.Marshal()

package incoming

import "encoding/json"

func UnmarshalFirehoseRequest(data []byte) (FirehoseRequest, error) {
	var r FirehoseRequest
	err := json.Unmarshal(data, &r)
	return r, err
}

func (r *FirehoseRequest) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

type FirehoseRequest struct {
	RequestID string   `json:"requestId"`
	Timestamp int64    `json:"timestamp"`
	Records   []Record `json:"records"`
}

type Record struct {
	Data Data `json:"data"`
}

type Data struct {
	MessageType         string     `json:"messageType"`
	Owner               string     `json:"owner"`
	LogGroup            string     `json:"logGroup"`
	LogStream           string     `json:"logStream"`
	SubscriptionFilters []string   `json:"subscriptionFilters"`
	LogEvents           []LogEvent `json:"logEvents"`
}

type LogEvent struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
	Message   string `json:"message"`
}
