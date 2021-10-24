package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Depado/ginprom"
	"github.com/cenkalti/backoff/v4"
	"github.com/elastic/go-elasticsearch/v7"
	"github.com/elastic/go-elasticsearch/v7/estransport"
	"github.com/elastic/go-elasticsearch/v7/esutil"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/jnovack/flag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Moulick/Kinesis2Elastic/gzipbinding"
	"github.com/Moulick/Kinesis2Elastic/log"
)

const (
	numWorkers         = 3
	flushBytes         = 5000000
	incomingAuthHeader = "X-Amz-Firehose-Access-Key"
	shutdownTimeout    = 30 * time.Second
)

var (
	encodingMismatch = errors.New("data encoding mismatch")
	MIMEGZIP         = "application/x-gzip"
)

// firehoseSuccessResponse is the reponse sent back to AWS Kinesis Firehose on success
// https://docs.aws.amazon.com/firehose/latest/dev/httpdeliveryrequestresponse.html
type firehoseSuccessResponse struct {
	RequestId string `json:"requestId"`
	Timestamp int64  `json:"timestamp"`
}

// firehoseErrorBody is the reponse sent back to AWS Kinesis Firehose on error
// https://docs.aws.amazon.com/firehose/latest/dev/httpdeliveryrequestresponse.html
type firehoseErrorBody struct {
	RequestId string `json:"requestId"`
	Timestamp int64  `json:"timestamp"`
	Error     string `json:"errorMessage"`
}

// incomingFirehose is the data that firehose sends for HTTP Endpoint Destination
// Timestamp is epoch in milliseconds
// https://docs.aws.amazon.com/firehose/latest/dev/httpdeliveryrequestresponse.html
type incomingFirehose struct {
	RequestID string   `binding:"required" json:"requestId"`
	Timestamp int64    `binding:"required" json:"timestamp"`
	Records   []record `binding:"required" json:"records"`
}

// record is one record in the request
// Data is a base64 encoded string
type record struct {
	Data string `json:"data"`
}

// document is the body sent to ElasticSearch
type document struct {
	TimeStamp time.Time       `json:"@timestamp"`
	Log       json.RawMessage `json:"log,inline"`
	RequestID string          `json:"requestId"`
}

// dataDetect detects the content type and encoding of the request body.
// returns error for unsupported Content-Encoding and Content-Type if not supported or mismatch in header vs body
// return nil and detected content type if success
func dataDetect(c *gin.Context, logger *zap.SugaredLogger) (error, string) {
	// extract header for incoming request
	contentType := c.ContentType()
	logger.Debugw("Content-Type", "Content-Type", contentType)
	// is the content type supported?
	if contentType != "application/json" {
		return fmt.Errorf("unsupported Content-Type: %s", contentType), contentType
	}

	contentEncodingHeader := c.GetHeader("Content-Encoding")
	logger.Debugw("Content-Encoding", "Content-Encoding", contentEncodingHeader)
	// is content encoding supported?
	if contentEncodingHeader != "" {
		if contentEncodingHeader != "gzip" {
			return fmt.Errorf("unsupported Content-Encoding %s", contentEncodingHeader), ""
		}
	}

	// Duplicate the body, as like when we do detection, it closes the body
	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		return err, ""
	}
	// Restore the request body to its original state
	c.Request.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	// convert body to bytes
	reqBodyBytes, err := io.ReadAll(ioutil.NopCloser(bytes.NewBuffer(body)))
	if err != nil {
		return err, ""
	}
	// check if body is gzip compressed
	realContentEncoding := http.DetectContentType(reqBodyBytes)

	// log if the header and content type are not the same
	if realContentEncoding == MIMEGZIP {
		if contentEncodingHeader != "gzip" {
			logger.Warnw("detected data encoding mismatch, incoming Content-Encoding header not set properly", "expected", "gzip", "received", realContentEncoding)
			return encodingMismatch, ""
		}
	} else if strings.Contains(realContentEncoding, "text/plain") {
		return nil, "text/plain"
	} else {
		return fmt.Errorf("unsupported data encoding %s", realContentEncoding), ""
	}
	// all checkes passed, return the content type
	return nil, realContentEncoding
}

func main() {
	fmt.Println("Starting Kinesis2Elastic")
	// Create context that listens for the interrupt signal from the OS.
	// kill (no param) default send syscall.SIGTERM
	// kill -2 is syscall.SIGINT
	// kill -9 is syscall.SIGKILL but can't be catch, so don't need add it
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var (
		elasticURL      string
		outputIndex     string
		countSuccessful uint64
		port            string
	)

	flag.StringVar(&elasticURL, "elastic_url", "http://localhost:9200", "URL of the ElasticSearch Cluster with schema and optional Port")
	flag.StringVar(&outputIndex, "output_index", "firehose-output", "output_index name to create documents, output_index will be created if does not exist")
	flag.StringVar(&port, "port", "8080", "port to listen on")
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
	suggar.Infow("ElasticSearch URL is ", "URL", elasticURL)
	suggar.Infow("ElasticSearch Output Index", "index", outputIndex)
	suggar.Infow("ElasticSearch worker threads", "workers", numWorkers)

	// suggar.Debug("DEBUG LOG")
	// suggar.Info("INFO LOG")
	// suggar.Warn("WARN LOG")
	// suggar.Error("ERROR LOG")
	// suggar.DPanic("PANIC LOG")
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
		It takes the incoming data, breaks up the records into individual ElasticSearch Documents
		It reuses the same X-Amz-Firehose-Request-Id for all the documents which were part of the same request
		It also converts the timestamp to @timestamp that elasticSearch can understand
		It passes on the X-Amz-Firehose-Access-Key from the request to ElasticSearch basic auth,
			the X-Amz-Firehose-Access-Key is expected to be a base64 encoded username:password
		It uses the bulk output_index api of elasticSearch for best performance
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
			dataIncoming incomingFirehose
		)

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
		err, contentType := dataDetect(c, zaplog)
		if errors.Is(err, encodingMismatch) {
			zaplog.Warnf("treating body as gzip(ed) json")
			contentType = MIMEGZIP
		} else if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// switch on detected content type, ignoring the header and
		// unmarshal the request body to incomingFirehose type
		switch contentType {
		case MIMEGZIP:
			if err = c.ShouldBindBodyWith(&dataIncoming, gzipbinding.GzipJSONBinding{}); err != nil {
				zaplog.Errorw("Error parsing GZIP JSON request body", "error", err)
				c.AbortWithStatusJSON(http.StatusBadRequest, firehoseErrorBody{
					RequestId: XAmzFirehoseRequestID,
					Timestamp: time.Now().UTC().UnixMilli(),
					Error:     err.Error(),
				})
				return
			}
		case "text/plain":
			if err = c.ShouldBindJSON(&dataIncoming); err != nil {
				zaplog.Errorw("Error parsing JSON request body", "error", err)
				c.AbortWithStatusJSON(http.StatusBadRequest, firehoseErrorBody{
					RequestId: XAmzFirehoseRequestID,
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
				RequestId: XAmzFirehoseRequestID,
				Timestamp: time.Now().UTC().UnixMilli(),
				Error:     err.Error(),
			})
			return
		}

		// authHeader here is then used for auth againt es
		// it's supposed to be base64 of username:password
		authHeader := c.Request.Header.Get(incomingAuthHeader)

		bi, err := getBulkIndexer(c, elasticURL, authHeader, zapConfig, outputIndex, XAmzFirehoseRequestID, zaplog)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, firehoseErrorBody{
				RequestId: XAmzFirehoseRequestID,
				Timestamp: time.Now().UTC().UnixMilli(),
				Error:     err.Error(),
			})
			return
		}
		// This closes the bulk indexer, essentially flushing the data to es

		err = queueBulkIndex(documents, countSuccessful, bi, zaplog)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, firehoseErrorBody{
				RequestId: XAmzFirehoseRequestID,
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
				RequestId: XAmzFirehoseRequestID,
				Timestamp: time.Now().UTC().UnixMilli(),
				Error:     err.Error(),
			})
			return
		}

		if !c.IsAborted() {
			c.JSON(http.StatusOK, firehoseSuccessResponse{
				RequestId: dataIncoming.RequestID,
				Timestamp: dataIncoming.Timestamp,
			})
			return
		}

	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
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

// getBulkIndexer returns a new elasticSearch client and BulkIndexer
func getBulkIndexer(c *gin.Context, elasticURL string, authHeader string, zapConfig zap.Config, outputIndex string, XAmzFirehoseRequestID string, logger *zap.SugaredLogger) (esutil.BulkIndexer, error) {
	// exponential backoff to prevent overloading elasticsearch
	retryBackoff := backoff.NewExponentialBackOff()
	cfg := elasticsearch.Config{
		RetryBackoff: func(i int) time.Duration {
			if i == 1 {
				retryBackoff.Reset()
			}
			return retryBackoff.NextBackOff()
		},
		Logger:        &estransport.JSONLogger{Output: os.Stdout}, // EnableResponseBody: true, // EnableRequestBody:  true,
		Addresses:     []string{elasticURL},
		RetryOnStatus: []int{502, 503, 504, 429},
		// the authHeader extracted above is passed on as it is for auth basic
		Header: http.Header(map[string][]string{"Authorization": {"Basic " + authHeader}}),
	}
	// more colourful logs when in debug mode
	if zapConfig.Level.Level() == zapcore.DebugLevel {
		cfg.EnableDebugLogger = true
		cfg.Logger = &estransport.ColorLogger{Output: os.Stdout}
	}

	// The only issue here is that the es client is created on every api call, I don't know how bad that is
	// This is currently necessary to extract the auth header from the request and pass it on to ES
	es, err := elasticsearch.NewClient(cfg)
	if err != nil {
		logger.Fatalw("Failed creating es client",
			"err", err,
		)
	}
	// create a new bulk Indexer
	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Index:         outputIndex,
		Client:        es,
		NumWorkers:    numWorkers,      // The number of worker goroutines TODO: make a flag
		FlushBytes:    flushBytes,      // The flush threshold in bytes  TODO: make a flag
		FlushInterval: 5 * time.Second, // The periodic flush interval TODO: make a flag
	})
	if err != nil {
		// logger.Fatalw("Failed creating BulkIndexer",
		// 	"err", err,
		// )
		c.AbortWithStatusJSON(http.StatusInternalServerError, firehoseErrorBody{
			RequestId: XAmzFirehoseRequestID,
			Timestamp: time.Now().UTC().UnixMilli(),
			Error:     err.Error(),
		})
	}

	return bi, err
}

// splitRecords splits the incoming record into a slice of document
func splitRecords(dataIncoming incomingFirehose, XAmzFirehoseRequestID string, logger *zap.SugaredLogger) ([]document, error) {
	var documents []document

	// iterate over all the records to split them into separate documents
	for _, i := range dataIncoming.Records {
		decodedRecord, err := base64.StdEncoding.DecodeString(i.Data)
		if err != nil {
			logger.Fatalw("Failed decoding record data", "err", err)
			return []document{}, err
		}
		// logger.Debugw("decoded record from kinesis", "Record", decodedRecord)

		event := document{
			TimeStamp: time.UnixMilli(dataIncoming.Timestamp).UTC(),
			Log:       decodedRecord,
			RequestID: XAmzFirehoseRequestID,
		}
		// Make sure the document is valid
		if _, err = json.Marshal(i); err != nil {
			logger.Errorw("Failed to marshal record to Json", "err", err)
			return []document{}, err
		}

		documents = append(documents, event)
	}

	return documents, nil
}

// queueBulkIndex adds the documents to the getBulkIndexer
func queueBulkIndex(documents []document, countSuccessful uint64, bi esutil.BulkIndexer, logger *zap.SugaredLogger) error {
	// loop over the documents and add them to the bulk indexer
	for _, d := range documents {
		documentByte, err := json.Marshal(d)
		if err != nil {
			logger.Errorw("Failed to marshal document to Json", "err", err)
			return err
		}

		item := esutil.BulkIndexerItem{
			// Action field configures the operation to perform (index, create, delete, update)
			Action: "index",
			// Body is an `io.Reader` with the payload
			Body: bytes.NewReader(documentByte),

			// OnSuccess is called for each successful operation
			OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem) {
				atomic.AddUint64(&countSuccessful, 1)
			},
			// OnFailure is called for each failed operation
			OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem, err error) {
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

func read(r io.Reader) string {
	var b bytes.Buffer
	_, _ = b.ReadFrom(r)

	return b.String()
}
