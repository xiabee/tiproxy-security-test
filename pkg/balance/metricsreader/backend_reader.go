// Copyright 2024 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package metricsreader

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/pingcap/tiproxy/lib/config"
	"github.com/pingcap/tiproxy/lib/util/waitgroup"
	"github.com/pingcap/tiproxy/pkg/manager/elect"
	"github.com/pingcap/tiproxy/pkg/util/httputil"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/siddontang/go/hack"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

const (
	// backendReaderKey is the key in etcd for VIP election.
	backendReaderKey = "/tiproxy/metricreader/owner"
	// sessionTTL is the session's TTL in seconds for backend reader owner election.
	sessionTTL = 30
	// backendMetricPath is the path of backend HTTP API to read metrics.
	backendMetricPath = "/metrics"
	// ownerMetricPath is the path of reading backend metrics from the backend reader owner.
	ownerMetricPath = "/api/backend/metrics"
	goPoolSize      = 100
	goMaxIdle       = time.Minute
)

type backendHistory struct {
	Step1History []model.SamplePair
	Step2History []model.SamplePair
}

type BackendReader struct {
	sync.Mutex
	queryRules   map[string]QueryRule
	queryResults map[string]QueryResult
	// the owner generates the history from querying backends and other members query the history from the owner
	// rule key: {backend name: backendHistory}
	history map[string]map[string]backendHistory
	// the owner marshalles history to share it to other members
	// cache the marshalled history to avoid duplicated marshalling
	marshalledHistory []byte
	election          elect.Election
	cfgGetter         config.ConfigGetter
	backendFetcher    TopologyFetcher
	httpCli           *http.Client
	lg                *zap.Logger
	cfg               *config.HealthCheck
	wgp               *waitgroup.WaitGroupPool
	isOwner           atomic.Bool
}

func NewBackendReader(lg *zap.Logger, cfgGetter config.ConfigGetter, httpCli *http.Client, backendFetcher TopologyFetcher,
	cfg *config.HealthCheck) *BackendReader {
	return &BackendReader{
		queryRules:     make(map[string]QueryRule),
		queryResults:   make(map[string]QueryResult),
		history:        make(map[string]map[string]backendHistory),
		lg:             lg,
		cfgGetter:      cfgGetter,
		backendFetcher: backendFetcher,
		cfg:            cfg,
		wgp:            waitgroup.NewWaitGroupPool(goPoolSize, goMaxIdle),
		httpCli:        httpCli,
	}
}

func (br *BackendReader) Start(ctx context.Context, etcdCli *clientv3.Client) error {
	cfg := br.cfgGetter.GetConfig()
	ip, _, statusPort, err := cfg.GetIPPort()
	if err != nil {
		return err
	}

	// Use the status address as the key so that it can read metrics from the address.
	id := net.JoinHostPort(ip, statusPort)
	electionCfg := elect.DefaultElectionConfig(sessionTTL)
	election := elect.NewElection(br.lg, etcdCli, electionCfg, id, backendReaderKey, br)
	br.election = election
	election.Start(ctx)
	return nil
}

func (br *BackendReader) OnElected() {
	br.isOwner.Store(true)
}

func (br *BackendReader) OnRetired() {
	br.isOwner.Store(false)
}

func (br *BackendReader) AddQueryRule(key string, rule QueryRule) {
	br.Lock()
	defer br.Unlock()
	br.queryRules[key] = rule
}

func (br *BackendReader) RemoveQueryRule(key string) {
	br.Lock()
	defer br.Unlock()
	delete(br.queryRules, key)
}

func (br *BackendReader) GetQueryResult(key string) QueryResult {
	br.Lock()
	defer br.Unlock()
	// Return an empty QueryResult if it's not found.
	return br.queryResults[key]
}

func (br *BackendReader) ReadMetrics(ctx context.Context) (map[string]QueryResult, error) {
	if br.isOwner.Load() {
		if err := br.readFromBackends(ctx); err != nil {
			return nil, err
		}
	} else {
		owner, err := br.election.GetOwnerID(ctx)
		if err != nil {
			br.lg.Error("get owner failed, won't read metrics", zap.Error(err))
			return nil, err
		}
		if err = br.readFromOwner(ctx, owner); err != nil {
			return nil, err
		}
	}
	return br.queryResults, nil
}

func (br *BackendReader) readFromBackends(ctx context.Context) error {
	addrs, err := br.getBackendAddrs(ctx)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return nil
	}
	allNames := br.collectAllNames()
	if len(allNames) == 0 {
		return nil
	}

	for _, addr := range addrs {
		func(addr string) {
			br.wgp.RunWithRecover(func() {
				if ctx.Err() != nil {
					return
				}
				resp, err := br.readBackendMetric(ctx, addr)
				if err != nil {
					br.lg.Error("read metrics from backend failed", zap.String("addr", addr), zap.Error(err))
					return
				}
				text := filterMetrics(hack.String(resp), allNames)
				mf, err := parseMetrics(text)
				if err != nil {
					br.lg.Error("parse metrics failed", zap.String("addr", addr), zap.Error(err))
					return
				}
				result := br.groupMetricsByRule(mf, addr)
				br.mergeQueryResult(result, addr)
			}, nil, br.lg)
		}(addr)
	}
	br.wgp.Wait()

	br.purgeHistory()
	br.Lock()
	br.marshalledHistory = nil
	br.Unlock()
	return nil
}

func (br *BackendReader) collectAllNames() []string {
	br.Lock()
	defer br.Unlock()
	names := make([]string, 0, len(br.queryRules)*3)
	for _, rule := range br.queryRules {
		for _, name := range rule.Names {
			if slices.Index(names, name) < 0 {
				names = append(names, name)
			}
		}
	}
	return names
}

func (br *BackendReader) readBackendMetric(ctx context.Context, addr string) ([]byte, error) {
	httpCli := *br.httpCli
	httpCli.Timeout = br.cfg.DialTimeout
	b := backoff.WithContext(backoff.WithMaxRetries(backoff.NewConstantBackOff(br.cfg.RetryInterval), uint64(br.cfg.MaxRetries)), ctx)
	return httputil.Get(httpCli, addr, backendMetricPath, b)
}

// groupMetricsByRule gets the result for each rule of one backend.
func (br *BackendReader) groupMetricsByRule(mfs map[string]*dto.MetricFamily, backend string) map[string]model.Value {
	now := model.TimeFromUnixNano(time.Now().UnixNano())
	br.Lock()
	defer br.Unlock()
	// rule key: backend value
	results := make(map[string]model.Value, len(br.queryRules))
	for ruleKey, rule := range br.queryRules {
		// If the metric doesn't exist, skip it.
		metricExists := true
		for _, name := range rule.Names {
			if _, ok := mfs[name]; !ok {
				metricExists = false
				break
			}
		}
		if !metricExists {
			continue
		}

		// step 1: get the latest pair (at a timepoint) and add it to step1History
		// E.g. calculate process_cpu_seconds_total/tidb_server_maxprocs
		sampleValue := rule.Metric2Value(mfs)
		if math.IsNaN(float64(sampleValue)) {
			continue
		}
		pair := model.SamplePair{Timestamp: now, Value: sampleValue}
		ruleHistory, ok := br.history[ruleKey]
		if !ok {
			ruleHistory = make(map[string]backendHistory)
			br.history[ruleKey] = ruleHistory
		}
		beHistory := ruleHistory[backend]
		beHistory.Step1History = append(beHistory.Step1History, pair)

		// step 2: get the latest pair by the history and add it to step2History
		// E.g. calculate irate(process_cpu_seconds_total/tidb_server_maxprocs[30s])
		sampleValue = rule.Range2Value(beHistory.Step1History)
		if math.IsNaN(float64(sampleValue)) {
			continue
		}
		beHistory.Step2History = append(beHistory.Step2History, model.SamplePair{Timestamp: now, Value: sampleValue})
		ruleHistory[backend] = beHistory

		// step 3: return the result
		// E.g. return the metrics for 1 minute as a matrix
		labels := map[model.LabelName]model.LabelValue{LabelNameInstance: model.LabelValue(backend)}
		switch rule.ResultType {
		case model.ValVector:
			// vector indicates returning the latest pair
			results[ruleKey] = model.Vector{{Value: sampleValue, Timestamp: now, Metric: labels}}
		case model.ValMatrix:
			// matrix indicates returning the history
			// copy a slice to avoid data race
			pairs := make([]model.SamplePair, len(beHistory.Step2History))
			copy(pairs, beHistory.Step2History)
			results[ruleKey] = model.Matrix{{Values: pairs, Metric: labels}}
		default:
			br.lg.Error("unsupported value type", zap.String("value type", rule.ResultType.String()))
		}
	}
	return results
}

// mergeQueryResult merges the result of one backend into the final result.
func (br *BackendReader) mergeQueryResult(backendValues map[string]model.Value, backend string) {
	br.Lock()
	defer br.Unlock()
	for ruleKey, value := range backendValues {
		result := br.queryResults[ruleKey]
		if result.Value == nil || reflect.ValueOf(result.Value).IsNil() {
			result.Value = value
			br.queryResults[ruleKey] = result
			continue
		}
		switch result.Value.Type() {
		case model.ValVector:
			idx := -1
			for i, v := range result.Value.(model.Vector) {
				if v.Metric[LabelNameInstance] == model.LabelValue(backend) {
					idx = i
					break
				}
			}
			if idx >= 0 {
				result.Value.(model.Vector)[idx] = value.(model.Vector)[0]
			} else {
				result.Value = append(result.Value.(model.Vector), value.(model.Vector)[0])
			}
		case model.ValMatrix:
			idx := -1
			for i, v := range result.Value.(model.Matrix) {
				if v.Metric[LabelNameInstance] == model.LabelValue(backend) {
					idx = i
					break
				}
			}
			if idx >= 0 {
				result.Value.(model.Matrix)[idx] = value.(model.Matrix)[0]
			} else {
				result.Value = append(result.Value.(model.Matrix), value.(model.Matrix)[0])
			}
		default:
			br.lg.Error("unsupported value type", zap.Stringer("value type", result.Value.Type()))
		}
		br.queryResults[ruleKey] = result
	}
}

// purgeHistory purges the expired or useless history values, otherwise the memory grows infinitely.
func (br *BackendReader) purgeHistory() {
	now := time.Now()
	br.Lock()
	defer br.Unlock()
	for id, ruleHistory := range br.history {
		rule, ok := br.queryRules[id]
		// the rule is removed
		if !ok {
			delete(br.history, id)
			continue
		}
		for backend, backendHistory := range ruleHistory {
			backendHistory.Step1History = purgeHistory(backendHistory.Step1History, rule.Retention, now)
			backendHistory.Step2History = purgeHistory(backendHistory.Step2History, rule.Retention, now)
			// the history is expired, maybe the backend is down
			if len(backendHistory.Step1History) == 0 && len(backendHistory.Step2History) == 0 {
				delete(ruleHistory, backend)
			} else {
				ruleHistory[backend] = backendHistory
			}
		}
	}
}

func (br *BackendReader) GetBackendMetrics() ([]byte, error) {
	br.Lock()
	defer br.Unlock()
	if br.marshalledHistory != nil {
		return br.marshalledHistory, nil
	}
	marshalled, err := json.Marshal(br.history)
	if err != nil {
		return nil, err
	}
	br.marshalledHistory = marshalled
	return marshalled, nil
}

// readFromOwner queries metric history from the owner.
// If every member queries directly from backends, the backends may suffer from too much pressure.
func (br *BackendReader) readFromOwner(ctx context.Context, addr string) error {
	httpCli := *br.httpCli
	httpCli.Timeout = br.cfg.DialTimeout
	b := backoff.WithContext(backoff.WithMaxRetries(backoff.NewConstantBackOff(br.cfg.RetryInterval), uint64(br.cfg.MaxRetries)), ctx)
	resp, err := httputil.Get(*br.httpCli, addr, ownerMetricPath, b)
	if err != nil {
		return err
	}

	var newHistory map[string]map[string]backendHistory
	if err := json.Unmarshal(resp, &newHistory); err != nil {
		return err
	}
	// If this instance becomes the owner in the next round, it can reuse the history.
	br.Lock()
	br.history = newHistory
	br.marshalledHistory = nil
	br.Unlock()
	return nil
}

func (br *BackendReader) getBackendAddrs(ctx context.Context) ([]string, error) {
	backends, err := br.backendFetcher.GetTiDBTopology(ctx)
	if err != nil {
		br.lg.Error("failed to get backend addresses, stop reading metrics", zap.Error(err))
		return nil, err
	}
	addrs := make([]string, 0, len(backends))
	for _, backend := range backends {
		addrs = append(addrs, net.JoinHostPort(backend.IP, strconv.Itoa(int(backend.StatusPort))))
	}
	return addrs, nil
}

func (br *BackendReader) Close() {
	if br.election != nil {
		br.election.Close()
	}
}

func purgeHistory(history []model.SamplePair, retention time.Duration, now time.Time) []model.SamplePair {
	idx := -1
	for i := range history {
		if time.UnixMilli(int64(history[i].Timestamp)).Add(retention).After(now) {
			idx = i
			break
		}
	}
	if idx > 0 {
		copy(history[:], history[idx:])
		return history[:len(history)-idx]
	} else if idx < 0 {
		history = history[:0]
	}
	return history
}

// filterMetrics filters the necessary metrics so that it's faster to parse.
func filterMetrics(all string, names []string) string {
	var buffer strings.Builder
	buffer.Grow(4096)
	for {
		idx := strings.Index(all, "\n")
		var line string
		if idx < 0 {
			line = all
		} else {
			line = all[:idx+1]
			all = all[idx+1:]
		}
		for i := range names {
			// strings.Contains() includes the metric type in the result but it's slower.
			// Note that the result is always in `Metric.Untyped` because the metric type is ignored.
			if strings.HasPrefix(line, names[i]) {
				buffer.WriteString(line)
				break
			}
		}
		if idx < 0 {
			break
		}
	}
	return buffer.String()
}

func parseMetrics(text string) (map[string]*dto.MetricFamily, error) {
	var parser expfmt.TextParser
	return parser.TextToMetricFamilies(strings.NewReader(text))
}
