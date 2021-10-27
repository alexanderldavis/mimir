// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/queryrange/results_cache.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package queryrange

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/grafana/dskit/flagext"
	"github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/timestamp"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/uber/jaeger-client-go"

	apierror "github.com/grafana/mimir/pkg/api/error"
	"github.com/grafana/mimir/pkg/chunk/cache"
	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/tenant"
	util_log "github.com/grafana/mimir/pkg/util/log"
	"github.com/grafana/mimir/pkg/util/spanlogger"
	"github.com/grafana/mimir/pkg/util/validation"
)

var (
	// Value that cacheControlHeader has if the response indicates that the results should not be cached.
	noStoreValue = "no-store"

	// ResultsCacheGenNumberHeaderName holds name of the header we want to set in http response
	ResultsCacheGenNumberHeaderName = "Results-Cache-Gen-Number"
)

type CacheGenNumberLoader interface {
	GetResultsCacheGenNumber(tenantIDs []string) string
}

// ResultsCacheConfig is the config for the results cache.
type ResultsCacheConfig struct {
	CacheConfig cache.Config `yaml:"cache"`
	Compression string       `yaml:"compression"`
}

// RegisterFlags registers flags.
func (cfg *ResultsCacheConfig) RegisterFlags(f *flag.FlagSet) {
	cfg.CacheConfig.RegisterFlagsWithPrefix("frontend.", "", f)

	f.StringVar(&cfg.Compression, "frontend.compression", "", "Use compression in results cache. Supported values are: 'snappy' and '' (disable compression).")
	//lint:ignore faillint Need to pass the global logger like this for warning on deprecated methods
	flagext.DeprecatedFlag(f, "frontend.cache-split-interval", "Deprecated: The maximum interval expected for each request, results will be cached per single interval. This behavior is now determined by querier.split-queries-by-interval.", util_log.Logger)
}

func (cfg *ResultsCacheConfig) Validate() error {
	switch cfg.Compression {
	case "snappy", "":
		// valid
	default:
		return errors.Errorf("unsupported compression type: %s", cfg.Compression)
	}

	return cfg.CacheConfig.Validate()
}

// Extractor is used by the cache to extract a subset of a response from a cache entry.
type Extractor interface {
	// Extract extracts a subset of a response from the `start` and `end` timestamps in milliseconds in the `from` response.
	Extract(start, end int64, from Response) Response
	ResponseWithoutHeaders(resp Response) Response
}

// PrometheusResponseExtractor helps extracting specific info from Query Response.
type PrometheusResponseExtractor struct{}

// Extract extracts response for specific a range from a response.
func (PrometheusResponseExtractor) Extract(start, end int64, from Response) Response {
	promRes := from.(*PrometheusResponse)
	return &PrometheusResponse{
		Status: StatusSuccess,
		Data: PrometheusData{
			ResultType: promRes.Data.ResultType,
			Result:     extractMatrix(start, end, promRes.Data.Result),
		},
		Headers: promRes.Headers,
	}
}

// ResponseWithoutHeaders is useful in caching data without headers since
// we anyways do not need headers for sending back the response so this saves some space by reducing size of the objects.
func (PrometheusResponseExtractor) ResponseWithoutHeaders(resp Response) Response {
	promRes := resp.(*PrometheusResponse)
	return &PrometheusResponse{
		Status: StatusSuccess,
		Data: PrometheusData{
			ResultType: promRes.Data.ResultType,
			Result:     promRes.Data.Result,
		},
	}
}

// CacheSplitter generates cache keys. This is a useful interface for downstream
// consumers who wish to implement their own strategies.
type CacheSplitter interface {
	GenerateCacheKey(userID string, r Request) string
}

// constSplitter is a utility for using a constant split interval when determining cache keys
type constSplitter time.Duration

// GenerateCacheKey generates a cache key based on the userID, Request and interval.
func (t constSplitter) GenerateCacheKey(userID string, r Request) string {
	currentInterval := r.GetStart() / int64(time.Duration(t)/time.Millisecond)
	return fmt.Sprintf("%s:%s:%d:%d", userID, r.GetQuery(), r.GetStep(), currentInterval)
}

// ShouldCacheFn checks whether the current request should go to cache
// or not. If not, just send the request to next handler.
type ShouldCacheFn func(r Request) bool

// resultsCacheAlwaysEnabled is a ShouldCacheFn function always returning true.
var resultsCacheAlwaysEnabled = func(_ Request) bool { return true }

type resultsCache struct {
	logger   log.Logger
	cfg      ResultsCacheConfig
	next     Handler
	cache    cache.Cache
	limits   Limits
	splitter CacheSplitter

	extractor            Extractor
	minCacheExtent       int64 // discard any cache extent smaller than this
	merger               Merger
	cacheGenNumberLoader CacheGenNumberLoader
	shouldCache          ShouldCacheFn
}

// NewResultsCacheMiddleware creates results cache middleware from config.
// The middleware cache result using a unique cache key for a given request (step,query,user) and interval.
// The cache assumes that each request length (end-start) is below or equal the interval.
// Each request starting from within the same interval will hit the same cache entry.
// If the cache doesn't have the entire duration of the request cached, it will query the uncached parts and append them to the cache entries.
// see `generateKey`.
func NewResultsCacheMiddleware(
	logger log.Logger,
	cfg ResultsCacheConfig,
	splitter CacheSplitter,
	limits Limits,
	merger Merger,
	extractor Extractor,
	cacheGenNumberLoader CacheGenNumberLoader,
	shouldCache ShouldCacheFn,
	reg prometheus.Registerer,
) (Middleware, cache.Cache, error) {
	c, err := cache.New(cfg.CacheConfig, reg, logger)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Compression == "snappy" {
		c = cache.NewSnappy(c, logger)
	}

	if cacheGenNumberLoader != nil {
		c = cache.NewCacheGenNumMiddleware(c)
	}

	return MiddlewareFunc(func(next Handler) Handler {
		return &resultsCache{
			logger:               logger,
			cfg:                  cfg,
			next:                 next,
			cache:                c,
			limits:               limits,
			merger:               merger,
			extractor:            extractor,
			minCacheExtent:       defaultMinCacheExtent,
			splitter:             splitter,
			cacheGenNumberLoader: cacheGenNumberLoader,
			shouldCache:          shouldCache,
		}
	}), c, nil
}

func (s resultsCache) Do(ctx context.Context, r Request) (Response, error) {
	tenantIDs, err := tenant.TenantIDs(ctx)
	if err != nil {
		return nil, apierror.New(apierror.TypeBadData, err.Error())
	}

	if s.shouldCache != nil && !s.shouldCache(r) {
		return s.next.Do(ctx, r)
	}

	if s.cacheGenNumberLoader != nil {
		ctx = cache.InjectCacheGenNumber(ctx, s.cacheGenNumberLoader.GetResultsCacheGenNumber(tenantIDs))
	}

	var (
		key      = s.splitter.GenerateCacheKey(tenant.JoinTenantIDs(tenantIDs), r)
		extents  []Extent
		response Response
	)

	maxCacheFreshness := validation.MaxDurationPerTenant(tenantIDs, s.limits.MaxCacheFreshness)
	maxCacheTime := int64(model.Now().Add(-maxCacheFreshness))
	if r.GetStart() > maxCacheTime {
		return s.next.Do(ctx, r)
	}

	cached, ok := s.get(ctx, key)
	if ok {
		response, extents, err = s.handleHit(ctx, r, cached, maxCacheTime)
	} else {
		response, extents, err = s.handleMiss(ctx, r, maxCacheTime)
	}

	if err == nil && len(extents) > 0 {
		extents, err := filterRecentCacheExtents(r, maxCacheFreshness, s.extractor, extents)
		if err != nil {
			return nil, err
		}
		s.put(ctx, key, extents)
	}

	return response, err
}

// isRequestCachable says whether the request is eligible for caching.
func isRequestCachable(req Request, maxCacheTime int64, logger log.Logger) bool {
	// We can run with step alignment disabled because Grafana does it already. Mimir automatically aligning start and end is not
	// PromQL compatible. But this means we cannot cache queries that do not have their start and end aligned.
	if !isRequestStepAligned(req) {
		return false
	}

	// Do not cache it at all if the query time range is more recent than the configured max cache freshness.
	if req.GetStart() > maxCacheTime {
		return false
	}

	if !isAtModifierCachable(req, maxCacheTime, logger) {
		return false
	}

	return true
}

// isResponseCachable says whether the response should be cached or not.
func isResponseCachable(ctx context.Context, r Response, cacheGenNumberLoader CacheGenNumberLoader, logger log.Logger) bool {
	headerValues := getHeaderValuesWithName(r, cacheControlHeader)
	for _, v := range headerValues {
		if v == noStoreValue {
			level.Debug(logger).Log("msg", fmt.Sprintf("%s header in response is equal to %s, not caching the response", cacheControlHeader, noStoreValue))
			return false
		}
	}

	if cacheGenNumberLoader == nil {
		return true
	}

	genNumbersFromResp := getHeaderValuesWithName(r, ResultsCacheGenNumberHeaderName)
	genNumberFromCtx := cache.ExtractCacheGenNumber(ctx)

	if len(genNumbersFromResp) == 0 && genNumberFromCtx != "" {
		level.Debug(logger).Log("msg", fmt.Sprintf("we found results cache gen number %s set in store but none in headers", genNumberFromCtx))
		return false
	}

	for _, gen := range genNumbersFromResp {
		if gen != genNumberFromCtx {
			level.Debug(logger).Log("msg", fmt.Sprintf("inconsistency in results cache gen numbers %s (GEN-FROM-RESPONSE) != %s (GEN-FROM-STORE), not caching the response", gen, genNumberFromCtx))
			return false
		}
	}

	return true
}

var errAtModifierAfterEnd = errors.New("at modifier after end")

// isAtModifierCachable returns true if the @ modifier result
// is safe to cache.
func isAtModifierCachable(r Request, maxCacheTime int64, logger log.Logger) bool {
	// There are 2 cases when @ modifier is not safe to cache:
	//   1. When @ modifier points to time beyond the maxCacheTime.
	//   2. If the @ modifier time is > the query range end while being
	//      below maxCacheTime. In such cases if any tenant is intentionally
	//      playing with old data, we could cache empty result if we look
	//      beyond query end.
	query := r.GetQuery()
	if !strings.Contains(query, "@") {
		return true
	}
	expr, err := parser.ParseExpr(query)
	if err != nil {
		// We are being pessimistic in such cases.
		level.Warn(logger).Log("msg", "failed to parse query, considering @ modifier as not cachable", "query", query, "err", err)
		return false
	}

	// This resolves the start() and end() used with the @ modifier.
	expr = promql.PreprocessExpr(expr, timestamp.Time(r.GetStart()), timestamp.Time(r.GetEnd()))

	end := r.GetEnd()
	atModCachable := true
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		switch e := n.(type) {
		case *parser.VectorSelector:
			if e.Timestamp != nil && (*e.Timestamp > end || *e.Timestamp > maxCacheTime) {
				atModCachable = false
				return errAtModifierAfterEnd
			}
		case *parser.MatrixSelector:
			ts := e.VectorSelector.(*parser.VectorSelector).Timestamp
			if ts != nil && (*ts > end || *ts > maxCacheTime) {
				atModCachable = false
				return errAtModifierAfterEnd
			}
		case *parser.SubqueryExpr:
			if e.Timestamp != nil && (*e.Timestamp > end || *e.Timestamp > maxCacheTime) {
				atModCachable = false
				return errAtModifierAfterEnd
			}
		}
		return nil
	})

	return atModCachable
}

func getHeaderValuesWithName(r Response, headerName string) (headerValues []string) {
	for _, hv := range r.GetHeaders() {
		if hv.GetName() != headerName {
			continue
		}

		headerValues = append(headerValues, hv.GetValues()...)
	}

	return
}

func (s resultsCache) handleMiss(ctx context.Context, r Request, maxCacheTime int64) (Response, []Extent, error) {
	response, err := s.next.Do(ctx, r)
	if err != nil {
		return nil, nil, err
	}

	if !isRequestCachable(r, maxCacheTime, s.logger) || !isResponseCachable(ctx, response, s.cacheGenNumberLoader, s.logger) {
		return response, []Extent{}, nil
	}

	extent, err := toExtent(ctx, r, s.extractor.ResponseWithoutHeaders(response))
	if err != nil {
		return nil, nil, err
	}

	extents := []Extent{
		extent,
	}
	return response, extents, nil
}

func (s resultsCache) handleHit(ctx context.Context, r Request, extents []Extent, maxCacheTime int64) (Response, []Extent, error) {
	var (
		reqResps []RequestResponse
		err      error
	)
	log, ctx := spanlogger.NewWithLogger(ctx, s.logger, "handleHit")
	defer log.Finish()

	requests, responses, err := partitionCacheExtents(r, extents, s.minCacheExtent, s.extractor)
	if err != nil {
		return nil, nil, err
	}
	if len(requests) == 0 {
		response, err := s.merger.MergeResponse(responses...)
		// No downstream requests so no need to write back to the cache.
		return response, nil, err
	}

	reqResps, err = DoRequests(ctx, s.next, requests, s.limits, false)
	if err != nil {
		return nil, nil, err
	}

	for _, reqResp := range reqResps {
		responses = append(responses, reqResp.Response)
		if !isRequestCachable(r, maxCacheTime, s.logger) || !isResponseCachable(ctx, reqResp.Response, s.cacheGenNumberLoader, s.logger) {
			continue
		}
		extent, err := toExtent(ctx, reqResp.Request, s.extractor.ResponseWithoutHeaders(reqResp.Response))
		if err != nil {
			return nil, nil, err
		}
		extents = append(extents, extent)
	}

	mergedExtents, err := mergeCacheExtentsForRequest(ctx, r, s.merger, extents)
	if err != nil {
		return nil, nil, err
	}

	response, err := s.merger.MergeResponse(responses...)
	return response, mergedExtents, err
}

// mergeCacheExtentsForRequest merges the provided cache extents for the input request and returns merged extents.
// The input extents can be overlapping and are not required to be sorted.
func mergeCacheExtentsForRequest(ctx context.Context, r Request, merger Merger, extents []Extent) ([]Extent, error) {
	// Fast path.
	if len(extents) <= 1 {
		return extents, nil
	}

	sort.Slice(extents, func(i, j int) bool {
		if extents[i].Start == extents[j].Start {
			// as an optimization, for two extents starts at the same time, we
			// put bigger extent at the front of the slice, which helps
			// to reduce the amount of merge we have to do later.
			return extents[i].End > extents[j].End
		}

		return extents[i].Start < extents[j].Start
	})

	// Merge any extents - potentially overlapping
	accumulator, err := newAccumulator(extents[0])
	if err != nil {
		return nil, err
	}
	mergedExtents := make([]Extent, 0, len(extents))

	for i := 1; i < len(extents); i++ {
		if accumulator.End+r.GetStep() < extents[i].Start {
			mergedExtents, err = mergeCacheExtentsWithAccumulator(mergedExtents, accumulator)
			if err != nil {
				return nil, err
			}
			accumulator, err = newAccumulator(extents[i])
			if err != nil {
				return nil, err
			}
			continue
		}

		if accumulator.End >= extents[i].End {
			continue
		}

		accumulator.TraceId = jaegerTraceID(ctx)
		accumulator.End = extents[i].End
		currentRes, err := extents[i].toResponse()
		if err != nil {
			return nil, err
		}
		merged, err := merger.MergeResponse(accumulator.Response, currentRes)
		if err != nil {
			return nil, err
		}
		accumulator.Response = merged
	}

	return mergeCacheExtentsWithAccumulator(mergedExtents, accumulator)
}

type accumulator struct {
	Response
	Extent
}

func mergeCacheExtentsWithAccumulator(extents []Extent, acc *accumulator) ([]Extent, error) {
	any, err := types.MarshalAny(acc.Response)
	if err != nil {
		return nil, err
	}
	return append(extents, Extent{
		Start:    acc.Extent.Start,
		End:      acc.Extent.End,
		Response: any,
		TraceId:  acc.Extent.TraceId,
	}), nil
}

func newAccumulator(base Extent) (*accumulator, error) {
	res, err := base.toResponse()
	if err != nil {
		return nil, err
	}
	return &accumulator{
		Response: res,
		Extent:   base,
	}, nil
}

func toExtent(ctx context.Context, req Request, res Response) (Extent, error) {
	any, err := types.MarshalAny(res)
	if err != nil {
		return Extent{}, err
	}
	return Extent{
		Start:    req.GetStart(),
		End:      req.GetEnd(),
		Response: any,
		TraceId:  jaegerTraceID(ctx),
	}, nil
}

// partitionCacheExtents calculates the required requests to satisfy req given the cached data.
// extents must be in order by start time.
func partitionCacheExtents(req Request, extents []Extent, minCacheExtent int64, extractor Extractor) ([]Request, []Response, error) {
	var requests []Request
	var cachedResponses []Response
	start := req.GetStart()

	for _, extent := range extents {
		// If there is no overlap, ignore this extent.
		if extent.GetEnd() < start || extent.Start > req.GetEnd() {
			continue
		}

		// If this extent is tiny and request is not tiny, discard it: more efficient to do a few larger queries.
		// Hopefully tiny request can make tiny extent into not-so-tiny extent.

		// However if the step is large enough, the split_query_by_interval middleware would generate a query with same start and end.
		// For example, if the step size is more than 12h and the interval is 24h.
		// This means the extent's start and end time would be same, even if the timerange covers several hours.
		if (req.GetStart() != req.GetEnd()) && (req.GetEnd()-req.GetStart() > minCacheExtent) && (extent.End-extent.Start < minCacheExtent) {
			continue
		}

		// If there is a bit missing at the front, make a request for that.
		if start < extent.Start {
			r := req.WithStartEnd(start, extent.Start)
			requests = append(requests, r)
		}
		res, err := extent.toResponse()
		if err != nil {
			return nil, nil, err
		}
		// extract the overlap from the cached extent.
		cachedResponses = append(cachedResponses, extractor.Extract(start, req.GetEnd(), res))
		start = extent.End
	}

	// Lastly, make a request for any data missing at the end.
	if start < req.GetEnd() {
		r := req.WithStartEnd(start, req.GetEnd())
		requests = append(requests, r)
	}

	// If start and end are the same (valid in promql), start == req.GetEnd() and we won't do the query.
	// But we should only do the request if we don't have a valid cached response for it.
	if req.GetStart() == req.GetEnd() && len(cachedResponses) == 0 {
		requests = append(requests, req)
	}

	return requests, cachedResponses, nil
}

func filterRecentCacheExtents(req Request, maxCacheFreshness time.Duration, extractor Extractor, extents []Extent) ([]Extent, error) {
	maxCacheTime := (int64(model.Now().Add(-maxCacheFreshness)) / req.GetStep()) * req.GetStep()
	for i := range extents {
		// Never cache data for the latest freshness period.
		if extents[i].End > maxCacheTime {
			extents[i].End = maxCacheTime
			res, err := extents[i].toResponse()
			if err != nil {
				return nil, err
			}
			extracted := extractor.Extract(extents[i].Start, maxCacheTime, res)
			any, err := types.MarshalAny(extracted)
			if err != nil {
				return nil, err
			}
			extents[i].Response = any
		}
	}
	return extents, nil
}

func (s resultsCache) get(ctx context.Context, key string) ([]Extent, bool) {
	found, bufs, _ := s.cache.Fetch(ctx, []string{cache.HashKey(key)})
	if len(found) != 1 {
		return nil, false
	}

	var resp CachedResponse
	log, ctx := spanlogger.NewWithLogger(ctx, s.logger, "unmarshal-extent") //nolint:ineffassign,staticcheck
	defer log.Finish()

	log.LogFields(otlog.Int("bytes", len(bufs[0])))

	if err := proto.Unmarshal(bufs[0], &resp); err != nil {
		level.Error(log).Log("msg", "error unmarshalling cached value", "err", err)
		log.Error(err)
		return nil, false
	}

	if resp.Key != key {
		return nil, false
	}

	// Refreshes the cache if it contains an old proto schema.
	for _, e := range resp.Extents {
		if e.Response == nil {
			return nil, false
		}
	}

	return resp.Extents, true
}

func (s resultsCache) put(ctx context.Context, key string, extents []Extent) {
	buf, err := proto.Marshal(&CachedResponse{
		Key:     key,
		Extents: extents,
	})
	if err != nil {
		level.Error(s.logger).Log("msg", "error marshalling cached value", "err", err)
		return
	}

	s.cache.Store(ctx, []string{cache.HashKey(key)}, [][]byte{buf})
}

func jaegerTraceID(ctx context.Context) string {
	span := opentracing.SpanFromContext(ctx)
	if span == nil {
		return ""
	}

	spanContext, ok := span.Context().(jaeger.SpanContext)
	if !ok {
		return ""
	}

	return spanContext.TraceID().String()
}

func extractMatrix(start, end int64, matrix []SampleStream) []SampleStream {
	result := make([]SampleStream, 0, len(matrix))
	for _, stream := range matrix {
		extracted, ok := extractSampleStream(start, end, stream)
		if ok {
			result = append(result, extracted)
		}
	}
	return result
}

func extractSampleStream(start, end int64, stream SampleStream) (SampleStream, bool) {
	result := SampleStream{
		Labels:  stream.Labels,
		Samples: make([]mimirpb.Sample, 0, len(stream.Samples)),
	}
	for _, sample := range stream.Samples {
		if start <= sample.TimestampMs && sample.TimestampMs <= end {
			result.Samples = append(result.Samples, sample)
		}
	}
	if len(result.Samples) == 0 {
		return SampleStream{}, false
	}
	return result, true
}

func (e *Extent) toResponse() (Response, error) {
	msg, err := types.EmptyAny(e.Response)
	if err != nil {
		return nil, err
	}

	if err := types.UnmarshalAny(e.Response, msg); err != nil {
		return nil, err
	}

	resp, ok := msg.(Response)
	if !ok {
		return nil, fmt.Errorf("bad cached type")
	}
	return resp, nil
}
