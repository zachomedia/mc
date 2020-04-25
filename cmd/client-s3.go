/*
 * MinIO Client (C) 2015-2020 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/minio/mc/pkg/httptracer"
	"github.com/minio/mc/pkg/probe"
	minio "github.com/minio/minio-go/v6"
	"github.com/minio/minio-go/v6/pkg/credentials"
	"github.com/minio/minio-go/v6/pkg/encrypt"
	"github.com/minio/minio-go/v6/pkg/policy"
	"github.com/minio/minio-go/v6/pkg/s3utils"
	"github.com/minio/minio/pkg/bucket/object/tagging"
	"github.com/minio/minio/pkg/mimedb"
)

// S3Client construct
type S3Client struct {
	sync.Mutex
	targetURL    *ClientURL
	api          *minio.Client
	virtualStyle bool
}

const (
	amazonHostNameAccelerated = "s3-accelerate.amazonaws.com"
	googleHostName            = "storage.googleapis.com"
	serverEncryptionKeyPrefix = "x-amz-server-side-encryption"

	defaultRecordDelimiter = "\n"
	defaultFieldDelimiter  = ","
)

const (
	recordDelimiterType      = "recorddelimiter"
	fieldDelimiterType       = "fielddelimiter"
	quoteCharacterType       = "quotechar"
	quoteEscapeCharacterType = "quoteescchar"
	quoteFieldType           = "quotefield"
	fileHeaderType           = "fileheader"
	commentCharType          = "commentchar"
	typeJSONType             = "type"
	// AmzObjectLockMode sets object lock mode
	AmzObjectLockMode = "X-Amz-Object-Lock-Mode"
	// AmzObjectLockRetainUntilDate sets object lock retain until date
	AmzObjectLockRetainUntilDate = "X-Amz-Object-Lock-Retain-Until-Date"
	// AmzObjectLockLegalHold sets object lock legal hold
	AmzObjectLockLegalHold = "X-Amz-Object-Lock-Legal-Hold"
)

var timeSentinel = time.Unix(0, 0).UTC()

// newFactory encloses New function with client cache.
func newFactory() func(config *Config) (Client, *probe.Error) {
	clientCache := make(map[uint32]*minio.Client)
	var mutex sync.Mutex

	// Return New function.
	return func(config *Config) (Client, *probe.Error) {
		// Creates a parsed URL.
		targetURL := newClientURL(config.HostURL)
		// By default enable HTTPs.
		useTLS := true
		if targetURL.Scheme == "http" {
			useTLS = false
		}

		// Instantiate s3
		s3Clnt := &S3Client{}
		// Save the target URL.
		s3Clnt.targetURL = targetURL

		// Save if target supports virtual host style.
		hostName := targetURL.Host
		s3Clnt.virtualStyle = isVirtualHostStyle(hostName, config.Lookup)
		isS3AcceleratedEndpoint := isAmazonAccelerated(hostName)

		if s3Clnt.virtualStyle {
			// If Google URL replace it with 'storage.googleapis.com'
			if isGoogle(hostName) {
				hostName = googleHostName
			}
		}
		// Generate a hash out of s3Conf.
		confHash := fnv.New32a()
		confHash.Write([]byte(hostName + config.AccessKey + config.SecretKey))
		confSum := confHash.Sum32()

		// Lookup previous cache by hash.
		mutex.Lock()
		defer mutex.Unlock()
		var api *minio.Client
		var found bool
		if api, found = clientCache[confSum]; !found {
			// if Signature version '4' use NewV4 directly.
			creds := credentials.NewStaticV4(config.AccessKey, config.SecretKey, config.SessionToken)
			// if Signature version '2' use NewV2 directly.
			if strings.ToUpper(config.Signature) == "S3V2" {
				creds = credentials.NewStaticV2(config.AccessKey, config.SecretKey, config.SessionToken)
			}
			// Not found. Instantiate a new MinIO
			var e error

			options := minio.Options{
				Creds:        creds,
				Secure:       useTLS,
				Region:       "",
				BucketLookup: config.Lookup,
			}

			api, e = minio.NewWithOptions(hostName, &options)
			if e != nil {
				return nil, probe.NewError(e)
			}

			tr := &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 15 * time.Second,
				}).DialContext,
				MaxIdleConnsPerHost:   256,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 10 * time.Second,
				// Set this value so that the underlying transport round-tripper
				// doesn't try to auto decode the body of objects with
				// content-encoding set to `gzip`.
				//
				// Refer:
				//    https://golang.org/src/net/http/transport.go?h=roundTrip#L1843
				DisableCompression: true,
			}

			if useTLS {
				// Keep TLS config.
				tlsConfig := &tls.Config{
					RootCAs: globalRootCAs,
					// Can't use SSLv3 because of POODLE and BEAST
					// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
					// Can't use TLSv1.1 because of RC4 cipher usage
					MinVersion: tls.VersionTLS12,
				}
				if config.Insecure {
					tlsConfig.InsecureSkipVerify = true
				}
				tr.TLSClientConfig = tlsConfig

				// Because we create a custom TLSClientConfig, we have to opt-in to HTTP/2.
				// See https://github.com/golang/go/issues/14275
				//
				// TODO: Enable http2.0 when upstream issues related to HTTP/2 are fixed.
				//
				// if e = http2.ConfigureTransport(tr); e != nil {
				// 	return nil, probe.NewError(e)
				// }
			}

			var transport http.RoundTripper = tr
			if config.Debug {
				if strings.EqualFold(config.Signature, "S3v4") {
					transport = httptracer.GetNewTraceTransport(newTraceV4(), transport)
				} else if strings.EqualFold(config.Signature, "S3v2") {
					transport = httptracer.GetNewTraceTransport(newTraceV2(), transport)
				}
			}

			// Set the new transport.
			api.SetCustomTransport(transport)

			// If Amazon Accelerated URL is requested enable it.
			if isS3AcceleratedEndpoint {
				api.SetS3TransferAccelerate(amazonHostNameAccelerated)
			}

			// Set app info.
			api.SetAppInfo(config.AppName, config.AppVersion)

			// Cache the new MinIO Client with hash of config as key.
			clientCache[confSum] = api
		}

		// Store the new api object.
		s3Clnt.api = api

		return s3Clnt, nil
	}
}

// S3New returns an initialized S3Client structure. If debug is enabled,
// it also enables an internal trace transport.
var S3New = newFactory()

// GetURL get url.
func (c *S3Client) GetURL() ClientURL {
	return *c.targetURL
}

// AddNotificationConfig - Add bucket notification
func (c *S3Client) AddNotificationConfig(arn string, events []string, prefix, suffix string, ignoreExisting bool) *probe.Error {
	bucket, _ := c.url2BucketAndObject()
	// Validate total fields in ARN.
	fields := strings.Split(arn, ":")
	if len(fields) != 6 {
		return errInvalidArgument()
	}

	// Get any enabled notification.
	mb, e := c.api.GetBucketNotification(bucket)
	if e != nil {
		return probe.NewError(e)
	}

	accountArn := minio.NewArn(fields[1], fields[2], fields[3], fields[4], fields[5])
	nc := minio.NewNotificationConfig(accountArn)

	// Configure events
	for _, event := range events {
		switch event {
		case "put":
			nc.AddEvents(minio.ObjectCreatedAll)
		case "delete":
			nc.AddEvents(minio.ObjectRemovedAll)
		case "get":
			nc.AddEvents(minio.ObjectAccessedAll)
		default:
			return errInvalidArgument().Trace(events...)
		}
	}
	if prefix != "" {
		nc.AddFilterPrefix(prefix)
	}
	if suffix != "" {
		nc.AddFilterSuffix(suffix)
	}

	switch fields[2] {
	case "sns":
		if !mb.AddTopic(nc) {
			return errInvalidArgument().Trace("Overlapping Topic configs")
		}
	case "sqs":
		if !mb.AddQueue(nc) {
			return errInvalidArgument().Trace("Overlapping Queue configs")
		}
	case "lambda":
		if !mb.AddLambda(nc) {
			return errInvalidArgument().Trace("Overlapping lambda configs")
		}
	default:
		return errInvalidArgument().Trace(fields[2])
	}

	// Set the new bucket configuration
	if err := c.api.SetBucketNotification(bucket, mb); err != nil {
		if ignoreExisting && strings.Contains(err.Error(), "An object key name filtering rule defined with overlapping prefixes, overlapping suffixes, or overlapping combinations of prefixes and suffixes for the same event types") {
			return nil
		}
		return probe.NewError(err)
	}
	return nil
}

// RemoveNotificationConfig - Remove bucket notification
func (c *S3Client) RemoveNotificationConfig(arn string, event string, prefix string, suffix string) *probe.Error {
	bucket, _ := c.url2BucketAndObject()
	// Remove all notification configs if arn is empty
	if arn == "" {
		if err := c.api.RemoveAllBucketNotification(bucket); err != nil {
			return probe.NewError(err)
		}
		return nil
	}

	mb, e := c.api.GetBucketNotification(bucket)
	if e != nil {
		return probe.NewError(e)
	}

	fields := strings.Split(arn, ":")
	if len(fields) != 6 {
		return errInvalidArgument().Trace(fields...)
	}
	accountArn := minio.NewArn(fields[1], fields[2], fields[3], fields[4], fields[5])

	// if we are passed filters for either events, suffix or prefix, then only delete the single event that matches
	// the arguments
	if event != "" || suffix != "" || prefix != "" {
		// Translate events to type events for comparison
		events := strings.Split(event, ",")
		var eventsTyped []minio.NotificationEventType
		for _, e := range events {
			switch e {
			case "put":
				eventsTyped = append(eventsTyped, minio.ObjectCreatedAll)
			case "delete":
				eventsTyped = append(eventsTyped, minio.ObjectRemovedAll)
			case "get":
				eventsTyped = append(eventsTyped, minio.ObjectAccessedAll)
			default:
				return errInvalidArgument().Trace(events...)
			}
		}
		var err error
		// based on the arn type, we'll look for the event in the corresponding sublist and delete it if there's a match
		switch fields[2] {
		case "sns":
			err = mb.RemoveTopicByArnEventsPrefixSuffix(accountArn, eventsTyped, prefix, suffix)
		case "sqs":
			err = mb.RemoveQueueByArnEventsPrefixSuffix(accountArn, eventsTyped, prefix, suffix)
		case "lambda":
			err = mb.RemoveLambdaByArnEventsPrefixSuffix(accountArn, eventsTyped, prefix, suffix)
		default:
			return errInvalidArgument().Trace(fields[2])
		}
		if err != nil {
			return probe.NewError(err)
		}

	} else {
		// remove all events for matching arn
		switch fields[2] {
		case "sns":
			mb.RemoveTopicByArn(accountArn)
		case "sqs":
			mb.RemoveQueueByArn(accountArn)
		case "lambda":
			mb.RemoveLambdaByArn(accountArn)
		default:
			return errInvalidArgument().Trace(fields[2])
		}
	}

	// Set the new bucket configuration
	if e := c.api.SetBucketNotification(bucket, mb); e != nil {
		return probe.NewError(e)
	}
	return nil
}

// NotificationConfig notification config
type NotificationConfig struct {
	ID     string   `json:"id"`
	Arn    string   `json:"arn"`
	Events []string `json:"events"`
	Prefix string   `json:"prefix"`
	Suffix string   `json:"suffix"`
}

// ListNotificationConfigs - List notification configs
func (c *S3Client) ListNotificationConfigs(arn string) ([]NotificationConfig, *probe.Error) {
	var configs []NotificationConfig
	bucket, _ := c.url2BucketAndObject()
	mb, e := c.api.GetBucketNotification(bucket)
	if e != nil {
		return nil, probe.NewError(e)
	}

	// Generate pretty event names from event types
	prettyEventNames := func(eventsTypes []minio.NotificationEventType) []string {
		var result []string
		for _, eventType := range eventsTypes {
			result = append(result, string(eventType))
		}
		return result
	}

	getFilters := func(config minio.NotificationConfig) (prefix, suffix string) {
		if config.Filter == nil {
			return
		}
		for _, filter := range config.Filter.S3Key.FilterRules {
			if strings.ToLower(filter.Name) == "prefix" {
				prefix = filter.Value
			}
			if strings.ToLower(filter.Name) == "suffix" {
				suffix = filter.Value
			}

		}
		return prefix, suffix
	}

	for _, config := range mb.TopicConfigs {
		if arn != "" && config.Topic != arn {
			continue
		}
		prefix, suffix := getFilters(config.NotificationConfig)
		configs = append(configs, NotificationConfig{ID: config.ID,
			Arn:    config.Topic,
			Events: prettyEventNames(config.Events),
			Prefix: prefix,
			Suffix: suffix})
	}

	for _, config := range mb.QueueConfigs {
		if arn != "" && config.Queue != arn {
			continue
		}
		prefix, suffix := getFilters(config.NotificationConfig)
		configs = append(configs, NotificationConfig{ID: config.ID,
			Arn:    config.Queue,
			Events: prettyEventNames(config.Events),
			Prefix: prefix,
			Suffix: suffix})
	}

	for _, config := range mb.LambdaConfigs {
		if arn != "" && config.Lambda != arn {
			continue
		}
		prefix, suffix := getFilters(config.NotificationConfig)
		configs = append(configs, NotificationConfig{ID: config.ID,
			Arn:    config.Lambda,
			Events: prettyEventNames(config.Events),
			Prefix: prefix,
			Suffix: suffix})
	}

	return configs, nil
}

// Supported content types
var supportedContentTypes = []string{
	"csv",
	"json",
	"gzip",
	"bzip2",
}

// set the SelectObjectOutputSerialization struct using options passed in by client. If unspecified,
// default S3 API specified defaults
func selectObjectOutputOpts(selOpts SelectObjectOpts, i minio.SelectObjectInputSerialization) minio.SelectObjectOutputSerialization {
	var isOK bool
	var recDelim, fldDelim, quoteChar, quoteEscChar, qf string

	o := minio.SelectObjectOutputSerialization{}
	if _, ok := selOpts.OutputSerOpts["json"]; ok {
		recDelim, isOK = selOpts.OutputSerOpts["json"][recordDelimiterType]
		if !isOK {
			recDelim = "\n"
		}
		o.JSON = &minio.JSONOutputOptions{RecordDelimiter: recDelim}
	}
	if _, ok := selOpts.OutputSerOpts["csv"]; ok {
		o.CSV = &minio.CSVOutputOptions{RecordDelimiter: defaultRecordDelimiter, FieldDelimiter: defaultFieldDelimiter}
		if recDelim, isOK = selOpts.OutputSerOpts["csv"][recordDelimiterType]; isOK {
			o.CSV.RecordDelimiter = recDelim
		}

		if fldDelim, isOK = selOpts.OutputSerOpts["csv"][fieldDelimiterType]; isOK {
			o.CSV.FieldDelimiter = fldDelim
		}
		if quoteChar, isOK = selOpts.OutputSerOpts["csv"][quoteCharacterType]; isOK {
			o.CSV.QuoteCharacter = quoteChar
		}

		if quoteEscChar, isOK = selOpts.OutputSerOpts["csv"][quoteEscapeCharacterType]; isOK {
			o.CSV.QuoteEscapeCharacter = quoteEscChar
		}
		if qf, isOK = selOpts.OutputSerOpts["csv"][quoteFieldType]; isOK {
			o.CSV.QuoteFields = minio.CSVQuoteFields(qf)
		}
	}
	// default to CSV output if options left unspecified
	if o.CSV == nil && o.JSON == nil {
		if i.JSON != nil {
			o.JSON = &minio.JSONOutputOptions{RecordDelimiter: "\n"}
		} else {
			o.CSV = &minio.CSVOutputOptions{RecordDelimiter: defaultRecordDelimiter, FieldDelimiter: defaultFieldDelimiter}
		}
	}
	return o
}

func trimCompressionFileExts(name string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".bz"), ".bz2")
}

// set the SelectObjectInputSerialization struct using options passed in by client. If unspecified,
// default S3 API specified defaults
func selectObjectInputOpts(selOpts SelectObjectOpts, object string) minio.SelectObjectInputSerialization {
	var isOK bool
	var recDelim, fldDelim, quoteChar, quoteEscChar, fileHeader, commentChar, typ string

	i := minio.SelectObjectInputSerialization{}
	if _, ok := selOpts.InputSerOpts["parquet"]; ok {
		i.Parquet = &minio.ParquetInputOptions{}
	}
	if _, ok := selOpts.InputSerOpts["json"]; ok {
		i.JSON = &minio.JSONInputOptions{}
		if typ = selOpts.InputSerOpts["json"][typeJSONType]; typ != "" {
			i.JSON.Type = minio.JSONType(typ)
		}
	}
	if _, ok := selOpts.InputSerOpts["csv"]; ok {
		i.CSV = &minio.CSVInputOptions{RecordDelimiter: defaultRecordDelimiter}
		if recDelim, isOK = selOpts.InputSerOpts["csv"][recordDelimiterType]; isOK {
			i.CSV.RecordDelimiter = recDelim
		}
		if fldDelim, isOK = selOpts.InputSerOpts["csv"][fieldDelimiterType]; isOK {
			i.CSV.FieldDelimiter = fldDelim
		}
		if quoteChar, isOK = selOpts.InputSerOpts["csv"][quoteCharacterType]; isOK {
			i.CSV.QuoteCharacter = quoteChar
		}

		if quoteEscChar, isOK = selOpts.InputSerOpts["csv"][quoteEscapeCharacterType]; isOK {
			i.CSV.QuoteEscapeCharacter = quoteEscChar
		}
		fileHeader = selOpts.InputSerOpts["csv"][fileHeaderType]
		i.CSV.FileHeaderInfo = minio.CSVFileHeaderInfo(fileHeader)
		if commentChar, isOK = selOpts.InputSerOpts["csv"][commentCharType]; isOK {
			i.CSV.Comments = commentChar
		}
	}
	if i.CSV == nil && i.JSON == nil && i.Parquet == nil {
		ext := filepath.Ext(trimCompressionFileExts(object))
		if strings.Contains(ext, "csv") {
			i.CSV = &minio.CSVInputOptions{
				RecordDelimiter: defaultRecordDelimiter,
				FieldDelimiter:  defaultFieldDelimiter,
				FileHeaderInfo:  minio.CSVFileHeaderInfoUse,
			}
		}
		if strings.Contains(ext, "parquet") || strings.Contains(object, ".parquet") {
			i.Parquet = &minio.ParquetInputOptions{}
		}
		if strings.Contains(ext, "json") {
			i.JSON = &minio.JSONInputOptions{Type: minio.JSONLinesType}
		}
	}
	if i.CompressionType == "" {
		i.CompressionType = selectCompressionType(selOpts, object)
	}
	return i
}

// get client specified compression type or default compression type from file extension
func selectCompressionType(selOpts SelectObjectOpts, object string) minio.SelectCompressionType {
	ext := filepath.Ext(object)
	contentType := mimedb.TypeByExtension(ext)

	if selOpts.CompressionType != "" {
		return selOpts.CompressionType
	}
	if strings.Contains(ext, "parquet") || strings.Contains(object, ".parquet") {
		return minio.SelectCompressionNONE
	}
	if contentType != "" {
		if strings.Contains(contentType, "gzip") {
			return minio.SelectCompressionGZIP
		} else if strings.Contains(contentType, "bzip") {
			return minio.SelectCompressionBZIP
		}
	}
	return minio.SelectCompressionNONE
}

// Select - select object content wrapper.
func (c *S3Client) Select(expression string, sse encrypt.ServerSide, selOpts SelectObjectOpts) (io.ReadCloser, *probe.Error) {
	opts := minio.SelectObjectOptions{
		Expression:     expression,
		ExpressionType: minio.QueryExpressionTypeSQL,
		// Set any encryption headers
		ServerSideEncryption: sse,
	}

	bucket, object := c.url2BucketAndObject()

	opts.InputSerialization = selectObjectInputOpts(selOpts, object)
	opts.OutputSerialization = selectObjectOutputOpts(selOpts, opts.InputSerialization)
	reader, e := c.api.SelectObjectContent(context.Background(), bucket, object, opts)
	if e != nil {
		return nil, probe.NewError(e)
	}
	return reader, nil
}

func (c *S3Client) watchOneBucket(bucket, prefix, suffix string, events []string, doneCh chan struct{}, eventChan chan EventInfo, errorChan chan *probe.Error) {
	// Start listening on all bucket events.
	eventsCh := c.api.ListenBucketNotification(bucket, prefix, suffix, events, doneCh)
	for notificationInfo := range eventsCh {
		if notificationInfo.Err != nil {
			if nErr, ok := notificationInfo.Err.(minio.ErrorResponse); ok && nErr.Code == "APINotSupported" {
				errorChan <- probe.NewError(APINotImplemented{
					API:     "Watch",
					APIType: c.targetURL.Scheme + "://" + c.targetURL.Host,
				})
				return
			}
			errorChan <- probe.NewError(notificationInfo.Err)
		}

		for _, record := range notificationInfo.Records {
			bucketName := record.S3.Bucket.Name
			key, e := url.QueryUnescape(record.S3.Object.Key)
			if e != nil {
				errorChan <- probe.NewError(e)
				continue
			}
			u := *c.targetURL
			u.Path = path.Join(string(u.Separator), bucketName, key)
			if strings.HasPrefix(record.EventName, "s3:ObjectCreated:") {
				if strings.HasPrefix(record.EventName, "s3:ObjectCreated:Copy") {
					eventChan <- EventInfo{
						Time:         record.EventTime,
						Size:         record.S3.Object.Size,
						UserMetadata: record.S3.Object.UserMetadata,
						Path:         u.String(),
						Type:         EventCreateCopy,
						Host:         record.Source.Host,
						Port:         record.Source.Port,
						UserAgent:    record.Source.UserAgent,
					}
				} else if strings.HasPrefix(record.EventName, "s3:ObjectCreated:PutRetention") {
					eventChan <- EventInfo{
						Time:         record.EventTime,
						Size:         record.S3.Object.Size,
						UserMetadata: record.S3.Object.UserMetadata,
						Path:         u.String(),
						Type:         EventCreatePutRetention,
						Host:         record.Source.Host,
						Port:         record.Source.Port,
						UserAgent:    record.Source.UserAgent,
					}
				} else {
					eventChan <- EventInfo{
						Time:         record.EventTime,
						Size:         record.S3.Object.Size,
						UserMetadata: record.S3.Object.UserMetadata,
						Path:         u.String(),
						Type:         EventCreate,
						Host:         record.Source.Host,
						Port:         record.Source.Port,
						UserAgent:    record.Source.UserAgent,
					}
				}
			} else if strings.HasPrefix(record.EventName, "s3:ObjectRemoved:") {
				eventChan <- EventInfo{
					Time:      record.EventTime,
					Path:      u.String(),
					Type:      EventRemove,
					Host:      record.Source.Host,
					Port:      record.Source.Port,
					UserAgent: record.Source.UserAgent,
				}
			} else if record.EventName == minio.ObjectAccessedGet {
				eventChan <- EventInfo{
					Time:         record.EventTime,
					Size:         record.S3.Object.Size,
					UserMetadata: record.S3.Object.UserMetadata,
					Path:         u.String(),
					Type:         EventAccessedRead,
					Host:         record.Source.Host,
					Port:         record.Source.Port,
					UserAgent:    record.Source.UserAgent,
				}
			} else if record.EventName == minio.ObjectAccessedHead {
				eventChan <- EventInfo{
					Time:         record.EventTime,
					Size:         record.S3.Object.Size,
					UserMetadata: record.S3.Object.UserMetadata,
					Path:         u.String(),
					Type:         EventAccessedStat,
					Host:         record.Source.Host,
					Port:         record.Source.Port,
					UserAgent:    record.Source.UserAgent,
				}
			}
		}
	}
}

// Watch - Start watching on all bucket events for a given account ID.
func (c *S3Client) Watch(params watchParams) (*WatchObject, *probe.Error) {
	// Extract bucket and object.
	bucket, object := c.url2BucketAndObject()

	// Flag set to set the notification.
	var events []string
	for _, event := range params.events {
		switch event {
		case "put":
			events = append(events, string(minio.ObjectCreatedAll))
		case "delete":
			events = append(events, string(minio.ObjectRemovedAll))
		case "get":
			events = append(events, string(minio.ObjectAccessedAll))
		default:
			return nil, errInvalidArgument().Trace(event)
		}
	}
	if object != "" && params.prefix != "" {
		return nil, errInvalidArgument().Trace(params.prefix, object)
	}
	if object != "" && params.prefix == "" {
		params.prefix = object
	}

	// The list of buckets to watch
	var buckets []string
	if bucket == "" {
		bkts, err := c.api.ListBuckets()
		if err != nil {
			return nil, probe.NewError(err)
		}
		for _, b := range bkts {
			buckets = append(buckets, b.Name)
		}
	} else {
		buckets = append(buckets, bucket)
	}

	wo := &WatchObject{
		eventInfoChan: make(chan EventInfo),
		errorChan:     make(chan *probe.Error),
		doneChan:      make(chan bool),
	}

	// A done channel for each bucket listening API call
	doneChs := make([]chan struct{}, len(buckets))
	for i := range doneChs {
		doneChs[i] = make(chan struct{})
	}

	go func() {
		// Stop all listening bucket API calls when
		// receiving the main done call
		<-wo.doneChan
		for i := range doneChs {
			close(doneChs[i])
		}
	}()

	var wg sync.WaitGroup
	for i, bucket := range buckets {
		wg.Add(1)
		go func(bucket string, doneCh chan struct{}) {
			c.watchOneBucket(bucket, params.prefix, params.suffix, events, doneCh, wo.Events(), wo.Errors())
			wg.Done()
		}(bucket, doneChs[i])
	}

	go func() {
		wg.Wait()
		wo.Close()
	}()

	return wo, nil
}

// Get - get object with metadata.
func (c *S3Client) Get(sse encrypt.ServerSide) (io.ReadCloser, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	opts := minio.GetObjectOptions{}
	opts.ServerSideEncryption = sse
	reader, e := c.api.GetObject(bucket, object, opts)
	if e != nil {
		errResponse := minio.ToErrorResponse(e)
		if errResponse.Code == "NoSuchBucket" {
			return nil, probe.NewError(BucketDoesNotExist{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "InvalidBucketName" {
			return nil, probe.NewError(BucketInvalid{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "NoSuchKey" {
			return nil, probe.NewError(ObjectMissing{})
		}
		return nil, probe.NewError(e)
	}
	return reader, nil
}

// Copy - copy object, uses server side copy API. Also uses an abstracted API
// such that large file sizes will be copied in multipart manner on server
// side.
func (c *S3Client) Copy(source string, size int64, progress io.Reader, srcSSE, tgtSSE encrypt.ServerSide, metadata map[string]string, disableMultipart bool) *probe.Error {
	dstBucket, dstObject := c.url2BucketAndObject()
	if dstBucket == "" {
		return probe.NewError(BucketNameEmpty{})
	}

	tokens := splitStr(source, string(c.targetURL.Separator), 3)

	// Source object
	src := minio.NewSourceInfo(tokens[1], tokens[2], srcSSE)

	destOpts := minio.DestInfoOptions{
		Encryption: tgtSSE,
	}

	if lockModeStr, ok := metadata[AmzObjectLockMode]; ok {
		destOpts.Mode = minio.RetentionMode(strings.ToUpper(lockModeStr))
		delete(metadata, AmzObjectLockMode)
	}

	if retainUntilDateStr, ok := metadata[AmzObjectLockRetainUntilDate]; ok {
		delete(metadata, AmzObjectLockRetainUntilDate)
		if t, e := time.Parse(time.RFC3339, retainUntilDateStr); e == nil {
			destOpts.RetainUntilDate = t.UTC()
		}
	}

	if lh, ok := metadata[AmzObjectLockLegalHold]; ok {
		destOpts.LegalHold = minio.LegalHoldStatus(lh)
		delete(metadata, AmzObjectLockLegalHold)
	}

	// Assign metadata after irrelevant parts are delete above
	destOpts.UserMeta = metadata

	// Destination object
	dst, e := minio.NewDestinationInfoWithOptions(dstBucket, dstObject, destOpts)
	if e != nil {
		return probe.NewError(e)
	}

	if disableMultipart {
		e = c.api.CopyObjectWithProgress(dst, src, progress)
	} else {
		e = c.api.ComposeObjectWithProgress(dst, []minio.SourceInfo{src}, progress)
	}

	if e != nil {
		errResponse := minio.ToErrorResponse(e)
		if errResponse.Code == "AccessDenied" {
			return probe.NewError(PathInsufficientPermission{
				Path: c.targetURL.String(),
			})
		}
		if errResponse.Code == "NoSuchBucket" {
			return probe.NewError(BucketDoesNotExist{
				Bucket: dstBucket,
			})
		}
		if errResponse.Code == "InvalidBucketName" {
			return probe.NewError(BucketInvalid{
				Bucket: dstBucket,
			})
		}
		if errResponse.Code == "NoSuchKey" {
			return probe.NewError(ObjectMissing{})
		}
		return probe.NewError(e)
	}
	return nil
}

// Put - upload an object with custom metadata.
func (c *S3Client) Put(ctx context.Context, reader io.Reader, size int64, metadata map[string]string, progress io.Reader, sse encrypt.ServerSide, md5, disableMultipart bool) (int64, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return 0, probe.NewError(BucketNameEmpty{})
	}

	contentType, ok := metadata["Content-Type"]
	if ok {
		delete(metadata, "Content-Type")
	} else {
		// Set content-type if not specified.
		contentType = "application/octet-stream"
	}

	cacheControl, ok := metadata["Cache-Control"]
	if ok {
		delete(metadata, "Cache-Control")
	}

	contentEncoding, ok := metadata["Content-Encoding"]
	if ok {
		delete(metadata, "Content-Encoding")
	}

	contentDisposition, ok := metadata["Content-Disposition"]
	if ok {
		delete(metadata, "Content-Disposition")
	}

	contentLanguage, ok := metadata["Content-Language"]
	if ok {
		delete(metadata, "Content-Language")
	}

	storageClass, ok := metadata["X-Amz-Storage-Class"]
	if ok {
		delete(metadata, "X-Amz-Storage-Class")
	}

	lockModeStr, ok := metadata[AmzObjectLockMode]
	lockMode := minio.RetentionMode("")
	if ok {
		lockMode = minio.RetentionMode(strings.ToUpper(lockModeStr))
		delete(metadata, AmzObjectLockMode)
	}

	retainUntilDate := timeSentinel
	retainUntilDateStr, ok := metadata[AmzObjectLockRetainUntilDate]
	if ok {
		delete(metadata, AmzObjectLockRetainUntilDate)
		if t, e := time.Parse(time.RFC3339, retainUntilDateStr); e == nil {
			retainUntilDate = t.UTC()
		}
	}
	opts := minio.PutObjectOptions{
		UserMetadata:         metadata,
		Progress:             progress,
		NumThreads:           defaultMultipartThreadsNum,
		ContentType:          contentType,
		CacheControl:         cacheControl,
		ContentDisposition:   contentDisposition,
		ContentEncoding:      contentEncoding,
		ContentLanguage:      contentLanguage,
		StorageClass:         strings.ToUpper(storageClass),
		ServerSideEncryption: sse,
		SendContentMd5:       md5,
		DisableMultipart:     disableMultipart,
	}

	if !retainUntilDate.IsZero() && !retainUntilDate.Equal(timeSentinel) {
		opts.RetainUntilDate = &retainUntilDate
	}

	if lockModeStr != "" {
		opts.Mode = &lockMode
	}

	if lh, ok := metadata[AmzObjectLockLegalHold]; ok {
		delete(metadata, AmzObjectLockLegalHold)
		opts.LegalHold = minio.LegalHoldStatus(strings.ToUpper(lh))
	}

	n, e := c.api.PutObjectWithContext(ctx, bucket, object, reader, size, opts)
	if e != nil {
		errResponse := minio.ToErrorResponse(e)
		if errResponse.Code == "UnexpectedEOF" || e == io.EOF {
			return n, probe.NewError(UnexpectedEOF{
				TotalSize:    size,
				TotalWritten: n,
			})
		}
		if errResponse.Code == "AccessDenied" {
			return n, probe.NewError(PathInsufficientPermission{
				Path: c.targetURL.String(),
			})
		}
		if errResponse.Code == "MethodNotAllowed" {
			return n, probe.NewError(ObjectAlreadyExists{
				Object: object,
			})
		}
		if errResponse.Code == "XMinioObjectExistsAsDirectory" {
			return n, probe.NewError(ObjectAlreadyExistsAsDirectory{
				Object: object,
			})
		}
		if errResponse.Code == "NoSuchBucket" {
			return n, probe.NewError(BucketDoesNotExist{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "InvalidBucketName" {
			return n, probe.NewError(BucketInvalid{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "NoSuchKey" {
			return n, probe.NewError(ObjectMissing{})
		}
		return n, probe.NewError(e)
	}
	return n, nil
}

// Remove incomplete uploads.
func (c *S3Client) removeIncompleteObjects(bucket string, objectsCh <-chan string) <-chan minio.RemoveObjectError {
	removeObjectErrorCh := make(chan minio.RemoveObjectError)

	// Goroutine reads from objectsCh and sends error to removeObjectErrorCh if any.
	go func() {
		defer close(removeObjectErrorCh)

		for object := range objectsCh {
			if err := c.api.RemoveIncompleteUpload(bucket, object); err != nil {
				removeObjectErrorCh <- minio.RemoveObjectError{ObjectName: object, Err: err}
			}
		}
	}()

	return removeObjectErrorCh
}

// AddUserAgent - add custom user agent.
func (c *S3Client) AddUserAgent(app string, version string) {
	c.api.SetAppInfo(app, version)
}

// Remove - remove object or bucket(s).
func (c *S3Client) Remove(isIncomplete, isRemoveBucket, isBypass bool, contentCh <-chan *ClientContent) <-chan *probe.Error {
	errorCh := make(chan *probe.Error)

	prevBucket := ""
	// Maintain objectsCh, statusCh for each bucket
	var objectsCh chan string
	var statusCh <-chan minio.RemoveObjectError
	opts := minio.RemoveObjectsOptions{
		GovernanceBypass: isBypass,
	}

	go func() {
		defer close(errorCh)
		if isRemoveBucket {
			if _, object := c.url2BucketAndObject(); object != "" {
				errorCh <- probe.NewError(errors.New("cannot delete prefixes with `mc rb` command - Use `mc rm` instead"))
				return
			}
		}
		for content := range contentCh {
			// Convert content.URL.Path to objectName for objectsCh.
			bucket, objectName := c.splitPath(content.URL.Path)

			// We don't treat path when bucket is
			// empty, just skip it when it happens.
			if bucket == "" {
				continue
			}

			// Init objectsCh the first time.
			if prevBucket == "" {
				objectsCh = make(chan string)
				prevBucket = bucket
				if isIncomplete {
					statusCh = c.removeIncompleteObjects(bucket, objectsCh)
				} else {
					statusCh = c.api.RemoveObjectsWithOptions(bucket, objectsCh, opts)
				}
			}

			if prevBucket != bucket {
				if objectsCh != nil {
					close(objectsCh)
				}
				for removeStatus := range statusCh {
					errorCh <- probe.NewError(removeStatus.Err)
				}
				// Remove bucket if it qualifies.
				if isRemoveBucket && !isIncomplete {
					if err := c.api.RemoveBucket(prevBucket); err != nil {
						errorCh <- probe.NewError(err)
					}
				}
				// Re-init objectsCh for next bucket
				objectsCh = make(chan string)
				if isIncomplete {
					statusCh = c.removeIncompleteObjects(bucket, objectsCh)
				} else {
					statusCh = c.api.RemoveObjectsWithOptions(bucket, objectsCh, opts)
				}
				prevBucket = bucket
			}

			if objectName != "" {
				// Send object name once but continuously checks for pending
				// errors in parallel, the reason is that minio-go RemoveObjects
				// can block if there is any pending error not received yet.
				sent := false
				for !sent {
					select {
					case objectsCh <- objectName:
						sent = true
					case removeStatus := <-statusCh:
						errorCh <- probe.NewError(removeStatus.Err)
					}
				}
			} else {
				// end of bucket - close the objectsCh
				if objectsCh != nil {
					close(objectsCh)
				}
				objectsCh = nil
			}
		}
		// Close objectsCh at end of contentCh
		if objectsCh != nil {
			close(objectsCh)
		}
		// Write remove objects status to errorCh
		if statusCh != nil {
			for removeStatus := range statusCh {
				errorCh <- probe.NewError(removeStatus.Err)
			}
		}
		// Remove last bucket if it qualifies.
		if isRemoveBucket && prevBucket != "" && !isIncomplete {
			if err := c.api.RemoveBucket(prevBucket); err != nil {
				errorCh <- probe.NewError(err)
			}
		}
	}()
	return errorCh
}

// MakeBucket - make a new bucket.
func (c *S3Client) MakeBucket(region string, ignoreExisting, withLock bool) *probe.Error {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return probe.NewError(BucketNameEmpty{})
	}
	if object != "" {
		if !strings.HasSuffix(object, string(c.targetURL.Separator)) {
			object = path.Dir(object)
		}
		if !strings.HasSuffix(object, string(c.targetURL.Separator)) {
			return probe.NewError(BucketNameTopLevel{})
		}
		var retried bool
		for {
			_, e := c.api.PutObject(bucket, object,
				bytes.NewReader([]byte("")), 0, minio.PutObjectOptions{})
			if e == nil {
				return nil
			}
			if retried {
				return probe.NewError(e)
			}
			switch minio.ToErrorResponse(e).Code {
			case "NoSuchBucket":
				if withLock {
					e = c.api.MakeBucketWithObjectLock(bucket, region)
				} else {
					e = c.api.MakeBucket(bucket, region)
				}
				if e != nil {
					return probe.NewError(e)
				}
				retried = true
				continue
			}
			return probe.NewError(e)
		}
	}

	var e error
	if withLock {
		e = c.api.MakeBucketWithObjectLock(bucket, region)
	} else {
		e = c.api.MakeBucket(bucket, region)
	}
	if e != nil {
		// Ignore bucket already existing error when ignoreExisting flag is enabled
		if ignoreExisting {
			switch minio.ToErrorResponse(e).Code {
			case "BucketAlreadyOwnedByYou":
				fallthrough
			case "BucketAlreadyExists":
				return nil
			}
		}
		return probe.NewError(e)
	}
	return nil
}

// GetAccessRules - get configured policies from the server
func (c *S3Client) GetAccessRules() (map[string]string, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return map[string]string{}, probe.NewError(BucketNameEmpty{})
	}
	policies := map[string]string{}
	policyStr, e := c.api.GetBucketPolicy(bucket)
	if e != nil {
		return nil, probe.NewError(e)
	}
	if policyStr == "" {
		return policies, nil
	}
	var p policy.BucketAccessPolicy
	if e = json.Unmarshal([]byte(policyStr), &p); e != nil {
		return nil, probe.NewError(e)
	}
	policyRules := policy.GetPolicies(p.Statements, bucket, object)
	// Hide policy data structure at this level
	for k, v := range policyRules {
		policies[k] = string(v)
	}
	return policies, nil
}

// GetAccess get access policy permissions.
func (c *S3Client) GetAccess() (string, string, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return "", "", probe.NewError(BucketNameEmpty{})
	}
	policyStr, e := c.api.GetBucketPolicy(bucket)
	if e != nil {
		return "", "", probe.NewError(e)
	}
	if policyStr == "" {
		return string(policy.BucketPolicyNone), policyStr, nil
	}
	var p policy.BucketAccessPolicy
	if e = json.Unmarshal([]byte(policyStr), &p); e != nil {
		return "", "", probe.NewError(e)
	}
	pType := string(policy.GetPolicy(p.Statements, bucket, object))
	if pType == string(policy.BucketPolicyNone) && policyStr != "" {
		pType = "custom"
	}
	return pType, policyStr, nil
}

// SetAccess set access policy permissions.
func (c *S3Client) SetAccess(bucketPolicy string, isJSON bool) *probe.Error {
	bucket, object := c.url2BucketAndObject()
	if bucket == "" {
		return probe.NewError(BucketNameEmpty{})
	}
	if isJSON {
		if e := c.api.SetBucketPolicy(bucket, bucketPolicy); e != nil {
			return probe.NewError(e)
		}
		return nil
	}
	policyStr, e := c.api.GetBucketPolicy(bucket)
	if e != nil {
		return probe.NewError(e)
	}
	var p = policy.BucketAccessPolicy{Version: "2012-10-17"}
	if policyStr != "" {
		if e = json.Unmarshal([]byte(policyStr), &p); e != nil {
			return probe.NewError(e)
		}
	}
	p.Statements = policy.SetPolicy(p.Statements, policy.BucketPolicy(bucketPolicy), bucket, object)
	if len(p.Statements) == 0 {
		if e = c.api.SetBucketPolicy(bucket, ""); e != nil {
			return probe.NewError(e)
		}
		return nil
	}
	policyB, e := json.Marshal(p)
	if e != nil {
		return probe.NewError(e)
	}
	if e = c.api.SetBucketPolicy(bucket, string(policyB)); e != nil {
		return probe.NewError(e)
	}
	return nil
}

// listObjectWrapper - select ObjectList version depending on the target hostname
func (c *S3Client) listObjectWrapper(bucket, object string, isRecursive bool, doneCh chan struct{}, metadata bool) <-chan minio.ObjectInfo {
	if isGoogle(c.targetURL.Host) {
		// Google Cloud S3 layer doesn't implement ListObjectsV2 implementation
		// https://github.com/minio/mc/issues/3073
		return c.api.ListObjects(bucket, object, isRecursive, doneCh)
	}
	if metadata {
		return c.api.ListObjectsV2WithMetadata(bucket, object, isRecursive, doneCh)
	}
	return c.api.ListObjectsV2(bucket, object, isRecursive, doneCh)
}

func (c *S3Client) statIncompleteUpload(bucket, object string) (*ClientContent, *probe.Error) {
	nonRecursive := false
	objectMetadata := &ClientContent{}
	// Prefix to pass to minio-go listing in order to fetch a given object/directory
	prefix := strings.TrimRight(object, string(c.targetURL.Separator))

	for objectMultipartInfo := range c.api.ListIncompleteUploads(bucket, prefix, nonRecursive, nil) {
		if objectMultipartInfo.Err != nil {
			return nil, probe.NewError(objectMultipartInfo.Err)
		}

		if objectMultipartInfo.Key == object {
			objectMetadata.URL = *c.targetURL
			objectMetadata.Time = objectMultipartInfo.Initiated
			objectMetadata.Size = objectMultipartInfo.Size
			objectMetadata.Type = os.FileMode(0664)
			objectMetadata.Metadata = map[string]string{}
			return objectMetadata, nil
		}

		if strings.HasSuffix(objectMultipartInfo.Key, string(c.targetURL.Separator)) {
			objectMetadata.URL = *c.targetURL
			objectMetadata.Type = os.ModeDir
			objectMetadata.Metadata = map[string]string{}
			return objectMetadata, nil
		}
	}
	return nil, probe.NewError(ObjectMissing{})
}

// Stat - send a 'HEAD' on a bucket or object to fetch its metadata. It also returns
// a DIR type content if a prefix does exist in the server.
func (c *S3Client) Stat(isIncomplete, isPreserve bool, sse encrypt.ServerSide) (*ClientContent, *probe.Error) {
	c.Lock()
	defer c.Unlock()
	bucket, object := c.url2BucketAndObject()

	// Bucket name cannot be empty, stat on URL has no meaning.
	if bucket == "" {
		return nil, probe.NewError(BucketNameEmpty{})
	}

	if object == "" {
		content, err := c.bucketStat(bucket)
		if err != nil {
			return nil, err.Trace(bucket)
		}
		return content, nil
	}

	// If the request is for incomplete upload stat, handle it here.
	if isIncomplete {
		return c.statIncompleteUpload(bucket, object)
	}

	// The following code tries to calculate if a given prefix/object does really exist
	// using minio-go listing API. The following inputs are supported:
	//     - /path/to/existing/object
	//     - /path/to/existing_directory
	//     - /path/to/existing_directory/
	//     - /path/to/empty_directory
	//     - /path/to/empty_directory/

	opts := minio.StatObjectOptions{}
	opts.ServerSideEncryption = sse

	if !strings.HasSuffix(object, string(c.targetURL.Separator)) {
		// Issue HEAD request first but ignore no such key error
		// so we can check if there is such prefix which exists
		ctnt, err := c.getObjectStat(bucket, object, opts)
		if err == nil {
			return ctnt, err
		}
		// Ignore object missing error but return for other errors
		if !errors.As(err.ToGoError(), &ObjectMissing{}) {
			return nil, err
		}
	}

	nonRecursive := false
	objectMetadata := &ClientContent{}

	// Prefix to pass to minio-go listing in order to fetch if a prefix exists
	prefix := strings.TrimRight(object, string(c.targetURL.Separator))

	for objectStat := range c.listObjectWrapper(bucket, prefix, nonRecursive, nil, false) {
		if objectStat.Err != nil {
			return nil, probe.NewError(objectStat.Err)
		}
		if strings.HasSuffix(objectStat.Key, string(c.targetURL.Separator)) {
			objectMetadata.URL = *c.targetURL
			objectMetadata.Type = os.ModeDir
			return objectMetadata, nil
		}
	}

	return nil, probe.NewError(ObjectMissing{})
}

// getObjectStat returns the metadata of an object from a HEAD call.
func (c *S3Client) getObjectStat(bucket, object string, opts minio.StatObjectOptions) (*ClientContent, *probe.Error) {
	objectMetadata := &ClientContent{}
	objectStat, e := c.api.StatObject(bucket, object, opts)
	if e != nil {
		errResponse := minio.ToErrorResponse(e)
		if errResponse.Code == "AccessDenied" {
			return nil, probe.NewError(PathInsufficientPermission{Path: c.targetURL.String()})
		}
		if errResponse.Code == "NoSuchBucket" {
			return nil, probe.NewError(BucketDoesNotExist{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "InvalidBucketName" {
			return nil, probe.NewError(BucketInvalid{
				Bucket: bucket,
			})
		}
		if errResponse.Code == "NoSuchKey" {
			return nil, probe.NewError(ObjectMissing{})
		}
		return nil, probe.NewError(e)
	}
	objectMetadata.URL = *c.targetURL
	objectMetadata.Time = objectStat.LastModified
	objectMetadata.Size = objectStat.Size
	objectMetadata.ETag = objectStat.ETag
	objectMetadata.Expires = objectStat.Expires
	objectMetadata.Type = os.FileMode(0664)
	objectMetadata.Metadata = map[string]string{}
	for k := range objectStat.Metadata {
		objectMetadata.Metadata[k] = objectStat.Metadata.Get(k)
	}
	objectMetadata.ETag = objectStat.ETag
	return objectMetadata, nil
}

func isAmazon(host string) bool {
	return s3utils.IsAmazonEndpoint(url.URL{Host: host})
}

func isAmazonChina(host string) bool {
	amazonS3ChinaHost := regexp.MustCompile(`^s3\.(cn.*?)\.amazonaws\.com\.cn$`)
	parts := amazonS3ChinaHost.FindStringSubmatch(host)
	return len(parts) > 1
}

func isAmazonAccelerated(host string) bool {
	return host == "s3-accelerate.amazonaws.com"
}

func isGoogle(host string) bool {
	return s3utils.IsGoogleEndpoint(url.URL{Host: host})
}

// Figure out if the URL is of 'virtual host' style.
// Use lookup from config to see if dns/path style look
// up should be used. If it is set to "auto", use virtual
// style for supported hosts such as Amazon S3 and Google
// Cloud Storage. Otherwise, default to path style
func isVirtualHostStyle(host string, lookup minio.BucketLookupType) bool {
	if lookup == minio.BucketLookupDNS {
		return true
	}
	if lookup == minio.BucketLookupPath {
		return false
	}
	return isAmazon(host) && !isAmazonChina(host) || isGoogle(host) || isAmazonAccelerated(host)
}

// url2BucketAndObject gives bucketName and objectName from URL path.
func (c *S3Client) url2BucketAndObject() (bucketName, objectName string) {
	path := c.targetURL.Path
	// Convert any virtual host styled requests.
	//
	// For the time being this check is introduced for S3,
	// If you have custom virtual styled hosts please.
	// List them below.
	if c.virtualStyle {
		var bucket string
		hostIndex := strings.Index(c.targetURL.Host, "s3")
		if hostIndex != -1 && !matchS3InHost(c.targetURL.Host) {
			hostIndex = -1
		}
		if hostIndex == -1 {
			hostIndex = strings.Index(c.targetURL.Host, "s3-accelerate")
		}
		if hostIndex == -1 {
			hostIndex = strings.Index(c.targetURL.Host, "storage.googleapis")
		}
		if hostIndex > 0 {
			bucket = c.targetURL.Host[:hostIndex-1]
			path = string(c.targetURL.Separator) + bucket + c.targetURL.Path
		}
	}
	tokens := splitStr(path, string(c.targetURL.Separator), 3)
	return tokens[1], tokens[2]
}

// splitPath split path into bucket and object.
func (c *S3Client) splitPath(path string) (bucketName, objectName string) {
	path = strings.TrimPrefix(path, string(c.targetURL.Separator))

	// Handle path if its virtual style.
	if c.virtualStyle {
		hostIndex := strings.Index(c.targetURL.Host, "s3")
		if hostIndex == -1 {
			hostIndex = strings.Index(c.targetURL.Host, "s3-accelerate")
		}
		if hostIndex == -1 {
			hostIndex = strings.Index(c.targetURL.Host, "storage.googleapis")
		}
		if hostIndex > 0 {
			bucketName = c.targetURL.Host[:hostIndex-1]
			objectName = path
			return bucketName, objectName
		}
	}

	tokens := splitStr(path, string(c.targetURL.Separator), 2)
	return tokens[0], tokens[1]
}

/// Bucket API operations.

// List - list at delimited path, if not recursive.
func (c *S3Client) List(isRecursive, isIncomplete, isMetadata bool, showDir DirOpt) <-chan *ClientContent {
	c.Lock()
	defer c.Unlock()

	contentCh := make(chan *ClientContent)
	if isIncomplete {
		if isRecursive {
			if showDir == DirNone {
				go c.listIncompleteRecursiveInRoutine(contentCh)
			} else {
				go c.listIncompleteRecursiveInRoutineDirOpt(contentCh, showDir)
			}
		} else {
			go c.listIncompleteInRoutine(contentCh)
		}
	} else {
		if isRecursive {
			if showDir == DirNone {
				go c.listRecursiveInRoutine(contentCh, isMetadata)
			} else {
				go c.listRecursiveInRoutineDirOpt(contentCh, showDir, isMetadata)
			}
		} else {
			go c.listInRoutine(contentCh, isMetadata)
		}
	}

	return contentCh
}

func (c *S3Client) listIncompleteInRoutine(contentCh chan *ClientContent) {
	defer close(contentCh)
	// get bucket and object from URL.
	b, o := c.url2BucketAndObject()
	switch {
	case b == "" && o == "":
		buckets, err := c.api.ListBuckets()
		if err != nil {
			contentCh <- &ClientContent{
				Err: probe.NewError(err),
			}
			return
		}
		isRecursive := false
		for _, bucket := range buckets {
			for object := range c.api.ListIncompleteUploads(bucket.Name, o, isRecursive, nil) {
				if object.Err != nil {
					contentCh <- &ClientContent{
						Err: probe.NewError(object.Err),
					}
					return
				}
				content := &ClientContent{}
				url := *c.targetURL
				// Join bucket with - incoming object key.
				url.Path = c.joinPath(bucket.Name, object.Key)
				switch {
				case strings.HasSuffix(object.Key, string(c.targetURL.Separator)):
					// We need to keep the trailing Separator, do not use filepath.Join().
					content.URL = url
					content.Time = time.Now()
					content.Type = os.ModeDir
				default:
					content.URL = url
					content.Size = object.Size
					content.Time = object.Initiated
					content.Type = os.ModeTemporary
				}
				contentCh <- content
			}
		}
	default:
		isRecursive := false
		for object := range c.api.ListIncompleteUploads(b, o, isRecursive, nil) {
			if object.Err != nil {
				contentCh <- &ClientContent{
					Err: probe.NewError(object.Err),
				}
				return
			}
			content := &ClientContent{}
			url := *c.targetURL
			// Join bucket with - incoming object key.
			url.Path = c.joinPath(b, object.Key)
			switch {
			case strings.HasSuffix(object.Key, string(c.targetURL.Separator)):
				// We need to keep the trailing Separator, do not use filepath.Join().
				content.URL = url
				content.Time = time.Now()
				content.Type = os.ModeDir
			default:
				content.URL = url
				content.Size = object.Size
				content.Time = object.Initiated
				content.Type = os.ModeTemporary
			}
			contentCh <- content
		}
	}
}

func (c *S3Client) listIncompleteRecursiveInRoutine(contentCh chan *ClientContent) {
	defer close(contentCh)
	// get bucket and object from URL.
	b, o := c.url2BucketAndObject()
	switch {
	case b == "" && o == "":
		buckets, err := c.api.ListBuckets()
		if err != nil {
			contentCh <- &ClientContent{
				Err: probe.NewError(err),
			}
			return
		}
		isRecursive := true
		for _, bucket := range buckets {
			for object := range c.api.ListIncompleteUploads(bucket.Name, o, isRecursive, nil) {
				if object.Err != nil {
					contentCh <- &ClientContent{
						Err: probe.NewError(object.Err),
					}
					return
				}
				url := *c.targetURL
				url.Path = c.joinPath(bucket.Name, object.Key)
				content := &ClientContent{}
				content.URL = url
				content.Size = object.Size
				content.Time = object.Initiated
				content.Type = os.ModeTemporary
				contentCh <- content
			}
		}
	default:
		isRecursive := true
		for object := range c.api.ListIncompleteUploads(b, o, isRecursive, nil) {
			if object.Err != nil {
				contentCh <- &ClientContent{
					Err: probe.NewError(object.Err),
				}
				return
			}
			url := *c.targetURL
			// Join bucket and incoming object key.
			url.Path = c.joinPath(b, object.Key)
			content := &ClientContent{}
			content.URL = url
			content.Size = object.Size
			content.Time = object.Initiated
			content.Type = os.ModeTemporary
			contentCh <- content
		}
	}
}

// Convert objectMultipartInfo to ClientContent
func (c *S3Client) objectMultipartInfo2ClientContent(bucket string, entry minio.ObjectMultipartInfo) ClientContent {

	content := ClientContent{}
	url := *c.targetURL
	// Join bucket and incoming object key.
	url.Path = c.joinPath(bucket, entry.Key)
	content.URL = url
	content.Size = entry.Size
	content.Time = entry.Initiated

	if strings.HasSuffix(entry.Key, string(c.targetURL.Separator)) {
		content.Type = os.ModeDir
	} else {
		content.Type = os.ModeTemporary
	}

	return content
}

// Recursively lists incomplete uploads.
func (c *S3Client) listIncompleteRecursiveInRoutineDirOpt(contentCh chan *ClientContent, dirOpt DirOpt) {
	defer close(contentCh)

	// Closure function reads list of incomplete uploads and sends to contentCh. If a directory is found, it lists
	// incomplete uploads of the directory content recursively.
	var listDir func(bucket, object string) bool
	listDir = func(bucket, object string) (isStop bool) {
		isRecursive := false
		for entry := range c.api.ListIncompleteUploads(bucket, object, isRecursive, nil) {
			if entry.Err != nil {
				url := *c.targetURL
				url.Path = c.joinPath(bucket, object)
				contentCh <- &ClientContent{URL: url, Err: probe.NewError(entry.Err)}

				errResponse := minio.ToErrorResponse(entry.Err)
				if errResponse.Code == "AccessDenied" {
					continue
				}

				return true
			}

			content := c.objectMultipartInfo2ClientContent(bucket, entry)

			// Handle if object.Key is a directory.
			if strings.HasSuffix(entry.Key, string(c.targetURL.Separator)) {
				if dirOpt == DirFirst {
					contentCh <- &content
				}
				if listDir(bucket, entry.Key) {
					return true
				}
				if dirOpt == DirLast {
					contentCh <- &content
				}
			} else {
				contentCh <- &content
			}
		}

		return false
	}

	bucket, object := c.url2BucketAndObject()
	var cContent *ClientContent
	var buckets []minio.BucketInfo
	var allBuckets bool
	// List all buckets if bucket and object are empty.
	if bucket == "" && object == "" {
		var e error
		allBuckets = true
		buckets, e = c.api.ListBuckets()
		if e != nil {
			contentCh <- &ClientContent{Err: probe.NewError(e)}
			return
		}
	} else if object == "" {
		// Get bucket stat if object is empty.
		content, err := c.bucketStat(bucket)
		if err != nil {
			contentCh <- &ClientContent{Err: err.Trace(bucket)}
			return
		}
		buckets = append(buckets, minio.BucketInfo{Name: bucket, CreationDate: content.Time})
	} else if strings.HasSuffix(object, string(c.targetURL.Separator)) {
		// Get stat of given object is a directory.
		isIncomplete := true
		content, perr := c.Stat(isIncomplete, false, nil)
		cContent = content
		if perr != nil {
			contentCh <- &ClientContent{Err: perr.Trace(bucket)}
			return
		}
		buckets = append(buckets, minio.BucketInfo{Name: bucket, CreationDate: content.Time})
	}
	for _, bucket := range buckets {
		if allBuckets {
			url := *c.targetURL
			url.Path = c.joinPath(bucket.Name)
			cContent = &ClientContent{
				URL:  url,
				Time: bucket.CreationDate,
				Type: os.ModeDir,
			}
		}
		if cContent != nil && dirOpt == DirFirst {
			contentCh <- cContent
		}
		//Recursively push all object prefixes into contentCh to mimic directory listing
		listDir(bucket.Name, object)

		if cContent != nil && dirOpt == DirLast {
			contentCh <- cContent
		}
	}
}

// Returns new path by joining path segments with URL path separator.
func (c *S3Client) joinPath(bucket string, objects ...string) string {
	p := string(c.targetURL.Separator) + bucket
	for _, o := range objects {
		p += string(c.targetURL.Separator) + o
	}
	return p
}

// Convert objectInfo to ClientContent
func (c *S3Client) objectInfo2ClientContent(bucket string, entry minio.ObjectInfo) *ClientContent {
	content := &ClientContent{}
	url := *c.targetURL
	// Join bucket and incoming object key.
	url.Path = c.joinPath(bucket, entry.Key)
	content.URL = url
	content.Size = entry.Size
	content.ETag = entry.ETag
	content.Time = entry.LastModified
	content.Expires = entry.Expires
	content.Metadata = map[string]string{}
	content.UserMetadata = map[string]string{}
	for k, v := range entry.UserMetadata {
		content.UserMetadata[k] = v
	}
	for k := range entry.Metadata {
		content.Metadata[k] = entry.Metadata.Get(k)
	}
	if strings.HasSuffix(entry.Key, string(c.targetURL.Separator)) && entry.Size == 0 && entry.LastModified.IsZero() {
		content.Type = os.ModeDir
		content.Time = time.Now()
	} else {
		content.Type = os.FileMode(0664)
	}

	return content
}

// Returns bucket stat info of current bucket.
func (c *S3Client) bucketStat(bucket string) (*ClientContent, *probe.Error) {
	exists, e := c.api.BucketExists(bucket)
	if e != nil {
		return nil, probe.NewError(e)
	}
	if !exists {
		return nil, probe.NewError(BucketDoesNotExist{Bucket: bucket})
	}
	return &ClientContent{URL: *c.targetURL, Time: time.Unix(0, 0), Type: os.ModeDir}, nil
}

// Recursively lists objects.
func (c *S3Client) listRecursiveInRoutineDirOpt(contentCh chan *ClientContent, dirOpt DirOpt, metadata bool) {
	defer close(contentCh)
	// Closure function reads list objects and sends to contentCh. If a directory is found, it lists
	// objects of the directory content recursively.
	var listDir func(bucket, object string) bool
	listDir = func(bucket, object string) (isStop bool) {
		isRecursive := false
		for entry := range c.listObjectWrapper(bucket, object, isRecursive, nil, metadata) {
			if entry.Err != nil {
				url := *c.targetURL
				url.Path = c.joinPath(bucket, object)
				contentCh <- &ClientContent{URL: url, Err: probe.NewError(entry.Err)}

				errResponse := minio.ToErrorResponse(entry.Err)
				if errResponse.Code == "AccessDenied" {
					continue
				}
				return true
			}

			content := c.objectInfo2ClientContent(bucket, entry)

			// Handle if object.Key is a directory.
			if content.Type.IsDir() {
				if dirOpt == DirFirst {
					contentCh <- content
				}
				if listDir(bucket, entry.Key) {
					return true
				}
				if dirOpt == DirLast {
					contentCh <- content
				}
			} else {
				contentCh <- content
			}
		}
		return false
	}

	bucket, object := c.url2BucketAndObject()

	var cContent *ClientContent
	var buckets []minio.BucketInfo
	var allBuckets bool
	// List all buckets if bucket and object are empty.
	if bucket == "" && object == "" {
		var e error
		allBuckets = true
		buckets, e = c.api.ListBuckets()
		if e != nil {
			contentCh <- &ClientContent{Err: probe.NewError(e)}
			return
		}
	} else if object == "" {
		// Get bucket stat if object is empty.
		content, err := c.bucketStat(bucket)
		if err != nil {
			contentCh <- &ClientContent{Err: err.Trace(bucket)}
			return
		}
		buckets = append(buckets, minio.BucketInfo{Name: bucket, CreationDate: content.Time})
	} else {
		// Get stat of given object is a directory.
		isIncomplete := false
		content, perr := c.Stat(isIncomplete, false, nil)
		cContent = content
		if perr != nil {
			contentCh <- &ClientContent{Err: perr.Trace(bucket)}
			return
		}
		buckets = append(buckets, minio.BucketInfo{Name: bucket, CreationDate: content.Time})
	}

	for _, bucket := range buckets {
		if allBuckets {
			url := *c.targetURL
			url.Path = c.joinPath(bucket.Name)
			cContent = &ClientContent{
				URL:  url,
				Time: bucket.CreationDate,
				Type: os.ModeDir,
			}
		}
		if cContent != nil && dirOpt == DirFirst {
			contentCh <- cContent
		}
		// Recurse thru prefixes to mimic directory listing and push into contentCh
		listDir(bucket.Name, object)

		if cContent != nil && dirOpt == DirLast {
			contentCh <- cContent
		}
	}
}

func (c *S3Client) listInRoutine(contentCh chan *ClientContent, metadata bool) {
	defer close(contentCh)
	// get bucket and object from URL.
	b, o := c.url2BucketAndObject()
	switch {
	case b == "" && o == "":
		buckets, e := c.api.ListBuckets()
		if e != nil {
			contentCh <- &ClientContent{
				Err: probe.NewError(e),
			}
			return
		}
		for _, bucket := range buckets {
			url := *c.targetURL
			url.Path = c.joinPath(bucket.Name)
			content := &ClientContent{}
			content.URL = url
			content.Size = 0
			content.Time = bucket.CreationDate
			content.Type = os.ModeDir
			contentCh <- content
		}
	case b != "" && !strings.HasSuffix(c.targetURL.Path, string(c.targetURL.Separator)) && o == "":
		content, err := c.bucketStat(b)
		if err != nil {
			contentCh <- &ClientContent{Err: err.Trace(b)}
			return
		}
		contentCh <- content
	default:
		isRecursive := false
		for object := range c.listObjectWrapper(b, o, isRecursive, nil, metadata) {
			if object.Err != nil {
				contentCh <- &ClientContent{
					Err: probe.NewError(object.Err),
				}
				return
			}

			// Avoid sending an empty directory when we are specifically listing it
			if strings.HasSuffix(object.Key, string(c.targetURL.Separator)) && o == object.Key {
				continue
			}

			contentCh <- c.objectInfo2ClientContent(b, object)
		}
	}
}

// S3 offers a range of storage classes designed for
// different use cases, following list captures these.
const (
	// General purpose.
	// s3StorageClassStandard = "STANDARD"
	// Infrequent access.
	// s3StorageClassInfrequent = "STANDARD_IA"
	// Reduced redundancy access.
	// s3StorageClassRedundancy = "REDUCED_REDUNDANCY"
	// Archive access.
	s3StorageClassGlacier = "GLACIER"
)

func (c *S3Client) listRecursiveInRoutine(contentCh chan *ClientContent, metadata bool) {
	defer close(contentCh)
	// get bucket and object from URL.
	b, o := c.url2BucketAndObject()
	switch {
	case b == "" && o == "":
		buckets, err := c.api.ListBuckets()
		if err != nil {
			contentCh <- &ClientContent{
				Err: probe.NewError(err),
			}
			return
		}
		for _, bucket := range buckets {
			isRecursive := true
			for object := range c.listObjectWrapper(bucket.Name, o, isRecursive, nil, metadata) {
				if object.Err != nil {
					contentCh <- &ClientContent{
						Err: probe.NewError(object.Err),
					}
					return
				}
				content := &ClientContent{}
				objectURL := *c.targetURL
				objectURL.Path = c.joinPath(bucket.Name, object.Key)
				content.URL = objectURL
				content.StorageClass = object.StorageClass
				content.Size = object.Size
				content.ETag = object.ETag
				content.Time = object.LastModified
				content.Type = os.FileMode(0664)
				content.Expires = object.Expires
				content.UserMetadata = map[string]string{}
				for k, v := range object.UserMetadata {
					content.UserMetadata[k] = v
				}
				contentCh <- content
			}
		}
	default:
		isRecursive := true
		for object := range c.listObjectWrapper(b, o, isRecursive, nil, metadata) {
			if object.Err != nil {
				contentCh <- &ClientContent{
					Err: probe.NewError(object.Err),
				}
				return
			}
			content := &ClientContent{}
			url := *c.targetURL
			// Join bucket and incoming object key.
			url.Path = c.joinPath(b, object.Key)
			content.URL = url
			content.Size = object.Size
			content.ETag = object.ETag
			content.Time = object.LastModified
			content.Type = os.FileMode(0664)
			content.Expires = object.Expires
			content.UserMetadata = map[string]string{}
			for k, v := range object.UserMetadata {
				content.UserMetadata[k] = v
			}
			contentCh <- content
		}
	}
}

// ShareDownload - get a usable presigned object url to share.
func (c *S3Client) ShareDownload(expires time.Duration) (string, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	// No additional request parameters are set for the time being.
	reqParams := make(url.Values)
	presignedURL, e := c.api.PresignedGetObject(bucket, object, expires, reqParams)
	if e != nil {
		return "", probe.NewError(e)
	}
	return presignedURL.String(), nil
}

// ShareUpload - get data for presigned post http form upload.
func (c *S3Client) ShareUpload(isRecursive bool, expires time.Duration, contentType string) (string, map[string]string, *probe.Error) {
	bucket, object := c.url2BucketAndObject()
	p := minio.NewPostPolicy()
	if e := p.SetExpires(UTCNow().Add(expires)); e != nil {
		return "", nil, probe.NewError(e)
	}
	if strings.TrimSpace(contentType) != "" || contentType != "" {
		// No need to verify for error here, since we have stripped out spaces.
		p.SetContentType(contentType)
	}
	if e := p.SetBucket(bucket); e != nil {
		return "", nil, probe.NewError(e)
	}
	if isRecursive {
		if e := p.SetKeyStartsWith(object); e != nil {
			return "", nil, probe.NewError(e)
		}
	} else {
		if e := p.SetKey(object); e != nil {
			return "", nil, probe.NewError(e)
		}
	}
	u, m, e := c.api.PresignedPostPolicy(p)
	if e != nil {
		return "", nil, probe.NewError(e)
	}
	return u.String(), m, nil
}

// SetObjectLockConfig - Set object lock configurataion of bucket.
func (c *S3Client) SetObjectLockConfig(mode *minio.RetentionMode, validity *uint, unit *minio.ValidityUnit) *probe.Error {
	bucket, _ := c.url2BucketAndObject()

	err := c.api.SetBucketObjectLockConfig(bucket, mode, validity, unit)
	if err != nil {
		return probe.NewError(err)
	}

	return nil
}

// PutObjectRetention - Set object retention for a given object.
func (c *S3Client) PutObjectRetention(mode *minio.RetentionMode, retainUntilDate *time.Time, bypassGovernance bool) *probe.Error {
	bucket, object := c.url2BucketAndObject()

	opts := minio.PutObjectRetentionOptions{
		RetainUntilDate:  retainUntilDate,
		Mode:             mode,
		GovernanceBypass: bypassGovernance,
	}
	err := c.api.PutObjectRetention(bucket, object, opts)
	if err != nil {
		return probe.NewError(err)
	}

	return nil
}

// PutObjectLegalHold - Set object legal hold for a given object.
func (c *S3Client) PutObjectLegalHold(lhold *minio.LegalHoldStatus) *probe.Error {
	bucket, object := c.url2BucketAndObject()
	opts := minio.PutObjectLegalHoldOptions{
		Status: lhold,
	}
	err := c.api.PutObjectLegalHold(bucket, object, opts)
	if err != nil {
		return probe.NewError(err)
	}
	return nil
}

// GetObjectLockConfig - Get object lock configuration of bucket.
func (c *S3Client) GetObjectLockConfig() (mode *minio.RetentionMode, validity *uint, unit *minio.ValidityUnit, perr *probe.Error) {
	bucket, _ := c.url2BucketAndObject()

	mode, validity, unit, err := c.api.GetBucketObjectLockConfig(bucket)
	if err != nil {
		return nil, nil, nil, probe.NewError(err)
	}

	return mode, validity, unit, nil
}

// GetObjectTagging - Get Object Tags
func (c *S3Client) GetObjectTagging() (tagging.Tagging, *probe.Error) {
	var err error
	bucketName, objectName := c.url2BucketAndObject()
	if bucketName == "" {
		return tagging.Tagging{}, probe.NewError(BucketNameEmpty{})
	}
	if objectName == "" {
		return tagging.Tagging{}, probe.NewError(ObjectNameEmpty{})
	}
	tagXML, err := c.api.GetObjectTagging(bucketName, objectName)
	if err != nil {
		return tagging.Tagging{}, probe.NewError(err)
	}
	var tagObj tagging.Tagging
	if err = xml.Unmarshal([]byte(tagXML), &tagObj); err != nil {
		return tagging.Tagging{}, probe.NewError(err)
	}
	return tagObj, nil
}

// SetObjectTagging - Set Object tags
func (c *S3Client) SetObjectTagging(tagMap map[string]string) *probe.Error {
	var err error
	bucketName, objectName := c.url2BucketAndObject()
	if bucketName == "" {
		return probe.NewError(BucketNameEmpty{})
	}
	if objectName == "" {
		return probe.NewError(ObjectNameEmpty{})
	}
	if err = c.api.PutObjectTagging(bucketName, objectName, tagMap); err != nil {
		return probe.NewError(err)
	}
	return nil
}

// DeleteObjectTagging - Delete object tags
func (c *S3Client) DeleteObjectTagging() *probe.Error {
	bucketName, objectName := c.url2BucketAndObject()
	if bucketName == "" {
		return probe.NewError(BucketNameEmpty{})
	}
	if objectName == "" {
		return probe.NewError(ObjectNameEmpty{})
	}
	if err := c.api.RemoveObjectTagging(bucketName, objectName); err != nil {
		return probe.NewError(err)
	}
	return nil
}
