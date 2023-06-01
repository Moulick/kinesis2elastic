package main

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Depado/ginprom"
	"github.com/cenkalti/backoff/v4"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/jnovack/flag"
	"github.com/opensearch-project/opensearch-go"
	"github.com/opensearch-project/opensearch-go/opensearchtransport"
	"github.com/opensearch-project/opensearch-go/opensearchutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Moulick/Kinesis2Elastic/gzipbinding"
	"github.com/Moulick/Kinesis2Elastic/incoming"
	"github.com/Moulick/Kinesis2Elastic/log"
	"github.com/Moulick/Kinesis2Elastic/outgoing"
)

// //go:embed ingest/axway-ingest.json
// var axwayIngest string
//
// //go:embed ingest/one-pipeline-to-rule-them-all.json
// var onePipelineToRuleThemAll string

const (
	numWorkers         = 3
	flushBytes         = 5000000
	incomingAuthHeader = "X-Amz-Firehose-Access-Key"
	shutdownTimeout    = 30 * time.Second
)

var (
	errEncodingMismatch = errors.New("data encoding mismatch")
	MIMEGZIP            = "application/x-gzip"
)

// firehoseSuccessResponse is the reponse sent back to AWS Kinesis Firehose on success
// https://docs.aws.amazon.com/firehose/latest/dev/httpdeliveryrequestresponse.html
type firehoseSuccessResponse struct {
	RequestID string `json:"requestId"`
	Timestamp int64  `json:"timestamp"`
}

// firehoseErrorBody is the reponse sent back to AWS Kinesis Firehose on error
// https://docs.aws.amazon.com/firehose/latest/dev/httpdeliveryrequestresponse.html
type firehoseErrorBody struct {
	RequestID string `json:"requestId"`
	Timestamp int64  `json:"timestamp"`
	Error     string `json:"errorMessage"`
}

// dataDetect detects the content type and encoding of the request body.
// returns error for unsupported Content-Encoding and Content-Type if not supported or mismatch in header vs body
// return nil and detected content type if success
func dataDetect(c *gin.Context, logger *zap.SugaredLogger) (string, error) {
	// extract header for incoming request
	contentType := c.ContentType()
	logger.Debugw("Content-Type", "Content-Type", contentType)
	// is the content type supported?
	if contentType != "application/json" {
		return contentType, fmt.Errorf("unsupported Content-Type: %s", contentType)
	}

	contentEncodingHeader := c.GetHeader("Content-Encoding")
	logger.Debugw("Content-Encoding", "Content-Encoding", contentEncodingHeader)
	// is content encoding supported?
	if contentEncodingHeader != "" {
		if contentEncodingHeader != "gzip" {
			return "", fmt.Errorf("unsupported Content-Encoding %s", contentEncodingHeader)
		}
	}

	// Duplicate the body, as like when we do detection, it closes the body
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return "", err
	}
	// Restore the request body to its original state
	c.Request.Body = io.NopCloser(bytes.NewBuffer(body))

	// convert body to bytes
	reqBodyBytes, err := io.ReadAll(io.NopCloser(bytes.NewBuffer(body)))
	if err != nil {
		return "", err
	}
	// check if body is gzip compressed
	realContentEncoding := http.DetectContentType(reqBodyBytes)

	// log if the header and content type are not the same
	if realContentEncoding == MIMEGZIP {
		if contentEncodingHeader != "gzip" {
			logger.Warnw("detected data encoding mismatch, incoming Content-Encoding header not set properly", "expected", "gzip", "received", realContentEncoding)
			return "", errEncodingMismatch
		}
	} else if strings.Contains(realContentEncoding, "text/plain") {
		return "text/plain", nil
	} else {
		return "", fmt.Errorf("unsupported data encoding %s", realContentEncoding)
	}
	// all checkes passed, return the content type
	return realContentEncoding, nil
}

//nolint:funlen
func main() {
	fmt.Println("Starting Kinesis2Elastic")
	// Create context that listens for the interrupt signal from the OS.
	// kill (no param) default send syscall.SIGTERM
	// kill -2 is syscall.SIGINT
	// kill -9 is syscall.SIGKILL but can't be catch, so don't need add it
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var (
		opensearchURL      string
		outputIndex        string
		countSuccessful    uint64
		port               string
		openSearchPipeline string
	)

	flag.StringVar(&opensearchURL, "opensearch_url", "http://localhost:9200", "URL of the OpenSearch Cluster with schema and optional Port")
	flag.StringVar(&outputIndex, "output_index", "firehose-output", "output_index name to create documents, output_index will be created if does not exist")
	flag.StringVar(&port, "port", "8080", "port to listen on")
	flag.StringVar(&openSearchPipeline, "opensearch_pipeline", "one-pipeline-to-rule-them-all", "pipeline to use for OpenSearch")
	// zap logger setup
	opts := log.Options{}
	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	// This config is both dev and prod depending on the log level set
	zapConfig := zap.Config{
		Level:       zap.NewAtomicLevelAt(zapcore.Level(opts.Level)),
		Development: opts.Development,
		Sampling: &zap.SamplingConfig{
			Initial:    100,
			Thereafter: 100,
		},
		Encoding: opts.Encoder,
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "ts",
			LevelKey:       "level",
			NameKey:        "logger",
			CallerKey:      "caller",
			FunctionKey:    zapcore.OmitKey,
			MessageKey:     "msg",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.CapitalLevelEncoder,
			EncodeTime:     zapcore.RFC3339TimeEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		},
		OutputPaths:       []string{"stderr"},
		ErrorOutputPaths:  []string{"stderr"},
		DisableStacktrace: false,
	}

	logger, _ := zapConfig.Build(zap.WithCaller(true))
	defer func(logger *zap.Logger) {
		// if cannot sync logger, probably nothing can be done anyway
		_ = logger.Sync()
	}(logger)

	suggar := logger.Sugar()

	suggar.Infof("Log Level is %s", zapConfig.Level.String())
	suggar.Infow("OpenSearch URL is ", "URL", opensearchURL)
	suggar.Infow("OpenSearch Output Index", "index", outputIndex)
	suggar.Infow("OpenSearch worker threads", "workers", numWorkers)

	suggar.Debug("DEBUG LOG")
	suggar.Info("INFO LOG")
	suggar.Warn("WARN LOG")
	suggar.Error("ERROR LOG")
	// suggar.Panic("PANIC LOG")
	// suggar.Fatal("FATAL LOG")

	if !opts.Development {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()

	// Add a ginzap middleware, which:
	//   - Logs all requests, like a combined access and error log.
	//   - Logs to stdout.
	//   - RFC3339 with UTC time format.
	r.Use(ginzap.Ginzap(logger, time.RFC3339, true))

	// Logs all panic to error log
	//   - stack means whether output the stack info.
	r.Use(ginzap.RecoveryWithZap(logger, true))

	// Setup the prometheus handler
	p := ginprom.New(ginprom.Engine(r))
	r.Use(p.Instrument())

	// This one basically for health check
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "pong",
		})
	})

	/*
		firehose API is designed to be invoked by AWS Kinesis Firehose Delivery Stream
		It takes the incoming data, breaks up the records into individual OpenSearch Documents
		It reuses the same X-Amz-Firehose-Request-Id for all the documents which were part of the same request
		It also converts the timestamp to @timestamp that OpenSearch can understand
		It passes on the X-Amz-Firehose-Access-Key from the request to OpenSearch basic auth,
			the X-Amz-Firehose-Access-Key is expected to be a base64 encoded username:password
		It uses the bulk output_index api of OpenSearch for best performance
	*/
	// Example Json
	// {
	//   "requestId": "ed4acda5-034f-9f42-bba1-f29aea6d7d8f",
	//   "timestamp": 1635622518652,
	//   "records": [
	//     {
	//       "data": "eyJQUklDRSI6ODQuNTF9"
	//     },
	//     {
	//       "data": "eyJQUklDRSI6ODQuNTF9"
	//     }
	//   ]
	// }
	r.POST("/firehose", func(c *gin.Context) {
		var (
			zaplog       *zap.SugaredLogger
			dataIncoming incoming.FirehoseRequest
		)

		// body, _ := io.ReadAll(c.Request.Body)                // TODO: remove this
		// c.Request.Body = io.NopCloser(bytes.NewBuffer(body)) // TODO: remove this
		// fmt.Println(string(body))                            // TODO: remove this
		// fmt.Println(json.Marshal(c.Request.Header))          // TODO: remove this

		// Extract the Firehose Request ID and set the logger to add to every log message
		XAmzFirehoseRequestID := c.Request.Header.Get("X-Amz-Firehose-Request-Id")
		if XAmzFirehoseRequestID != "" {
			zaplog = suggar.With("X-Amz-Firehose-Request-Id", XAmzFirehoseRequestID)
		} else {
			zaplog = suggar.With("X-Amz-Firehose-Request-Id", "manual")
			zaplog.Infof("X-Amz-Firehose-Request-Id header is empty, treating as manual request")
		}
		zaplog.Infof("New Request")

		// detect if the request is in a supported format
		contentType, err := dataDetect(c, zaplog)
		if errors.Is(err, errEncodingMismatch) {
			zaplog.Warnf("treating body as gzip(ed) json")
			contentType = MIMEGZIP
		} else if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// switch on detected content type, ignoring the header and
		// unmarshal the request body to incomingFirehose type
		zaplog.Debugw("Content type decided", "contentType", contentType)
		switch contentType {
		case MIMEGZIP:
			if err = c.ShouldBindBodyWith(&dataIncoming, gzipbinding.GzipJSONBinding{}); err != nil {
				zaplog.Errorw("Error parsing GZIP JSON request body", "error", err)
				c.AbortWithStatusJSON(http.StatusBadRequest, firehoseErrorBody{
					RequestID: XAmzFirehoseRequestID,
					Timestamp: time.Now().UTC().UnixMilli(),
					Error:     err.Error(),
				})
				return
			}
		case "text/plain":
			if err = c.ShouldBindJSON(&dataIncoming); err != nil {
				zaplog.Errorw("Error parsing JSON request body", "error", err)
				c.AbortWithStatusJSON(http.StatusBadRequest, firehoseErrorBody{
					RequestID: XAmzFirehoseRequestID,
					Timestamp: time.Now().UTC().UnixMilli(),
					Error:     err.Error(),
				})
				return
			}
		default:
			zaplog.Errorw("unsupported Content-Encoding", "Content-Encoding", contentType)
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "unsupported Content-Encoding"})
			return
		}

		// iterate over all the records to split them into separate documents
		documents, err := splitRecords(dataIncoming, XAmzFirehoseRequestID, zaplog)
		if err != nil {
			zaplog.Errorw("Failed splitting records", "err", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, firehoseErrorBody{
				RequestID: XAmzFirehoseRequestID,
				Timestamp: time.Now().UTC().UnixMilli(),
				Error:     err.Error(),
			})
			return
		}

		// authHeader here is then used for auth againt es
		// it's supposed to be base64 of username:password
		authHeader := c.Request.Header.Get(incomingAuthHeader)

		bi, err := getBulkIndexer(c, opensearchURL, openSearchPipeline, authHeader, zapConfig, outputIndex, XAmzFirehoseRequestID, zaplog)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, firehoseErrorBody{
				RequestID: XAmzFirehoseRequestID,
				Timestamp: time.Now().UTC().UnixMilli(),
				Error:     err.Error(),
			})
			return
		}
		// This closes the bulk indexer, essentially flushing the data to es

		err = queueBulkIndex(documents, countSuccessful, bi, zaplog)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, firehoseErrorBody{
				RequestID: XAmzFirehoseRequestID,
				Timestamp: time.Now().UTC().UnixMilli(),
				Error:     err.Error(),
			})
			return
		}
		// now that the indexer is filled with the documents, we can close it
		zaplog.Debugf("Closing bulk indexer")
		err = bi.Close(context.Background())
		if err != nil {
			zaplog.Errorw("Failed to close bulk indexer", "err", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, firehoseErrorBody{
				RequestID: XAmzFirehoseRequestID,
				Timestamp: time.Now().UTC().UnixMilli(),
				Error:     err.Error(),
			})
			return
		}

		if !c.IsAborted() {
			c.JSON(http.StatusOK, firehoseSuccessResponse{
				RequestID: dataIncoming.RequestID,
				Timestamp: dataIncoming.Timestamp,
			})
			return
		}
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 2 * time.Second,
	}
	// Initializing the server in a goroutine so that
	// it won't block the graceful shutdown handling below
	go func() {
		suggar.Infof("Listening and serving HTTP on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			suggar.Fatalw("Gin startup Failed", "err", err)
		}
	}()

	// Listen for the interrupt signal.
	<-ctx.Done()
	// Restore default behavior on the interrupt signal and notify user of shutdown.
	stop()
	suggar.Infof("shutting down gracefully, waiting %s, press Ctrl+C again to force", shutdownTimeout)

	// The context is used to inform the server it has 30 seconds to finish
	// the request it is currently handling
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		suggar.Fatal("Server forced to shutdown: ", err)
	}

	suggar.Info("Server exiting")
}

// getBulkIndexer returns a new opensearch client and BulkIndexer
func getBulkIndexer(c *gin.Context, opensearchURL string, opensearchPipeline string, authHeader string, zapConfig zap.Config, outputIndex string, XAmzFirehoseRequestID string, logger *zap.SugaredLogger) (opensearchutil.BulkIndexer, error) {
	// exponential backoff to prevent overloading opensearch
	retryBackoff := backoff.NewExponentialBackOff()
	cfg := opensearch.Config{
		RetryBackoff: func(i int) time.Duration {
			if i == 1 {
				retryBackoff.Reset()
			}
			return retryBackoff.NextBackOff()
		},
		Logger:        &opensearchtransport.JSONLogger{Output: os.Stdout}, // EnableResponseBody: true, // EnableRequestBody:  true,
		Addresses:     []string{opensearchURL},
		RetryOnStatus: []int{502, 503, 504, 429},
		// the authHeader extracted above is passed on as it is for auth basic
		Header: http.Header(map[string][]string{"Authorization": {"Basic " + authHeader}}),
	}
	// more colourful logs when in debug mode
	if zapConfig.Level.Level() == zapcore.DebugLevel {
		cfg.EnableDebugLogger = true
		cfg.Logger = &opensearchtransport.ColorLogger{Output: os.Stdout}
	}

	// The only issue here is that the opensearch client is created on every api call, I don't know how bad that is
	// This is currently necessary to extract the auth header from the request and pass it on to ES
	client, err := opensearch.NewClient(cfg)
	if err != nil {
		logger.Fatalw("Failed creating client client",
			"err", err,
		)
	}
	// create a new bulk Indexer
	bi, err := opensearchutil.NewBulkIndexer(opensearchutil.BulkIndexerConfig{
		Index:         outputIndex,
		Client:        client,
		NumWorkers:    numWorkers,         // The number of worker goroutines TODO: make a flag
		FlushBytes:    flushBytes,         // The flush threshold in bytes  TODO: make a flag
		FlushInterval: 5 * time.Second,    // The periodic flush interval TODO: make a flag
		Pipeline:      opensearchPipeline, // The pipeline to use for the indexing
	})
	if err != nil {
		// logger.Fatalw("Failed creating BulkIndexer",
		// 	"err", err,
		// )
		c.AbortWithStatusJSON(http.StatusInternalServerError, firehoseErrorBody{
			RequestID: XAmzFirehoseRequestID,
			Timestamp: time.Now().UTC().UnixMilli(),
			Error:     err.Error(),
		})
	}

	return bi, err
}

// splitRecords splits the incoming record into a slice of document
//
//nolint:funlen
func splitRecords(dataIncoming incoming.FirehoseRequest, xAmzFirehoseRequestID string, logger *zap.SugaredLogger) ([]outgoing.Document, error) {
	var documents []outgoing.Document

	// iterate over all the records to split them into separate documents
	for _, record := range dataIncoming.Records {
		data := incoming.Data{}
		// first decode the incoming data
		decodedCompressedRecord, err := base64.StdEncoding.DecodeString(record.Data)
		if err != nil {
			logger.Errorw("Failed base64 decoding compressed record",
				"err", err,
				"record", record,
			)
			return []outgoing.Document{}, err
		}
		// then decompress the data
		uncompressedRecord, err := gzip.NewReader(bytes.NewReader(decodedCompressedRecord))
		if err != nil {
			logger.Errorw("Failed to uncompress record",
				"err", err,
				"record", record.Data,
			)
			return []outgoing.Document{}, err
		}
		// then try to unmarshall the uncompressed data
		err = json.NewDecoder(uncompressedRecord).Decode(&data)
		if err != nil {
			logger.Errorw("Failed to decode record",
				"err", err,
				"record", uncompressedRecord,
			)
			return []outgoing.Document{}, err
		}
		logger.Debugw("Decoded record from kinesis", "record", data)

		// now loop over the log events and create a document for each
		for _, logEvent := range data.LogEvents {
			// used a json.rawMessage as the event can have any json structure
			cwEvent := json.RawMessage{}
			err = json.Unmarshal([]byte(logEvent.Message), &cwEvent)
			if err != nil {
				// if the event is not a json structure, just can save it as string
				logger.Debugw("Not a json LogEvent")
				cwEvent, err = json.Marshal(struct {
					Text string `json:"text"`
				}{
					Text: logEvent.Message},
				)
				if err != nil {
					logger.Errorw("Failed to marshal stringLog", "err", err, "stringLog", logEvent.Message)
					return []outgoing.Document{}, err
				}
				// cwEvent = json.RawMessage([]byte())
				// return []outgoing.Document{}, err
			}
			// Now we finally have a valid CloudWatch event
			// create a new document
			event := outgoing.Document{
				RequestID: xAmzFirehoseRequestID,
				TimeStamp: time.UnixMilli(dataIncoming.Timestamp).UTC(),
				Record: outgoing.Record{
					Data: outgoing.Data{
						MessageType:         data.MessageType,
						Owner:               data.Owner,
						LogGroup:            data.LogGroup,
						LogStream:           data.LogStream,
						SubscriptionFilters: data.SubscriptionFilters,
						LogEvent: outgoing.LogEvent{
							// make sure to use logEvent.Timestamp as the logevent timestamp is usually different from the kinesis request timestamp, as firehose batches and buffers the records
							ID:        logEvent.ID,
							Timestamp: time.UnixMilli(logEvent.Timestamp).UTC(),
							Message:   cwEvent,
						},
					},
				},
			}
			// Make sure the document is valid
			if _, err = json.Marshal(record); err != nil {
				logger.Errorw("Failed to marshal record to Json", "err", err)
				return []outgoing.Document{}, err
			}
			// add the document to the list of documents to send to bulkIndexer in one go
			documents = append(documents, event)
		}
	}

	return documents, nil
}

// queueBulkIndex adds the documents to the getBulkIndexer
func queueBulkIndex(documents []outgoing.Document, countSuccessful uint64, bi opensearchutil.BulkIndexer, logger *zap.SugaredLogger) error {
	// loop over the documents and add them to the bulk indexer
	for _, d := range documents {
		documentByte, err := json.Marshal(d)
		if err != nil {
			logger.Errorw("Failed to marshal document to Json", "err", err)
			return err
		}

		item := opensearchutil.BulkIndexerItem{
			// Action field configures the operation to perform (index, create, delete, update)
			Action: "index",
			// Body is an `io.Reader` with the payload
			Body: bytes.NewReader(documentByte),

			// OnSuccess is called for each successful operation
			OnSuccess: func(ctx context.Context, item opensearchutil.BulkIndexerItem, res opensearchutil.BulkIndexerResponseItem) {
				atomic.AddUint64(&countSuccessful, 1)
			},
			// OnFailure is called for each failed operation
			OnFailure: func(ctx context.Context, item opensearchutil.BulkIndexerItem, res opensearchutil.BulkIndexerResponseItem, err error) {
				if err != nil {
					logger.Errorw("Failed BulkIndex", "err", err)
				} else {
					logger.Errorw("Failed BulkIndex", "type", res.Error.Type, "err", res.Error.Reason)
				}
			},
		}

		if err = bi.Add(context.Background(), item); err != nil {
			logger.Errorw("Failed to add to bulk indexer", "err", err)
			return err
		}

		logger.Debugf("added to bulk indexer")
	}

	return nil
}

// func read(r io.Reader) string {
//	var b bytes.Buffer
//	_, _ = b.ReadFrom(r)
//
//	return b.String()
// }
