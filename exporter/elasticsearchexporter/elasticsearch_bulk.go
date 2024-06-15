// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package elasticsearchexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/elasticsearchexporter"

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/elastic/go-docappender/v2"
	elasticsearch7 "github.com/elastic/go-elasticsearch/v7"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/common/sanitize"
)

type esClientCurrent = elasticsearch7.Client
type esConfigCurrent = elasticsearch7.Config

type esBulkIndexerCurrent = bulkIndexerPool

type esBulkIndexerItem = docappender.BulkIndexerItem

// clientLogger implements the estransport.Logger interface
// that is required by the Elasticsearch client for logging.
type clientLogger zap.Logger

// LogRoundTrip should not modify the request or response, except for consuming and closing the body.
// Implementations have to check for nil values in request and response.
func (cl *clientLogger) LogRoundTrip(requ *http.Request, resp *http.Response, err error, _ time.Time, dur time.Duration) error {
	zl := (*zap.Logger)(cl)
	switch {
	case err == nil && resp != nil:
		zl.Debug("Request roundtrip completed.",
			zap.String("path", sanitize.String(requ.URL.Path)),
			zap.String("method", requ.Method),
			zap.Duration("duration", dur),
			zap.String("status", resp.Status))

	case err != nil:
		zl.Error("Request failed.", zap.NamedError("reason", err))
	}

	return nil
}

// RequestBodyEnabled makes the client pass a copy of request body to the logger.
func (*clientLogger) RequestBodyEnabled() bool {
	// TODO: introduce setting log the bodies for more detailed debug logs
	return false
}

// ResponseBodyEnabled makes the client pass a copy of response body to the logger.
func (*clientLogger) ResponseBodyEnabled() bool {
	// TODO: introduce setting log the bodies for more detailed debug logs
	return false
}

func newElasticsearchClient(
	ctx context.Context,
	config *Config,
	host component.Host,
	telemetry component.TelemetrySettings,
	userAgent string,
) (*esClientCurrent, error) {
	httpClient, err := config.ClientConfig.ToClient(ctx, host, telemetry)
	if err != nil {
		return nil, err
	}

	headers := make(http.Header)
	headers.Set("User-Agent", userAgent)

	// maxRetries configures the maximum number of event publishing attempts,
	// including the first send and additional retries.

	maxRetries := config.Retry.MaxRequests - 1
	retryDisabled := !config.Retry.Enabled || maxRetries <= 0

	if retryDisabled {
		maxRetries = 0
	}

	// endpoints converts Config.Endpoints, Config.CloudID,
	// and Config.ClientConfig.Endpoint to a list of addresses.
	endpoints, err := config.endpoints()
	if err != nil {
		return nil, err
	}

	return elasticsearch7.NewClient(esConfigCurrent{
		Transport: httpClient.Transport,

		// configure connection setup
		Addresses: endpoints,
		Username:  config.Authentication.User,
		Password:  string(config.Authentication.Password),
		APIKey:    string(config.Authentication.APIKey),
		Header:    headers,

		// configure retry behavior
		RetryOnStatus:        config.Retry.RetryOnStatus,
		DisableRetry:         retryDisabled,
		EnableRetryOnTimeout: config.Retry.Enabled,
		//RetryOnError:  retryOnError, // should be used from esclient version 8 onwards
		MaxRetries:   maxRetries,
		RetryBackoff: createElasticsearchBackoffFunc(&config.Retry),

		// configure sniffing
		DiscoverNodesOnStart:  config.Discovery.OnStart,
		DiscoverNodesInterval: config.Discovery.Interval,

		// configure internal metrics reporting and logging
		EnableMetrics:     false, // TODO
		EnableDebugLogger: false, // TODO
		Logger:            (*clientLogger)(telemetry.Logger),
	})
}

func createElasticsearchBackoffFunc(config *RetrySettings) func(int) time.Duration {
	if !config.Enabled {
		return nil
	}

	expBackoff := backoff.NewExponentialBackOff()
	if config.InitialInterval > 0 {
		expBackoff.InitialInterval = config.InitialInterval
	}
	if config.MaxInterval > 0 {
		expBackoff.MaxInterval = config.MaxInterval
	}
	expBackoff.Reset()

	return func(attempts int) time.Duration {
		if attempts == 1 {
			expBackoff.Reset()
		}

		return expBackoff.NextBackOff()
	}
}

func pushDocumentsLog(logger *zap.Logger, ctx context.Context, index string, document []byte, bulkIndexer *esBulkIndexerCurrent) error {
	logger.Info("Lalith [pushDocumentsLog] came to add doc")
	err := bulkIndexer.Add(ctx, index, bytes.NewReader(document))
	if err != nil {
		logger.Error("Lalith [pushDocumentsLog] ...some error1" + err.Error())
		return err
	}

	select {
	case err := <-bulkIndexer.errorChan:
		logger.Error("Lalith [pushDocumentsLog] some error in bulk indexer error chan")
		return err
	case <-ctx.Done():
		logger.Error("Lalith [pushDocumentsLog] context is done" + ctx.Err().Error())
		return ctx.Err()
	default:
		return nil
	}
}

func pushDocuments(ctx context.Context, index string, document []byte, bulkIndexer *esBulkIndexerCurrent) error {
	return bulkIndexer.Add(ctx, index, bytes.NewReader(document))
}

func newBulkIndexer(logger *zap.Logger, client *elasticsearch7.Client, config *Config) (*esBulkIndexerCurrent, error) {
	numWorkers := config.NumWorkers
	if numWorkers == 0 {
		numWorkers = runtime.NumCPU()
	}

	flushInterval := config.Flush.Interval
	if flushInterval == 0 {
		flushInterval = 30 * time.Second
	}

	flushBytes := config.Flush.Bytes
	if flushBytes == 0 {
		flushBytes = 5e+6
	}

	var maxDocRetry int
	if config.Retry.Enabled {
		// max_requests includes initial attempt
		// See https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/32344
		maxDocRetry = config.Retry.MaxRequests - 1
	}

	pool := &bulkIndexerPool{
		wg:        sync.WaitGroup{},
		items:     make(chan esBulkIndexerItem, config.NumWorkers),
		stats:     bulkIndexerStats{},
		errorChan: make(chan error, numWorkers),
	}
	pool.wg.Add(numWorkers)

	for i := 0; i < numWorkers; i++ {
		bi, err := docappender.NewBulkIndexer(docappender.BulkIndexerConfig{
			Client:                client,
			MaxDocumentRetries:    maxDocRetry,
			Pipeline:              config.Pipeline,
			RetryOnDocumentStatus: config.Retry.RetryOnStatus,
		})
		if err != nil {
			return nil, err
		}
		w := worker{
			indexer:       bi,
			items:         pool.items,
			flushInterval: flushInterval,
			flushTimeout:  config.Timeout,
			flushBytes:    flushBytes,
			logger:        logger,
			stats:         &pool.stats,
			errorChan:     pool.errorChan,
		}
		go func() {
			defer pool.wg.Done()
			w.run()
		}()
	}
	return pool, nil
}

type bulkIndexerStats struct {
	docsIndexed atomic.Int64
}

type bulkIndexerPool struct {
	items     chan esBulkIndexerItem
	wg        sync.WaitGroup
	stats     bulkIndexerStats
	errorChan chan error
}

// Add adds an item to the bulk indexer pool.
//
// Adding an item after a call to Close() will panic.
func (p *bulkIndexerPool) Add(ctx context.Context, index string, document io.WriterTo) error {
	item := esBulkIndexerItem{
		Index: index,
		Body:  document,
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.items <- item:
		return nil
	}
}

// Close closes the items channel and waits for the workers to drain it.
func (p *bulkIndexerPool) Close(ctx context.Context) error {
	close(p.items)
	doneCh := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(doneCh)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-doneCh:
		return nil
	}
}

type worker struct {
	indexer       *docappender.BulkIndexer
	items         <-chan esBulkIndexerItem
	flushInterval time.Duration
	flushTimeout  time.Duration
	flushBytes    int
	errorChan     chan error

	stats *bulkIndexerStats

	logger *zap.Logger
}

func (w *worker) run() {
	flushTick := time.NewTicker(w.flushInterval)
	defer flushTick.Stop()
	for {
		select {
		case item, ok := <-w.items:
			// if channel is closed, flush and return
			if !ok {
				w.flush()
				return
			}

			if err := w.indexer.Add(item); err != nil {
				w.logger.Error("error adding item to bulk indexer", zap.Error(err))
				w.errorChan <- err
			}

			// w.indexer.Len() can be either compressed or uncompressed bytes
			if w.indexer.Len() >= w.flushBytes {
				w.flush()
				flushTick.Reset(w.flushInterval)
			}
		case <-flushTick.C:
			// bulk indexer needs to be flushed every flush interval because
			// there may be pending bytes in bulk indexer buffer due to e.g. document level 429
			w.flush()
		}
	}
}

func (w *worker) flush() {
	w.logger.Info("Hello Lalith!! I came to flush now", zap.Int("items ready to flush", w.indexer.Items()))
	ctx := context.Background()
	if w.flushTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), w.flushTimeout)
		defer cancel()
	}
	stat, err := w.indexer.Flush(ctx)
	w.stats.docsIndexed.Add(stat.Indexed)
	if err != nil {
		//w.logger.Error("bulk indexer flush error", zap.Error(err))
		w.logger.Error("bulk indexer flush error")
		w.errorChan <- err
	}
	for _, resp := range stat.FailedDocs {
		w.logger.Error(fmt.Sprintf("Drop docs: failed to index: %#v", resp.Error),
			zap.Int("status", resp.Status))
		w.errorChan <- err
	}
	w.logger.Info("Lalith bulk indexer stat", zap.Int64("added index", stat.Indexed), zap.Int("failed index", len(stat.FailedDocs)))
}
