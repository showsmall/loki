package frontend

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/httpgrpc/server"
	"github.com/weaveworks/common/user"
	"go.uber.org/atomic"

	"github.com/cortexproject/cortex/pkg/util"
)

const (
	// StatusClientClosedRequest is the status code for when a client request cancellation of an http request
	StatusClientClosedRequest = 499
)

var (
	errTooManyRequest   = httpgrpc.Errorf(http.StatusTooManyRequests, "too many outstanding requests")
	errCanceled         = httpgrpc.Errorf(StatusClientClosedRequest, context.Canceled.Error())
	errDeadlineExceeded = httpgrpc.Errorf(http.StatusGatewayTimeout, context.DeadlineExceeded.Error())
)

// Config for a Frontend.
type Config struct {
	MaxOutstandingPerTenant int           `yaml:"max_outstanding_per_tenant"`
	CompressResponses       bool          `yaml:"compress_responses"`
	DownstreamURL           string        `yaml:"downstream_url"`
	LogQueriesLongerThan    time.Duration `yaml:"log_queries_longer_than"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	f.IntVar(&cfg.MaxOutstandingPerTenant, "querier.max-outstanding-requests-per-tenant", 100, "Maximum number of outstanding requests per tenant per frontend; requests beyond this error with HTTP 429.")
	f.BoolVar(&cfg.CompressResponses, "querier.compress-http-responses", false, "Compress HTTP responses.")
	f.StringVar(&cfg.DownstreamURL, "frontend.downstream-url", "", "URL of downstream Prometheus.")
	f.DurationVar(&cfg.LogQueriesLongerThan, "frontend.log-queries-longer-than", 0, "Log queries that are slower than the specified duration. Set to 0 to disable. Set to < 0 to enable on all queries.")
}

type Limits interface {
	// Returns max queriers to use per tenant, or 0 if shuffle sharding is disabled.
	MaxQueriersPerUser(user string) int
}

// Frontend queues HTTP requests, dispatches them to backends, and handles retries
// for requests which failed.
type Frontend struct {
	cfg          Config
	log          log.Logger
	roundTripper http.RoundTripper
	limits       Limits

	mtx    sync.Mutex
	cond   *sync.Cond // Notified when request is enqueued or dequeued, or querier is disconnected.
	queues *queues

	connectedClients *atomic.Int32

	// Metrics.
	numClients    prometheus.GaugeFunc
	queueDuration prometheus.Histogram
	queueLength   *prometheus.GaugeVec
}

type request struct {
	enqueueTime time.Time
	queueSpan   opentracing.Span
	originalCtx context.Context

	request  *httpgrpc.HTTPRequest
	err      chan error
	response chan *httpgrpc.HTTPResponse
}

// New creates a new frontend.
func New(cfg Config, limits Limits, log log.Logger, registerer prometheus.Registerer) (*Frontend, error) {
	connectedClients := atomic.NewInt32(0)
	f := &Frontend{
		cfg:    cfg,
		log:    log,
		limits: limits,
		queues: newUserQueues(cfg.MaxOutstandingPerTenant),
		queueDuration: promauto.With(registerer).NewHistogram(prometheus.HistogramOpts{
			Namespace: "cortex",
			Name:      "query_frontend_queue_duration_seconds",
			Help:      "Time spend by requests queued.",
			Buckets:   prometheus.DefBuckets,
		}),
		queueLength: promauto.With(registerer).NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "cortex",
			Name:      "query_frontend_queue_length",
			Help:      "Number of queries in the queue.",
		}, []string{"user"}),
		numClients: promauto.With(registerer).NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "cortex",
			Name:      "query_frontend_connected_clients",
			Help:      "Number of worker clients currently connected to the frontend.",
		}, func() float64 { return float64(connectedClients.Load()) }),
		connectedClients: connectedClients,
	}
	f.cond = sync.NewCond(&f.mtx)

	// The front end implements http.RoundTripper using a GRPC worker queue by default.
	f.roundTripper = f
	// However if the user has specified a downstream Prometheus, then we should use that.
	if cfg.DownstreamURL != "" {
		u, err := url.Parse(cfg.DownstreamURL)
		if err != nil {
			return nil, err
		}

		f.roundTripper = RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			tracer, span := opentracing.GlobalTracer(), opentracing.SpanFromContext(r.Context())
			if tracer != nil && span != nil {
				carrier := opentracing.HTTPHeadersCarrier(r.Header)
				tracer.Inject(span.Context(), opentracing.HTTPHeaders, carrier)
			}
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.URL.Path = path.Join(u.Path, r.URL.Path)
			r.Host = ""
			return http.DefaultTransport.RoundTrip(r)
		})
	}

	return f, nil
}

// Wrap uses a Tripperware to chain a new RoundTripper to the frontend.
func (f *Frontend) Wrap(trw Tripperware) {
	f.roundTripper = trw(f.roundTripper)
}

// Tripperware is a signature for all http client-side middleware.
type Tripperware func(http.RoundTripper) http.RoundTripper

// RoundTripFunc is to http.RoundTripper what http.HandlerFunc is to http.Handler.
type RoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// Close stops new requests and errors out any pending requests.
func (f *Frontend) Close() {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	for f.queues.len() > 0 {
		f.cond.Wait()
	}
}

// Handler for HTTP requests.
func (f *Frontend) Handler() http.Handler {
	if f.cfg.CompressResponses {
		return gziphandler.GzipHandler(http.HandlerFunc(f.handle))
	}
	return http.HandlerFunc(f.handle)
}

func (f *Frontend) handle(w http.ResponseWriter, r *http.Request) {

	startTime := time.Now()
	resp, err := f.roundTripper.RoundTrip(r)
	queryResponseTime := time.Since(startTime)

	if err != nil {
		writeError(w, err)
	} else {
		hs := w.Header()
		for h, vs := range resp.Header {
			hs[h] = vs
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}

	// If LogQueriesLongerThan is set to <0 we log every query, if it is set to 0 query logging
	// is disabled
	if f.cfg.LogQueriesLongerThan != 0 && queryResponseTime > f.cfg.LogQueriesLongerThan {
		logMessage := []interface{}{
			"msg", "slow query detected",
			"method", r.Method,
			"host", r.Host,
			"path", r.URL.Path,
			"time_taken", queryResponseTime.String(),
		}

		// Ensure the form has been parsed so all the parameters are present
		err = r.ParseForm()
		if err != nil {
			level.Warn(util.WithContext(r.Context(), f.log)).Log("msg", "unable to parse form for request", "err", err)
		}

		// Attempt to iterate through the Form to log any filled in values
		for k, v := range r.Form {
			logMessage = append(logMessage, fmt.Sprintf("param_%s", k), strings.Join(v, ","))
		}

		level.Info(util.WithContext(r.Context(), f.log)).Log(logMessage...)
	}
}

func writeError(w http.ResponseWriter, err error) {
	switch err {
	case context.Canceled:
		err = errCanceled
	case context.DeadlineExceeded:
		err = errDeadlineExceeded
	default:
	}
	server.WriteError(w, err)
}

// RoundTrip implement http.Transport.
func (f *Frontend) RoundTrip(r *http.Request) (*http.Response, error) {
	req, err := server.HTTPRequest(r)
	if err != nil {
		return nil, err
	}

	resp, err := f.RoundTripGRPC(r.Context(), req)
	if err != nil {
		return nil, err
	}

	httpResp := &http.Response{
		StatusCode: int(resp.Code),
		Body:       ioutil.NopCloser(bytes.NewReader(resp.Body)),
		Header:     http.Header{},
	}
	for _, h := range resp.Headers {
		httpResp.Header[h.Key] = h.Values
	}
	return httpResp, nil
}

type httpgrpcHeadersCarrier httpgrpc.HTTPRequest

func (c *httpgrpcHeadersCarrier) Set(key, val string) {
	c.Headers = append(c.Headers, &httpgrpc.Header{
		Key:    key,
		Values: []string{val},
	})
}

// RoundTripGRPC round trips a proto (instead of a HTTP request).
func (f *Frontend) RoundTripGRPC(ctx context.Context, req *httpgrpc.HTTPRequest) (*httpgrpc.HTTPResponse, error) {
	// Propagate trace context in gRPC too - this will be ignored if using HTTP.
	tracer, span := opentracing.GlobalTracer(), opentracing.SpanFromContext(ctx)
	if tracer != nil && span != nil {
		carrier := (*httpgrpcHeadersCarrier)(req)
		tracer.Inject(span.Context(), opentracing.HTTPHeaders, carrier)
	}

	request := request{
		request:     req,
		originalCtx: ctx,

		// Buffer of 1 to ensure response can be written by the server side
		// of the Process stream, even if this goroutine goes away due to
		// client context cancellation.
		err:      make(chan error, 1),
		response: make(chan *httpgrpc.HTTPResponse, 1),
	}

	if err := f.queueRequest(ctx, &request); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()

	case resp := <-request.response:
		return resp, nil

	case err := <-request.err:
		return nil, err
	}
}

// Process allows backends to pull requests from the frontend.
func (f *Frontend) Process(server Frontend_ProcessServer) error {
	querierID, err := getQuerierID(server)
	if err != nil {
		return err
	}

	f.registerQuerierConnection(querierID)
	defer f.unregisterQuerierConnection(querierID)

	// If the downstream request(from querier -> frontend) is cancelled,
	// we need to ping the condition variable to unblock getNextRequestForQuerier.
	// Ideally we'd have ctx aware condition variables...
	go func() {
		<-server.Context().Done()
		f.cond.Broadcast()
	}()

	lastUserIndex := -1

	for {
		req, idx, err := f.getNextRequestForQuerier(server.Context(), lastUserIndex, querierID)
		if err != nil {
			return err
		}
		lastUserIndex = idx

		// Handle the stream sending & receiving on a goroutine so we can
		// monitoring the contexts in a select and cancel things appropriately.
		resps := make(chan *httpgrpc.HTTPResponse, 1)
		errs := make(chan error, 1)
		go func() {
			err = server.Send(&FrontendToClient{
				Type:        HTTP_REQUEST,
				HttpRequest: req.request,
			})
			if err != nil {
				errs <- err
				return
			}

			resp, err := server.Recv()
			if err != nil {
				errs <- err
				return
			}

			resps <- resp.HttpResponse
		}()

		select {
		// If the upstream request is cancelled, we need to cancel the
		// downstream req.  Only way we can do that is to close the stream.
		// The worker client is expecting this semantics.
		case <-req.originalCtx.Done():
			return req.originalCtx.Err()

		// Is there was an error handling this request due to network IO,
		// then error out this upstream request _and_ stream.
		case err := <-errs:
			req.err <- err
			return err

		// Happy path: propagate the response.
		case resp := <-resps:
			req.response <- resp
		}
	}
}

func getQuerierID(server Frontend_ProcessServer) (string, error) {
	err := server.Send(&FrontendToClient{
		Type: GET_ID,
		// Old queriers don't support GET_ID, and will try to use the request.
		// To avoid confusing them, include dummy request.
		HttpRequest: &httpgrpc.HTTPRequest{
			Method: "GET",
			Url:    "/invalid_request_sent_by_frontend",
		},
	})

	if err != nil {
		return "", err
	}

	resp, err := server.Recv()

	// Old queriers will return empty string, which is fine. All old queriers will be
	// treated as single querier with lot of connections.
	// (Note: if resp is nil, GetClientID() returns "")
	return resp.GetClientID(), err
}

func (f *Frontend) queueRequest(ctx context.Context, req *request) error {
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return err
	}

	req.enqueueTime = time.Now()
	req.queueSpan, _ = opentracing.StartSpanFromContext(ctx, "queued")

	maxQueriers := f.limits.MaxQueriersPerUser(userID)

	f.mtx.Lock()
	defer f.mtx.Unlock()

	queue := f.queues.getOrAddQueue(userID, maxQueriers)
	if queue == nil {
		// This can only happen if userID is "".
		return errors.New("no queue found")
	}

	select {
	case queue <- req:
		f.queueLength.WithLabelValues(userID).Inc()
		f.cond.Broadcast()
		return nil
	default:
		return errTooManyRequest
	}
}

// getQueue picks a random queue and takes the next unexpired request off of it, so we
// fairly process users queries.  Will block if there are no requests.
func (f *Frontend) getNextRequestForQuerier(ctx context.Context, lastUserIndex int, querierID string) (*request, int, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	querierWait := false

FindQueue:
	// We need to wait if there are no users, or no pending requests for given querier.
	for (f.queues.len() == 0 || querierWait) && ctx.Err() == nil {
		querierWait = false
		f.cond.Wait()
	}

	if err := ctx.Err(); err != nil {
		return nil, lastUserIndex, err
	}

	for {
		queue, userID, idx := f.queues.getNextQueueForQuerier(lastUserIndex, querierID)
		lastUserIndex = idx
		if queue == nil {
			break
		}
		/*
		  We want to dequeue the next unexpired request from the chosen tenant queue.
		  The chance of choosing a particular tenant for dequeueing is (1/active_tenants).
		  This is problematic under load, especially with other middleware enabled such as
		  querier.split-by-interval, where one request may fan out into many.
		  If expired requests aren't exhausted before checking another tenant, it would take
		  n_active_tenants * n_expired_requests_at_front_of_queue requests being processed
		  before an active request was handled for the tenant in question.
		  If this tenant meanwhile continued to queue requests,
		  it's possible that it's own queue would perpetually contain only expired requests.
		*/

		// Pick the first non-expired request from this user's queue (if any).
		for {
			lastRequest := false
			request := <-queue
			if len(queue) == 0 {
				f.queues.deleteQueue(userID)
				lastRequest = true
			}

			// Tell close() we've processed a request.
			f.cond.Broadcast()

			f.queueDuration.Observe(time.Since(request.enqueueTime).Seconds())
			f.queueLength.WithLabelValues(userID).Dec()
			request.queueSpan.Finish()

			// Ensure the request has not already expired.
			if request.originalCtx.Err() == nil {
				return request, lastUserIndex, nil
			}

			// Stop iterating on this queue if we've just consumed the last request.
			if lastRequest {
				break
			}
		}
	}

	// There are no unexpired requests, so we can get back
	// and wait for more requests.
	querierWait = true
	goto FindQueue
}

// CheckReady determines if the query frontend is ready.  Function parameters/return
// chosen to match the same method in the ingester
func (f *Frontend) CheckReady(_ context.Context) error {
	// if the downstream url is configured the query frontend is not aware of the state
	//  of the queriers and is therefore always ready
	if f.cfg.DownstreamURL != "" {
		return nil
	}

	// if we have more than one querier connected we will consider ourselves ready
	connectedClients := f.connectedClients.Load()
	if connectedClients > 0 {
		return nil
	}

	msg := fmt.Sprintf("not ready: number of queriers connected to query-frontend is %d", connectedClients)
	level.Info(f.log).Log("msg", msg)
	return errors.New(msg)
}

func (f *Frontend) registerQuerierConnection(querier string) {
	f.connectedClients.Inc()

	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.queues.addQuerierConnection(querier)
}

func (f *Frontend) unregisterQuerierConnection(querier string) {
	f.connectedClients.Dec()

	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.queues.removeQuerierConnection(querier)
}
