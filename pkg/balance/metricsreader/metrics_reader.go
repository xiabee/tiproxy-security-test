// Copyright 2024 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package metricsreader

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/pingcap/tiproxy/lib/config"
	"github.com/pingcap/tiproxy/lib/util/errors"
	"github.com/pingcap/tiproxy/lib/util/waitgroup"
	"github.com/pingcap/tiproxy/pkg/manager/infosync"
	pnet "github.com/pingcap/tiproxy/pkg/proxy/net"
	"github.com/pingcap/tiproxy/pkg/util/monotime"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"go.uber.org/zap"
)

const (
	readResultNone = iota
	readResultOK
	readResultFail
)

type PromInfoFetcher interface {
	GetPromInfo(ctx context.Context) (*infosync.PrometheusInfo, error)
}

type MetricsReader interface {
	Start(ctx context.Context)
	AddQueryExpr(queryExpr QueryExpr) uint64
	RemoveQueryExpr(id uint64)
	GetQueryResult(id uint64) QueryResult
	Close()
}

var _ MetricsReader = (*DefaultMetricsReader)(nil)

type DefaultMetricsReader struct {
	sync.Mutex
	queryExprs   map[uint64]QueryExpr
	queryResults map[uint64]QueryResult
	wg           waitgroup.WaitGroup
	promFetcher  PromInfoFetcher
	cancel       context.CancelFunc
	lg           *zap.Logger
	cfg          *config.HealthCheck
	lastID       uint64
	readResult   int
}

func NewDefaultMetricsReader(lg *zap.Logger, promFetcher PromInfoFetcher, cfg *config.HealthCheck) *DefaultMetricsReader {
	return &DefaultMetricsReader{
		lg:           lg,
		promFetcher:  promFetcher,
		cfg:          cfg,
		queryExprs:   make(map[uint64]QueryExpr),
		queryResults: make(map[uint64]QueryResult),
	}
}

func (dmr *DefaultMetricsReader) Start(ctx context.Context) {
	// No PD, using static backends.
	if dmr.promFetcher == nil || reflect.ValueOf(dmr.promFetcher).IsNil() {
		return
	}
	childCtx, cancel := context.WithCancel(ctx)
	dmr.cancel = cancel
	dmr.wg.RunWithRecover(func() {
		ticker := time.NewTicker(dmr.cfg.MetricsInterval)
		defer ticker.Stop()
		for childCtx.Err() == nil {
			if results, err := dmr.readMetrics(childCtx); err != nil {
				// If there are successive errors, only log it once.
				if dmr.readResult != readResultFail {
					dmr.readResult = readResultFail
					dmr.lg.Warn("read metrics failed", zap.Error(err))
				}
			} else {
				if dmr.readResult != readResultOK {
					dmr.readResult = readResultOK
					dmr.lg.Debug("read metrics succeed")
				}
				if len(results) > 0 {
					dmr.Lock()
					dmr.queryResults = results
					dmr.Unlock()
				}
			}
			select {
			case <-ticker.C:
			case <-childCtx.Done():
				return
			}
		}
	}, nil, dmr.lg)
}

// Always refresh the prometheus address just in case it changes.
func (dmr *DefaultMetricsReader) getPromAPI(ctx context.Context) (promv1.API, error) {
	promInfo, err := dmr.promFetcher.GetPromInfo(ctx)
	if promInfo == nil {
		if err == nil {
			err = errors.New("no prometheus info found")
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	// TODO: support TLS and authentication.
	promAddr := fmt.Sprintf("http://%s", net.JoinHostPort(promInfo.IP, strconv.Itoa(promInfo.Port)))
	promClient, err := api.NewClient(api.Config{
		Address: promAddr,
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return promv1.NewAPI(promClient), nil
}

func (dmr *DefaultMetricsReader) readMetrics(ctx context.Context) (map[uint64]QueryResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	promQLAPI, err := dmr.getPromAPI(ctx)
	if err != nil {
		return nil, err
	}

	dmr.Lock()
	copyedMap := make(map[uint64]QueryExpr, len(dmr.queryExprs))
	for id, expr := range dmr.queryExprs {
		copyedMap[id] = expr
	}
	dmr.Unlock()
	results := make(map[uint64]QueryResult, len(copyedMap))
	now := time.Now()
	for id, expr := range copyedMap {
		qr := dmr.queryMetric(ctx, promQLAPI, expr, now)
		// Only update the result when it succeeds.
		if qr.Err == nil {
			qr.UpdateTime = monotime.Now()
			results[id] = qr
		} else {
			dmr.lg.Warn("querying metrics fails", zap.String("expr", expr.PromQL), zap.Error(qr.Err))
		}
	}
	return results, nil
}

func (dmr *DefaultMetricsReader) queryMetric(ctx context.Context, promQLAPI promv1.API, expr QueryExpr, curTime time.Time) QueryResult {
	promRange := expr.PromRange(curTime)
	if !expr.HasLabel {
		return dmr.queryOnce(ctx, promQLAPI, expr.PromQL, promRange)
	}

	// The label key is `job` in TiUP but is `component` in TiOperator. We don't know which, so try them both.
	var qr QueryResult
	for _, label := range [2]string{"job", "component"} {
		promQL := fmt.Sprintf(expr.PromQL, label)
		qr = dmr.queryOnce(ctx, promQLAPI, promQL, promRange)
		if qr.Err != nil {
			continue
		}
		if !qr.Empty() {
			expr.PromQL = promQL
			expr.HasLabel = false
			break
		}
	}
	return qr
}

func (dmr *DefaultMetricsReader) queryOnce(ctx context.Context, promQLAPI promv1.API, promQL string, promRange promv1.Range) QueryResult {
	childCtx, cancel := context.WithTimeout(ctx, dmr.cfg.MetricsTimeout)
	var qr QueryResult
	qr.Err = backoff.Retry(func() error {
		var err error
		if promRange.Start.IsZero() {
			qr.Value, _, err = promQLAPI.Query(childCtx, promQL, time.Time{})
		} else {
			qr.Value, _, err = promQLAPI.QueryRange(childCtx, promQL, promRange)
		}
		if !pnet.IsRetryableError(err) {
			return backoff.Permanent(errors.WithStack(err))
		}
		return errors.WithStack(err)
	}, backoff.WithContext(backoff.NewExponentialBackOff(), childCtx))
	cancel()
	return qr
}

func (dmr *DefaultMetricsReader) AddQueryExpr(queryExpr QueryExpr) uint64 {
	dmr.Lock()
	defer dmr.Unlock()
	dmr.lastID++
	dmr.queryExprs[dmr.lastID] = queryExpr
	return dmr.lastID
}

func (dmr *DefaultMetricsReader) RemoveQueryExpr(id uint64) {
	dmr.Lock()
	defer dmr.Unlock()
	delete(dmr.queryExprs, id)
}

func (dmr *DefaultMetricsReader) GetQueryResult(id uint64) QueryResult {
	dmr.Lock()
	defer dmr.Unlock()
	// Return an empty QueryResult if it's not found.
	return dmr.queryResults[id]
}

func (dmr *DefaultMetricsReader) Close() {
	if dmr.cancel != nil {
		dmr.cancel()
	}
	dmr.wg.Wait()
}
